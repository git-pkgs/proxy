package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
)

// rpmPackage is one package of a primary file, with the file it locates.
type rpmPackage struct {
	name, epoch, ver, rel, arch, body string
	// file overrides the file name built from the name and version.
	file string
}

func (p rpmPackage) filename() string {
	if p.file != "" {
		return p.file
	}
	return fmt.Sprintf("%s-%s-%s.%s.rpm", p.name, p.ver, p.rel, p.arch)
}

// rpmUpstream serves RPM repositories below its root and counts requests
// by path.
type rpmUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string]string
	hits  map[string]int
}

func newRPMUpstream(t *testing.T) *rpmUpstream {
	t.Helper()
	u := &rpmUpstream{files: map[string]string{}, hits: map[string]int{}}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		body, ok := u.files[r.URL.Path]
		u.hits[r.URL.Path]++
		u.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *rpmUpstream) hitsFor(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

func rpmSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// addRepository serves the repository at dir, below the root, with pkgs.
func (u *rpmUpstream) addRepository(t *testing.T, dir string, pkgs ...rpmPackage) {
	t.Helper()
	var primary strings.Builder
	primary.WriteString(`<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm">`)
	for _, p := range pkgs {
		href := "Packages/" + p.name[:1] + "/" + p.filename()
		fmt.Fprintf(&primary, `<package type="rpm"><name>%s</name><arch>%s</arch><version epoch="%s" ver="%s" rel="%s"/>`+
			`<checksum type="sha256" pkgid="YES">%s</checksum><size package="%d"/><location href="%s"/></package>`,
			p.name, p.arch, p.epoch, p.ver, p.rel, rpmSHA256(p.body), len(p.body), href)
		u.files["/"+dir+"/"+href] = p.body
	}
	primary.WriteString(`</metadata>`)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = io.WriteString(zw, primary.String())
	_ = zw.Close()
	compressed := gz.String()
	u.files["/"+dir+"/repodata/primary.xml.gz"] = compressed
	u.files["/"+dir+"/repodata/repomd.xml"] = fmt.Sprintf(`<repomd xmlns="http://linux.duke.edu/metadata/repo">`+
		`<data type="primary"><checksum type="sha256">%s</checksum><location href="repodata/primary.xml.gz"/><size>%d</size></data></repomd>`,
		rpmSHA256(compressed), len(compressed))
}

const (
	rpmMirrorCurlPath   = "/updates/40/Everything/x86_64/Packages/c/curl-8.6.0-10.fc40.x86_64.rpm"
	rpmMirrorBashPath   = "/releases/40/Everything/x86_64/os/Packages/b/bash-5.2.26-3.fc40.x86_64.rpm"
	rpmMirrorTzdataPath = "/releases/40/Everything/x86_64/os/Packages/t/tzdata-2024a-5.fc40.noarch.rpm"
)

// newFedoraTree serves Fedora 40's release and updates repositories for
// x86_64, laid out as on dl.fedoraproject.org.
func newFedoraTree(t *testing.T) *rpmUpstream {
	t.Helper()
	u := newRPMUpstream(t)
	u.addRepository(t, "releases/40/Everything/x86_64/os",
		rpmPackage{"curl", "0", "8.6.0", "7.fc40", "x86_64", "curl old", ""},
		rpmPackage{"bash", "0", "5.2.26", "3.fc40", "x86_64", "bash", ""},
		rpmPackage{"tzdata", "0", "2024a", "5.fc40", "noarch", "tzdata", ""},
		rpmPackage{"dual", "1", "2.0", "1.fc40", "x86_64", "dual one", ""},
		rpmPackage{"odd", "0", "1.0", "1.fc40", "x86_64", "odd", "odd.rpm"},
	)
	u.addRepository(t, "updates/40/Everything/x86_64",
		rpmPackage{"curl", "0", "8.6.0", "10.fc40", "x86_64", "curl new", ""},
		rpmPackage{"dual", "2", "2.0", "1.fc40", "x86_64", "dual two", ""},
		rpmPackage{"dual", "3", "2.0", "1.fc40", "x86_64", "dual three", ""},
	)
	return u
}

func rpmMirrorConfig(upstream *rpmUpstream) *config.Config {
	cfg := config.Default()
	cfg.Upstream.RPM = upstream.URL
	return cfg
}

func getRPM(proxy *handler.Proxy, upstream *rpmUpstream, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h := handler.NewRPMHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestMirrorRPMPackagesAreServedFromCache(t *testing.T) {
	upstream := newFedoraTree(t)

	progress, proxy := mirrorWithConfig(t, rpmMirrorConfig(upstream),
		// syft writes the epoch as a qualifier, 0 included.
		"pkg:rpm/fedora/curl@8.6.0-10.fc40?arch=x86_64&distro=fedora-40",
		"pkg:rpm/fedora/bash@5.2.26-3.fc40?arch=x86_64&distro=fedora-40&epoch=0",
		"pkg:rpm/fedora/tzdata@2024a-5.fc40?arch=noarch&distro=fedora-40",
		// A noarch package asked for by the machine's architecture.
		"pkg:rpm/fedora/tzdata@2024a-5.fc40?arch=x86_64&distro=fedora-40")

	if progress.Completed+progress.Skipped != 4 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	for _, path := range []string{rpmMirrorCurlPath, rpmMirrorBashPath, rpmMirrorTzdataPath,
		"/updates/40/Everything/x86_64/repodata/primary.xml.gz", "/releases/40/Everything/x86_64/os/repodata/primary.xml.gz"} {
		if got := upstream.hitsFor(path); got != 1 {
			t.Errorf("%s fetched %d times, want 1", path, got)
		}
	}

	// dnf gets the packages from the cache once the upstream is down.
	upstream.Close()
	for path, want := range map[string]string{
		rpmMirrorCurlPath:   "curl new",
		rpmMirrorBashPath:   "bash",
		rpmMirrorTzdataPath: "tzdata",
	} {
		if rec := getRPM(proxy, upstream, path); rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s from cache: status = %d, body = %q, want 200 and %q", path, rec.Code, rec.Body, want)
		}
	}
}

// upstream.rpm_repository_paths lays out another distribution's tree, with
// the major release in the path.
func TestMirrorRPMFollowsConfiguredRepositoryPaths(t *testing.T) {
	upstream := newRPMUpstream(t)
	upstream.addRepository(t, "9/BaseOS/x86_64/os", rpmPackage{"bash", "0", "5.1.8", "9.el9", "x86_64", "rocky bash", ""})
	cfg := rpmMirrorConfig(upstream)
	cfg.Upstream.RPMRepositoryPaths = []string{"{major}/AppStream/{arch}/os", "{major}/BaseOS/{arch}/os"}

	progress, _ := mirrorWithConfig(t, cfg, "pkg:rpm/rocky/bash@5.1.8-9.el9?arch=x86_64&distro=rocky-9.4")
	if progress.Completed != 1 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	if got := upstream.hitsFor("/9/BaseOS/x86_64/os/Packages/b/bash-5.1.8-9.el9.x86_64.rpm"); got != 1 {
		t.Errorf("package fetched %d times, want 1", got)
	}
}

// An epoch picks one of the builds that differ only in it, from the first
// repository that has it.
func TestMirrorRPMEpochSelectsTheBuild(t *testing.T) {
	upstream := newFedoraTree(t)

	progress, _ := mirrorWithConfig(t, rpmMirrorConfig(upstream),
		"pkg:rpm/fedora/dual@2.0-1.fc40?arch=x86_64&distro=fedora-40&epoch=1")
	if progress.Completed != 1 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	if got := upstream.hitsFor("/releases/40/Everything/x86_64/os/Packages/d/dual-2.0-1.fc40.x86_64.rpm"); got != 1 {
		t.Errorf("epoch 1 build fetched %d times, want 1", got)
	}
}

func TestMirrorRPMRefusesWhatItCantFind(t *testing.T) {
	upstream := newFedoraTree(t)

	tests := map[string]string{
		"pkg:rpm/fedora/curl@8.6.0-10.fc40?distro=fedora-40":                     "arch",
		"pkg:rpm/fedora/curl@8.6.0-10.fc40?arch=../x&distro=fedora-40":           "arch",
		"pkg:rpm/fedora/curl@8.6.0-10.fc40?arch=x86_64":                          "distro",
		"pkg:rpm/fedora/curl@8.6.0-10.fc40?arch=x86_64&distro=fedora":            "distro",
		"pkg:rpm/fedora/curl@8.6.0-10.fc40?arch=x86_64&distro=fedora-40/../x":    "distro",
		"pkg:rpm/fedora/curl@8.6.0-10.fc40?arch=x86_64&distro=fedora-40&epoch=x": "epoch",
		"pkg:rpm/fedora/curl@9.9.9-1.fc40?arch=x86_64&distro=fedora-40":          "isn't in",
		"pkg:rpm/fedora/curl@8.6.0-10.fc40?arch=aarch64&distro=fedora-40":        "isn't in",
		"pkg:rpm/fedora/curl@8.6.0-10.fc40?arch=x86_64&distro=fedora-40&epoch=3": "isn't in",
		"pkg:rpm/fedora/dual@2.0-1.fc40?arch=x86_64&distro=fedora-40":            "epoch",
		"pkg:rpm/fedora/odd@1.0-1.fc40?arch=x86_64&distro=fedora-40":             "isn't a package file",
	}
	for p, want := range tests {
		t.Run(p, func(t *testing.T) {
			progress, _ := mirrorWithConfig(t, rpmMirrorConfig(upstream), p)
			if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, want) {
				t.Errorf("failed = %d, errors = %v; want one error mentioning %q", progress.Failed, progress.Errors, want)
			}
		})
	}
}
