package mirror

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/denylist"
	"github.com/git-pkgs/registries"
)

// debCurlPackages returns a Packages index listing curl at version.
func debCurlPackages(t *testing.T, version string) []byte {
	t.Helper()
	return gzipped(t, "Package: curl\nVersion: "+version+"\nArchitecture: amd64\nFilename: pool/main/c/curl/curl_"+version+"_amd64.deb\n\n")
}

// TestRefreshDebianIndexPerSuite checks that suites sharing a pool are
// refreshed each on its own: a newer curl cached from trixie must not keep
// bookworm's security update from being mirrored.
func TestRefreshDebianIndexPerSuite(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	release := []byte("SHA256:\n 0000 1 main/binary-amd64/Packages.gz\n")
	pool := idx.URL + "/debian/pool/main/c/curl/"
	for suite, version := range map[string]string{"bookworm": "7.88.1-10", "trixie": "8.14.1-2"} {
		idx.set("/debian/dists/"+suite+"/InRelease", release)
		idx.set("/debian/dists/"+suite+"/main/binary-amd64/Packages.gz", debCurlPackages(t, version))
		clientFetchesCurl(t, m, "debian", idx.URL+"/debian/dists/"+suite+"/InRelease",
			"deb", version, "curl_"+version+"_amd64.deb", pool+"curl_"+version+"_amd64.deb")
	}

	idx.set("/debian/dists/bookworm/main/binary-amd64/Packages.gz", debCurlPackages(t, "7.88.1-10+deb12u8"))
	idx.set("/debian/dists/trixie/main/binary-amd64/Packages.gz", debCurlPackages(t, "8.14.1-2+deb13u1"))
	fetcher.reset()
	progress := refreshOnce(t, m)

	want := []string{pool + "curl_7.88.1-10+deb12u8_amd64.deb", pool + "curl_8.14.1-2+deb13u1_amd64.deb"}
	got := fetcher.fetched()
	slices.Sort(got)
	if !slices.Equal(got, want) || progress.Completed != 2 {
		t.Errorf("progress = %+v, fetched %v; want %v", progress, got, want)
	}
}

// TestRefreshDebianIndexSkipsOtherSuite checks that a suite's version of a
// package is not mirrored when only a newer one is cached: clients of that
// suite never fetched the package.
func TestRefreshDebianIndexSkipsOtherSuite(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	release := []byte("SHA256:\n 0000 1 main/binary-amd64/Packages.gz\n")
	pool := idx.URL + "/debian/pool/main/c/curl/"
	idx.set("/debian/dists/trixie/InRelease", release)
	idx.set("/debian/dists/trixie/main/binary-amd64/Packages.gz", debCurlPackages(t, "8.14.1-2"))
	clientFetchesCurl(t, m, "debian", idx.URL+"/debian/dists/trixie/InRelease",
		"deb", "8.14.1-2", "curl_8.14.1-2_amd64.deb", pool+"curl_8.14.1-2_amd64.deb")
	// A bookworm client fetched its index but never curl.
	idx.set("/debian/dists/bookworm/InRelease", release)
	idx.set("/debian/dists/bookworm/main/binary-amd64/Packages.gz", debCurlPackages(t, "7.88.1-10+deb12u8"))
	if _, _, err := m.proxy.FetchOrCacheMetadata(t.Context(), "debian", "key:bookworm", idx.URL+"/debian/dists/bookworm/InRelease", "*/*"); err != nil {
		t.Fatal(err)
	}
	fetcher.reset()

	if progress := refreshOnce(t, m); progress.Completed != 0 || len(fetcher.fetched()) != 0 {
		t.Errorf("progress = %+v, fetched %v; want nothing", progress, fetcher.fetched())
	}
}

