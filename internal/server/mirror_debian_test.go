package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
)

// debPackage is one stanza of a Packages index, with the pool file it names.
type debPackage struct {
	name, version, arch, filename, body string
}

// debUpstream serves Debian archives: the main one at its root and named
// ones under /{name}. It counts requests by path.
type debUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string]string
	hits  map[string]int
	// fail answers the paths it holds with that status.
	fail map[string]int
	// architectures, by archive root, or by root and suite as
	// "{root}#{suite}", are the architectures its Releases list, the first
	// of which holds Architecture: all packages. An archive not in it lists
	// none and holds them in binary-amd64.
	architectures map[string][]string
	// components, keyed as architectures, is the component its packages
	// are in, which its Releases list. An archive not in it keeps them in
	// main and lists no components.
	components map[string]string
}

func newDebUpstream(t *testing.T) *debUpstream {
	t.Helper()
	u := &debUpstream{files: map[string]string{}, hits: map[string]int{}, fail: map[string]int{}, architectures: map[string][]string{}, components: map[string]string{}}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		body, ok := u.files[r.URL.Path]
		u.hits[r.URL.Path]++
		status := u.fail[r.URL.Path]
		u.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// addSuite serves suite of the archive under root, with codename in its
// Release and pkgs in component main for each package's architecture.
func (u *debUpstream) addSuite(root, suite, codename string, pkgs ...debPackage) {
	archs, ok := u.architectures[root+"#"+suite]
	if !ok {
		archs = u.architectures[root]
	}
	comp, ok := u.components[root+"#"+suite]
	if !ok {
		comp = u.components[root]
	}
	indexes := map[string]*strings.Builder{}
	for _, p := range pkgs {
		arch := p.arch
		if arch == "all" {
			arch = "amd64"
			if len(archs) > 0 {
				arch = archs[0]
			}
		}
		component := "main"
		if comp != "" {
			component = comp
		}
		dir := component + "/binary-" + arch
		if indexes[dir] == nil {
			indexes[dir] = &strings.Builder{}
		}
		fmt.Fprintf(indexes[dir], "Package: %s\nVersion: %s\nArchitecture: %s\nFilename: %s\nSize: %d\nSHA256: %s\n\n",
			p.name, p.version, p.arch, p.filename, len(p.body), sha256Hex(p.body))
		u.files[root+"/"+p.filename] = p.body
	}
	dirs := make([]string, 0, len(indexes))
	for dir := range indexes {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	release := "Codename: " + codename + "\n"
	if len(archs) > 0 {
		release += "Architectures: " + strings.Join(archs, " ") + "\n"
	}
	if comp != "" {
		release += "Components: " + comp + "\n"
	}
	release += "SHA256:\n"
	for _, dir := range dirs {
		index := indexes[dir].String()
		release += fmt.Sprintf(" %s %d %s/Packages\n", sha256Hex(index), len(index), dir)
		u.files[root+"/dists/"+suite+"/"+dir+"/Packages"] = index
	}
	u.files[root+"/dists/"+suite+"/Release"] = release
}

func (u *debUpstream) hitsFor(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

const (
	debMirrorCurlPath    = "/pool/main/c/curl/curl_7.88.1-10+deb12u8_amd64.deb"
	debMirrorOpenSSHPath = "/pool/main/o/openssh/openssh-client_9.2p1-2+deb12u3_amd64.deb"
	debMirrorTzdataPath  = "/pool/main/t/tzdata/tzdata_2024a-0+deb12u1_all.deb"
	debMirrorZlibPath    = "/pool/main/z/zlib/zlib1g_1.2.11.dfsg-2ubuntu9.2_amd64.deb"
)

// newDebianArchives serves a Debian archive with bookworm and
// bookworm-updates, without bookworm-security, which Debian keeps in an
// archive of its own, and an Ubuntu one with jammy under /ubuntu.
func newDebianArchives(t *testing.T) *debUpstream {
	t.Helper()
	u := newDebUpstream(t)
	u.addSuite("", "bookworm", "bookworm",
		debPackage{"curl", "7.88.1-10+deb12u5", "amd64", "pool/main/c/curl/curl_7.88.1-10+deb12u5_amd64.deb", "curl old"},
		debPackage{"openssh-client", "1:9.2p1-2+deb12u3", "amd64", strings.TrimPrefix(debMirrorOpenSSHPath, "/"), "openssh-client"},
		debPackage{"tzdata", "2024a-0+deb12u1", "all", strings.TrimPrefix(debMirrorTzdataPath, "/"), "tzdata"},
		debPackage{"dual", "1:2.0-1", "amd64", "pool/main/d/dual/dual_2.0-1_amd64.deb", "dual one"},
		debPackage{"dual", "2:2.0-1", "amd64", "pool/main/d/dual/dual_2.0-1+e2_amd64.deb", "dual two"},
		debPackage{"odd", "1.0-1", "amd64", "pool/main/o/odd/odd.deb", "odd"},
	)
	u.addSuite("", "bookworm-updates", "bookworm",
		debPackage{"curl", "7.88.1-10+deb12u8", "amd64", strings.TrimPrefix(debMirrorCurlPath, "/"), "curl new"},
	)
	u.addSuite("/ubuntu", "jammy", "jammy",
		debPackage{"zlib1g", "1:1.2.11.dfsg-2ubuntu9.2", "amd64", strings.TrimPrefix(debMirrorZlibPath, "/"), "zlib1g"},
	)
	return u
}

func debMirrorConfig(upstream *debUpstream) *config.Config {
	cfg := config.Default()
	cfg.Upstream.Debian = upstream.URL
	cfg.Upstream.DebianRepositories = map[string]string{"ubuntu": upstream.URL + "/ubuntu"}
	return cfg
}

func getDeb(proxy *handler.Proxy, upstream *debUpstream, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h := handler.NewDebianHandler(proxy, "http://proxy.test", upstream.URL, map[string]string{"ubuntu": upstream.URL + "/ubuntu"})
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestMirrorDebianPackagesAreServedFromCache(t *testing.T) {
	upstream := newDebianArchives(t)

	progress, proxy := mirrorWithConfig(t, debMirrorConfig(upstream),
		// syft: the release as distro, the epoch in the version.
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?arch=amd64&distro=debian-12",
		"pkg:deb/debian/openssh-client@1%3A9.2p1-2+deb12u3?arch=amd64&distro=debian-12",
		// trivy: the point release, and the epoch as a qualifier.
		"pkg:deb/ubuntu/zlib1g@1.2.11.dfsg-2ubuntu9.2?arch=amd64&distro=ubuntu-22.04&epoch=1",
		"pkg:deb/debian/curl@7.88.1-10+deb12u5?arch=amd64&distro=debian-12.4",
		// A codename, and an Architecture: all package.
		"pkg:deb/debian/tzdata@2024a-0+deb12u1?arch=all&distro=bookworm")

	if progress.Completed != 5 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	for _, path := range []string{debMirrorCurlPath, debMirrorOpenSSHPath, debMirrorTzdataPath, "/ubuntu" + debMirrorZlibPath, "/dists/bookworm/main/binary-amd64/Packages"} {
		if got := upstream.hitsFor(path); got != 1 {
			t.Errorf("%s fetched %d times, want 1", path, got)
		}
	}

	// apt gets the packages from the cache once the upstream is down.
	upstream.Close()
	for path, want := range map[string]string{
		debMirrorCurlPath:             "curl new",
		debMirrorOpenSSHPath:          "openssh-client",
		debMirrorTzdataPath:           "tzdata",
		"/ubuntu" + debMirrorZlibPath: "zlib1g",
	} {
		if rec := getDeb(proxy, upstream, path); rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s from cache: status = %d, body = %q, want 200 and %q", path, rec.Code, rec.Body, want)
		}
	}
}

func TestMirrorDebianRefusesWhatItCantFind(t *testing.T) {
	upstream := newDebianArchives(t)

	tests := map[string]string{
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?distro=debian-12":                            "arch",
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?arch=../../etc&distro=debian-12":             "arch",
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?arch=-amd64&distro=debian-12":                "arch",
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?arch=amd64":                                  "distro",
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?arch=amd64&distro=debian-99":                 "distro",
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?arch=amd64&distro=Bookworm/../x":             "distro",
		"pkg:deb/other/curl@7.88.1-10+deb12u8?arch=amd64&distro=debian-12":                  "upstream.debian_repositories",
		"pkg:deb/debian/curl@9.9.9-1?arch=amd64&distro=debian-12":                           "isn't in",
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?arch=arm64&distro=debian-12":                 "no Debian Packages index",
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?arch=amd64&distro=debian-13":                 "trixie",
		"pkg:deb/ubuntu/zlib1g@1.2.11.dfsg-2ubuntu9.2?arch=amd64&distro=ubuntu-26.04":       "resolute",
		"pkg:deb/debian/openssh-client@9.2p1-2+deb12u3?arch=amd64&distro=debian-12&epoch=2": "isn't in",
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?arch=amd64&distro=debian-12&epoch=x":         "epoch",
		"pkg:deb/debian/odd@1.0-1?arch=amd64&distro=debian-12":                              "isn't a package file",
		"pkg:deb/debian/dual@2.0-1?arch=amd64&distro=debian-12":                             "epoch",
	}
	for p, want := range tests {
		t.Run(p, func(t *testing.T) {
			progress, _ := mirrorWithConfig(t, debMirrorConfig(upstream), p)
			if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, want) {
				t.Errorf("failed = %d, errors = %v; want one error mentioning %q", progress.Failed, progress.Errors, want)
			}
		})
	}
}

// An epoch picks one of two versions that differ only in it, given in the
// version or as a qualifier.
func TestMirrorDebianEpochSelectsTheVersion(t *testing.T) {
	upstream := newDebianArchives(t)

	progress, _ := mirrorWithConfig(t, debMirrorConfig(upstream),
		"pkg:deb/debian/dual@2.0-1?arch=amd64&distro=debian-12&epoch=2",
		"pkg:deb/debian/dual@1%3A2.0-1?arch=amd64&distro=debian-12")
	if progress.Completed != 2 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	for _, path := range []string{"/pool/main/d/dual/dual_2.0-1+e2_amd64.deb", "/pool/main/d/dual/dual_2.0-1_amd64.deb"} {
		if got := upstream.hitsFor(path); got != 1 {
			t.Errorf("%s fetched %d times, want 1", path, got)
		}
	}
}

// An archive failing for a suite fails the package, rather than leaving the
// suite out of the indexes that are reused.
func TestMirrorDebianFailsOnAFailingArchive(t *testing.T) {
	upstream := newDebianArchives(t)
	upstream.fail["/dists/bookworm-security/InRelease"] = http.StatusInternalServerError

	progress, _ := mirrorWithConfig(t, debMirrorConfig(upstream),
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?arch=amd64&distro=debian-12")
	if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, "bookworm-security") {
		t.Errorf("failed = %d, errors = %v; want one error naming bookworm-security", progress.Failed, progress.Errors)
	}
}

// An Architecture: all package is found for any architecture, and epoch=0
// matches a version without an epoch, as trivy writes it.
func TestMirrorDebianMatchesArchitectureAllAndEpochZero(t *testing.T) {
	upstream := newDebianArchives(t)

	progress, _ := mirrorWithConfig(t, debMirrorConfig(upstream),
		"pkg:deb/debian/tzdata@2024a-0+deb12u1?arch=amd64&distro=debian-12",
		"pkg:deb/debian/curl@7.88.1-10+deb12u8?arch=amd64&distro=debian-12&epoch=0")
	if progress.Completed != 2 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
}

// An architecture name can have a hyphen, as Debian's ports have.
func TestMirrorDebianArchitectureWithHyphen(t *testing.T) {
	upstream := newDebUpstream(t)
	upstream.addSuite("/ports", "sid", "sid",
		debPackage{"hello", "2.10-3", "hurd-amd64", "pool/main/h/hello/hello_2.10-3_hurd-amd64.deb", "hello"},
	)
	cfg := config.Default()
	cfg.Upstream.DebianRepositories = map[string]string{"ports": upstream.URL + "/ports"}

	progress, _ := mirrorWithConfig(t, cfg, "pkg:deb/ports/hello@2.10-3?arch=hurd-amd64&distro=sid")
	if progress.Completed != 1 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	if got := upstream.hitsFor("/ports/pool/main/h/hello/hello_2.10-3_hurd-amd64.deb"); got != 1 {
		t.Errorf("package fetched %d times, want 1", got)
	}
}

// An Architecture: all package is looked up in the index of an architecture
// the archive's Release lists, as a ports archive has no amd64.
func TestMirrorDebianArchitectureAllOnAPortsArchive(t *testing.T) {
	upstream := newDebUpstream(t)
	upstream.architectures["/ports"] = []string{"arm64", "armhf"}
	upstream.addSuite("/ports", "jammy", "jammy",
		debPackage{"tzdata", "2024a-0ubuntu0.22.04", "all", "pool/main/t/tzdata/tzdata_2024a-0ubuntu0.22.04_all.deb", "tzdata"},
	)
	cfg := config.Default()
	cfg.Upstream.DebianRepositories = map[string]string{"ports": upstream.URL + "/ports"}

	progress, _ := mirrorWithConfig(t, cfg,
		"pkg:deb/ports/tzdata@2024a-0ubuntu0.22.04?arch=all&distro=ubuntu-22.04")
	if progress.Completed != 1 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	if got := upstream.hitsFor("/ports/dists/jammy/main/binary-amd64/Packages"); got != 0 {
		t.Errorf("binary-amd64 read %d times, want none", got)
	}
}

// A package in a component the archive's Release lists, such as the stable
// of Docker's apt repository, is found there.
func TestMirrorDebianReadsTheComponentsTheReleaseLists(t *testing.T) {
	upstream := newDebUpstream(t)
	upstream.components["/docker"] = "stable"
	upstream.addSuite("/docker", "bookworm", "bookworm",
		debPackage{"docker-ce", "5:27.3.1-1~debian.12~bookworm", "amd64", "pool/stable/d/docker-ce/docker-ce_27.3.1-1~debian.12~bookworm_amd64.deb", "docker-ce"},
	)
	cfg := config.Default()
	cfg.Upstream.DebianRepositories = map[string]string{"docker": upstream.URL + "/docker"}

	progress, _ := mirrorWithConfig(t, cfg, "pkg:deb/docker/docker-ce@5:27.3.1-1~debian.12~bookworm?arch=amd64&distro=debian-12")
	if progress.Completed != 1 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	if got := upstream.hitsFor("/docker/dists/bookworm/main/binary-amd64/Packages"); got != 0 {
		t.Errorf("main read %d times, want none: the Release doesn't list it", got)
	}
}

// The Release of a suite is read once for the suites, components and the
// architecture of Architecture: all packages, and once more by the index
// reader that checks the indexes against it.
func TestMirrorDebianReadsEachReleaseOnce(t *testing.T) {
	upstream := newDebUpstream(t)
	upstream.architectures["/ports"] = []string{"arm64", "armhf"}
	upstream.addSuite("/ports", "jammy", "jammy",
		debPackage{"tzdata", "2024a-0ubuntu0.22.04", "all", "pool/main/t/tzdata/tzdata_2024a-0ubuntu0.22.04_all.deb", "tzdata"},
	)
	cfg := config.Default()
	cfg.Upstream.DebianRepositories = map[string]string{"ports": upstream.URL + "/ports"}

	progress, _ := mirrorWithConfig(t, cfg, "pkg:deb/ports/tzdata@2024a-0ubuntu0.22.04?arch=all&distro=ubuntu-22.04")
	if progress.Completed != 1 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	if got := upstream.hitsFor("/ports/dists/jammy/Release"); got != 2 {
		t.Errorf("Release read %d times, want 2", got)
	}
}

// Suites of a release that list other components, or none, are each read
// for theirs, and an Architecture: all package is found where the suites
// hold such packages in the indexes of different architectures.
func TestMirrorDebianSuitesThatDiffer(t *testing.T) {
	upstream := newDebUpstream(t)
	upstream.architectures["/vendor#jammy"] = []string{"arm64", "armhf"}
	upstream.architectures["/vendor#jammy-updates"] = []string{"armhf"}
	upstream.components["/vendor#jammy-updates"] = "stable"
	upstream.addSuite("/vendor", "jammy", "jammy",
		debPackage{"tzdata", "2024a-0ubuntu0.22.04", "all", "pool/main/t/tzdata/tzdata_2024a-0ubuntu0.22.04_all.deb", "tzdata"},
	)
	upstream.addSuite("/vendor", "jammy-updates", "jammy",
		debPackage{"agent", "1.2-1", "all", "pool/stable/a/agent/agent_1.2-1_all.deb", "agent"},
	)
	cfg := config.Default()
	cfg.Upstream.DebianRepositories = map[string]string{"vendor": upstream.URL + "/vendor"}

	progress, _ := mirrorWithConfig(t, cfg,
		"pkg:deb/vendor/tzdata@2024a-0ubuntu0.22.04?arch=all&distro=ubuntu-22.04",
		"pkg:deb/vendor/agent@1.2-1?arch=all&distro=ubuntu-22.04")
	if progress.Completed != 2 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
}
