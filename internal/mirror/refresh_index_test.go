package mirror

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// indexServer serves repository index documents that a test can change.
type indexServer struct {
	*httptest.Server
	mu   sync.Mutex
	docs map[string][]byte
}

func newIndexServer(t *testing.T) *indexServer {
	t.Helper()
	s := &indexServer{docs: map[string][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		body, ok := s.docs[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *indexServer) set(path string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[path] = body
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// setupIndexRefresh returns a mirror whose package files come from a
// servingFetcher and whose indexes come from an indexServer.
func setupIndexRefresh(t *testing.T) (*Mirror, *servingFetcher, *indexServer) {
	t.Helper()
	m, fetcher := setupRefreshMirror(t, "package")
	idx := newIndexServer(t)
	m.proxy.HTTPClient = idx.Client()
	m.proxy.CacheMetadata = true
	return m, fetcher, idx
}

// clientFetchesCurl caches an index document and a curl package file the
// way the handlers do for a client.
func clientFetchesCurl(t *testing.T, m *Mirror, metaEcosystem, indexURL, ecosystem, version, cacheFilename, fileURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := m.proxy.FetchOrCacheMetadata(ctx, metaEcosystem, "key:"+indexURL, indexURL, "*/*"); err != nil {
		t.Fatalf("fetching %s: %v", indexURL, err)
	}
	result, err := m.proxy.GetOrFetchArtifactFromURL(ctx, ecosystem, "curl", version, cacheFilename, fileURL)
	if err != nil {
		t.Fatalf("fetching %s: %v", fileURL, err)
	}
	_ = result.Reader.Close()
}

func refreshOnce(t *testing.T, m *Mirror) *Progress {
	t.Helper()
	progress, err := m.Refresh(context.Background(), Refresh{Top: 10, Since: time.Hour})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return progress
}

func cachedNames(t *testing.T, m *Mirror, ecosystem, filename string) []string {
	t.Helper()
	names, err := m.db.CachedArtifactNames(ecosystem, filename)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestRefreshAlpineIndex(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	dir := "/alpine/v3.20/main/aarch64"
	idx.set(dir+"/APKINDEX.tar.gz", apkIndexArchive(t, "P:curl\nV:8.9.0-r0\nA:aarch64\n\nP:busybox\nV:1.36.1-r29\n\n"))
	clientFetchesCurl(t, m, "alpine", idx.URL+dir+"/APKINDEX.tar.gz",
		"alpine", "8.9.0-r0", "edge/v3.20/main/aarch64/curl-8.9.0-r0.apk", idx.URL+dir+"/curl-8.9.0-r0.apk")

	// A new release of curl; busybox was never downloaded and stays out.
	idx.set(dir+"/APKINDEX.tar.gz", apkIndexArchive(t, "P:curl\nV:8.9.1-r0\nA:aarch64\n\nP:busybox\nV:1.36.1-r30\n\n"))
	fetcher.reset()
	progress := refreshOnce(t, m)

	if want := []string{idx.URL + dir + "/curl-8.9.1-r0.apk"}; !slices.Equal(fetcher.fetched(), want) || progress.Completed != 1 {
		t.Fatalf("progress = %+v, fetched %v; want %v", progress, fetcher.fetched(), want)
	}
	// Cached where the APK handler looks for it.
	if names := cachedNames(t, m, "alpine", "edge/v3.20/main/aarch64/curl-8.9.1-r0.apk"); !slices.Equal(names, []string{"curl"}) {
		t.Errorf("cached names = %v, want [curl]", names)
	}

	fetcher.reset()
	if progress := refreshOnce(t, m); progress.Completed != 0 || len(fetcher.fetched()) != 0 {
		t.Errorf("second run: progress = %+v, fetched %v; want nothing", progress, fetcher.fetched())
	}
}

func TestRefreshIndexNeverDowngrades(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	dir := "/alpine/v3.20/main/aarch64"
	idx.set(dir+"/APKINDEX.tar.gz", apkIndexArchive(t, "P:curl\nV:8.9.1-r0\n\n"))
	clientFetchesCurl(t, m, "alpine", idx.URL+dir+"/APKINDEX.tar.gz",
		"alpine", "8.9.1-r0", "edge/v3.20/main/aarch64/curl-8.9.1-r0.apk", idx.URL+dir+"/curl-8.9.1-r0.apk")
	idx.set(dir+"/APKINDEX.tar.gz", apkIndexArchive(t, "P:curl\nV:8.9.0-r0\n\n"))
	fetcher.reset()

	if progress := refreshOnce(t, m); progress.Completed != 0 || len(fetcher.fetched()) != 0 {
		t.Errorf("progress = %+v, fetched %v; want nothing", progress, fetcher.fetched())
	}
}

func TestRefreshDebianIndex(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	release := []byte("Suite: stable\nSHA256:\n 0000 1 main/binary-amd64/Packages.gz\n 0000 1 main/binary-arm64/Packages.gz\n")
	packages := func(curl string) []byte {
		return gzipped(t, "Package: curl\nVersion: "+curl+"\nArchitecture: amd64\nFilename: pool/main/c/curl/curl_"+curl+"_amd64.deb\nSHA256: "+
			"bc4a71180870f7945155fbb02f4b0a2e3faa2a62d6d31b7039013055ed19869a\n\n"+
			"Package: zlib1g\nVersion: 1:1.2.13.dfsg-1\nArchitecture: amd64\nFilename: pool/main/z/zlib/zlib1g_1.2.13.dfsg-1_amd64.deb\n\n")
	}
	idx.set("/debian/dists/bookworm/InRelease", release)
	idx.set("/debian/dists/bookworm/main/binary-amd64/Packages.gz", packages("7.88.1-10"))
	clientFetchesCurl(t, m, "debian", idx.URL+"/debian/dists/bookworm/InRelease",
		"deb", "7.88.1-10", "curl_7.88.1-10_amd64.deb", idx.URL+"/debian/pool/main/c/curl/curl_7.88.1-10_amd64.deb")

	idx.set("/debian/dists/bookworm/main/binary-amd64/Packages.gz", packages("7.88.1-10+deb12u8"))
	fetcher.reset()
	progress := refreshOnce(t, m)

	// Only the amd64 index is read: nothing was downloaded for arm64. The
	// file is checked against the index's SHA-256 of "package".
	want := []string{idx.URL + "/debian/pool/main/c/curl/curl_7.88.1-10+deb12u8_amd64.deb"}
	if !slices.Equal(fetcher.fetched(), want) || progress.Completed != 1 || progress.Failed != 0 {
		t.Fatalf("progress = %+v, fetched %v; want %v", progress, fetcher.fetched(), want)
	}
	if names := cachedNames(t, m, "deb", "curl_7.88.1-10+deb12u8_amd64.deb"); !slices.Equal(names, []string{"curl"}) {
		t.Errorf("cached names = %v, want [curl]", names)
	}
}

func TestRefreshDebianIndexChecksDigest(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	idx.set("/debian/dists/bookworm/InRelease", []byte("SHA256:\n 0000 1 main/binary-amd64/Packages\n"))
	idx.set("/debian/dists/bookworm/main/binary-amd64/Packages", []byte("Package: curl\nFilename: pool/main/c/curl/curl_7.88.1-10_amd64.deb\n\n"))
	clientFetchesCurl(t, m, "debian", idx.URL+"/debian/dists/bookworm/InRelease",
		"deb", "7.88.1-10", "curl_7.88.1-10_amd64.deb", idx.URL+"/debian/pool/main/c/curl/curl_7.88.1-10_amd64.deb")
	idx.set("/debian/dists/bookworm/main/binary-amd64/Packages", []byte(
		"Package: curl\nFilename: pool/main/c/curl/curl_7.88.1-11_amd64.deb\nSHA256: 0000000000000000000000000000000000000000000000000000000000000000\n\n"))
	fetcher.reset()

	progress := refreshOnce(t, m)
	if progress.Failed != 1 || progress.Completed != 0 {
		t.Errorf("progress = %+v, want the file failing its digest", progress)
	}
	if names := cachedNames(t, m, "deb", "curl_7.88.1-11_amd64.deb"); len(names) != 0 {
		t.Errorf("a file failing its digest was cached as %v", names)
	}
}

func TestRefreshRPMIndex(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	base := "/fedora/updates/40/Everything/x86_64"
	primary := func(rel string) []byte {
		return gzipped(t, `<metadata xmlns="http://linux.duke.edu/metadata/common"><package type="rpm"><name>curl</name><arch>x86_64</arch>`+
			`<location href="Packages/c/curl-8.6.0-`+rel+`.fc40.x86_64.rpm"/></package></metadata>`)
	}
	repomdFor := func(href string) []byte {
		return []byte(`<repomd><data type="primary"><location href="` + href + `"/></data></repomd>`)
	}
	idx.set(base+"/repodata/repomd.xml", repomdFor("repodata/aaaa-primary.xml.gz"))
	idx.set(base+"/repodata/aaaa-primary.xml.gz", primary("10"))
	clientFetchesCurl(t, m, "rpm", idx.URL+base+"/repodata/repomd.xml",
		"rpm", "8.6.0-10.fc40", "curl-8.6.0-10.fc40.x86_64.rpm", idx.URL+base+"/Packages/c/curl-8.6.0-10.fc40.x86_64.rpm")

	idx.set(base+"/repodata/repomd.xml", repomdFor("repodata/bbbb-primary.xml.gz"))
	idx.set(base+"/repodata/bbbb-primary.xml.gz", primary("11"))
	fetcher.reset()
	progress := refreshOnce(t, m)

	want := []string{idx.URL + base + "/Packages/c/curl-8.6.0-11.fc40.x86_64.rpm"}
	if !slices.Equal(fetcher.fetched(), want) || progress.Completed != 1 {
		t.Fatalf("progress = %+v, fetched %v; want %v", progress, fetcher.fetched(), want)
	}
	if names := cachedNames(t, m, "rpm", "curl-8.6.0-11.fc40.x86_64.rpm"); !slices.Equal(names, []string{"curl"}) {
		t.Errorf("cached names = %v, want [curl]", names)
	}
}

func TestRefreshIndexWithoutRecordedURLDoesNothing(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	result, err := m.proxy.GetOrFetchArtifactFromURL(context.Background(), "alpine", "curl", "8.9.0-r0",
		"edge/v3.20/main/aarch64/curl-8.9.0-r0.apk", idx.URL+"/alpine/v3.20/main/aarch64/curl-8.9.0-r0.apk")
	if err != nil {
		t.Fatal(err)
	}
	_ = result.Reader.Close()
	fetcher.reset()

	if progress := refreshOnce(t, m); progress.Completed != 0 || progress.Failed != 0 || len(fetcher.fetched()) != 0 {
		t.Errorf("progress = %+v, fetched %v; want nothing", progress, fetcher.fetched())
	}
}