// TestRefreshRPMIndexListingSeveralVersions checks that a repository
// listing old versions next to the newest mirrors only the newest, and
// nothing once it is cached.
func TestRefreshRPMIndexListingSeveralVersions(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	base := "/fedora/updates/40/Everything/x86_64"
	primary := func(rels ...string) []byte {
		var pkgs string
		for _, rel := range rels {
			pkgs += `<package type="rpm"><name>curl</name><arch>x86_64</arch><location href="Packages/c/curl-8.6.0-` + rel + `.fc40.x86_64.rpm"/></package>`
		}
		return gzipped(t, `<metadata xmlns="http://linux.duke.edu/metadata/common">`+pkgs+`</metadata>`)
	}
	idx.set(base+"/repodata/repomd.xml", []byte(`<repomd><data type="primary"><location href="repodata/primary.xml.gz"/></data></repomd>`))
	idx.set(base+"/repodata/primary.xml.gz", primary("10"))
	clientFetchesCurl(t, m, "rpm", idx.URL+base+"/repodata/repomd.xml",
		"rpm", "8.6.0-10.fc40", "curl-8.6.0-10.fc40.x86_64.rpm", idx.URL+base+"/Packages/c/curl-8.6.0-10.fc40.x86_64.rpm")

	idx.set(base+"/repodata/primary.xml.gz", primary("10", "12", "11"))
	fetcher.reset()
	progress := refreshOnce(t, m)
	want := []string{idx.URL + base + "/Packages/c/curl-8.6.0-12.fc40.x86_64.rpm"}
	if !slices.Equal(fetcher.fetched(), want) || progress.Completed != 1 {
		t.Fatalf("progress = %+v, fetched %v; want %v", progress, fetcher.fetched(), want)
	}

	fetcher.reset()
	if progress := refreshOnce(t, m); progress.Completed != 0 || len(fetcher.fetched()) != 0 {
		t.Errorf("second run: progress = %+v, fetched %v; want nothing", progress, fetcher.fetched())
	}
}

// TestRefreshIndexFailureDoesNotStopRun checks that an index that can't be
// read is counted as failed and the other indexes are still refreshed.
func TestRefreshIndexFailureDoesNotStopRun(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	release := []byte("SHA256:\n 0000 1 main/binary-amd64/Packages.gz\n")
	pool := idx.URL + "/debian/pool/main/c/curl/"
	for suite, version := range map[string]string{"bookworm": "7.88.1-10", "trixie": "8.14.1-2"} {
		idx.set("/debian/dists/"+suite+"/InRelease", release)
		idx.set("/debian/dists/"+suite+"/main/binary-amd64/Packages.gz", debCurlPackages(t, version))
		clientFetchesCurl(t, m, "debian", idx.URL+"/debian/dists/"+suite+"/InRelease",
			"deb", version, "curl_"+version+"_amd64.deb", pool+"curl_"+version+"_amd64.deb")
	}
	idx.set("/debian/dists/bookworm/main/binary-amd64/Packages.gz", []byte("not gzip"))
	idx.set("/debian/dists/trixie/main/binary-amd64/Packages.gz", debCurlPackages(t, "8.14.1-2+deb13u1"))
	fetcher.reset()

	progress := refreshOnce(t, m)
	want := []string{pool + "curl_8.14.1-2+deb13u1_amd64.deb"}
	if !slices.Equal(fetcher.fetched(), want) || progress.Completed != 1 || progress.Failed != 1 {
		t.Errorf("progress = %+v, fetched %v; want %v and one failure", progress, fetcher.fetched(), want)
	}
}

func TestNewestPerCachedLineGo(t *testing.T) {
	allowAll := func(registries.Version) bool { return true }
	for _, tt := range []struct {
		name     string
		cached   []string
		releases []registries.Version
		want     []string
	}{
		{"v prefix", []string{"v1.2.3"}, releases("v1.2.3", "v1.3.0"), []string{"v1.3.0"}},
		{"incompatible is its own line", []string{"v1.2.3"}, releases("v1.2.3", "v2.0.0+incompatible"), nil},
		{"pseudo-versions are prereleases", []string{"v1.2.3"}, releases("v1.2.3", "v1.2.4-0.20260101000000-abcdefabcdef"), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := newestPerCachedLine("golang", tt.cached, tt.releases, allowAll); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("newestPerCachedLine = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRefreshScopedNPMPackage(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "tarball")
	cacheNPM(t, m, "@babel/core", "7.24.0")
	m.Versions = stubReleases{"npm/@babel/core": oldReleases("7.24.0", "7.25.2")}
	fetcher.reset()

	progress := refreshOnce(t, m)
	if urls := fetcher.fetched(); progress.Completed != 1 || len(fetchedMatching(urls, "core-7.25.2.tgz")) != 1 {
		t.Errorf("progress = %+v, fetched %v; want @babel/core 7.25.2", progress, urls)
	}
}

func TestRefreshSkipsDeniedRelease(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "tarball")
	cacheNPM(t, m, "lodash", "4.17.20")
	m.Versions = stubReleases{"npm/lodash": oldReleases("4.17.20", "4.17.21", "4.17.22")}
	policy, err := denylist.New([]string{"pkg:npm/lodash@4.17.22"})
	if err != nil {
		t.Fatal(err)
	}
	m.proxy.Denylist = policy
	fetcher.reset()

	progress := refreshOnce(t, m)
	if urls := fetcher.fetched(); progress.Completed != 1 || len(fetchedMatching(urls, "lodash-4.17.21.tgz")) != 1 || len(fetchedMatching(urls, "4.17.22")) != 0 {
		t.Errorf("progress = %+v, fetched %v; want 4.17.21, not the denied 4.17.22", progress, urls)
	}
}

func TestRefreshListingFailureDoesNotStopOthers(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "tarball")
	cacheNPM(t, m, "lodash", "4.17.20")
	cacheNPM(t, m, "react", "18.3.0")
	m.Versions = stubReleases{"npm/react": oldReleases("18.3.0", "18.3.1")}
	fetcher.reset()

	progress := refreshOnce(t, m)
	if urls := fetcher.fetched(); progress.Completed != 1 || progress.Failed != 1 || len(fetchedMatching(urls, "react-18.3.1.tgz")) != 1 {
		t.Errorf("progress = %+v, fetched %v; want react refreshed and lodash failed", progress, urls)
	}
}

func TestRefreshStopsWhenCancelled(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "tarball")
	cacheNPM(t, m, "lodash", "4.17.20")
	m.Versions = stubReleases{"npm/lodash": oldReleases("4.17.20", "4.17.21")}
	fetcher.reset()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	progress, err := m.Refresh(ctx, Refresh{Top: 10, Since: time.Hour})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if progress.Completed != 0 || len(fetcher.fetched()) != 0 {
		t.Errorf("progress = %+v, fetched %v; want nothing after cancelling", progress, fetcher.fetched())
	}
}

// TestRefreshDebianArchitectureAll checks that a package for every
// architecture, listed in each binary-* index rather than one of its own,
// is refreshed.
func TestRefreshDebianArchitectureAll(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	packages := func(version string) []byte {
		return gzipped(t, "Package: ca-certificates\nVersion: "+version+"\nArchitecture: all\nFilename: pool/main/c/ca-certificates/ca-certificates_"+version+"_all.deb\n\n")
	}
	idx.set("/debian/dists/bookworm/InRelease", []byte("SHA256:\n 0000 1 main/binary-amd64/Packages.gz\n"))
	idx.set("/debian/dists/bookworm/main/binary-amd64/Packages.gz", packages("20230311"))
	pool := idx.URL + "/debian/pool/main/c/ca-certificates/"
	if _, _, err := m.proxy.FetchOrCacheMetadata(t.Context(), "debian", "key:bookworm", idx.URL+"/debian/dists/bookworm/InRelease", "*/*"); err != nil {
		t.Fatal(err)
	}
	result, err := m.proxy.GetOrFetchArtifactFromURL(t.Context(), "deb", "ca-certificates", "20230311", "ca-certificates_20230311_all.deb", pool+"ca-certificates_20230311_all.deb")
	if err != nil {
		t.Fatal(err)
	}
	_ = result.Reader.Close()

	idx.set("/debian/dists/bookworm/main/binary-amd64/Packages.gz", packages("20230311+deb12u1"))
	fetcher.reset()
	progress := refreshOnce(t, m)
	if want := []string{pool + "ca-certificates_20230311+deb12u1_all.deb"}; !slices.Equal(fetcher.fetched(), want) || progress.Completed != 1 {
		t.Errorf("progress = %+v, fetched %v; want %v", progress, fetcher.fetched(), want)
	}
}
