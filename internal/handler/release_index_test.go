package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// releaseArchive is an upstream repository whose release file and index
// change together, one generation at a time.
type releaseArchive struct {
	gen atomic.Int32
	// releasePath and indexPath are the request paths, release renders the
	// release file listing index's sha256 for a generation.
	releasePath, indexPath string
	release                func(indexSHA256 string, size int) string
}

func indexBody(gen int32) string { return fmt.Sprintf("index generation %d\n", gen) }

func (a *releaseArchive) serve(w http.ResponseWriter, r *http.Request) {
	gen := a.gen.Load()
	index := indexBody(gen)
	switch r.URL.Path {
	case "/" + a.releasePath:
		_, _ = w.Write([]byte(a.release(sha256Hex(index), len(index))))
	case "/" + a.indexPath:
		_, _ = w.Write([]byte(index))
	default:
		http.NotFound(w, r)
	}
}

type releaseIndexCase struct {
	name      string
	ecosystem string
	archive   *releaseArchive
	handler   func(p *Proxy, upstreamURL string) (http.Handler, func(path string) string)
}

func releaseIndexCases() []releaseIndexCase {
	return []releaseIndexCase{
		{
			name:      "debian InRelease",
			ecosystem: "debian",
			archive: &releaseArchive{
				releasePath: "dists/stable/InRelease",
				indexPath:   "dists/stable/main/binary-amd64/Packages",
				release: func(sum string, size int) string {
					return "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\nSuite: stable\nMD5Sum:\n 00000000000000000000000000000000 1 main/binary-amd64/Packages\nSHA256:\n" +
						fmt.Sprintf(" %s %d main/binary-amd64/Packages\n", sum, size) +
						"-----BEGIN PGP SIGNATURE-----\n\nxx\n-----END PGP SIGNATURE-----\n"
				},
			},
			handler: func(p *Proxy, upstreamURL string) (http.Handler, func(string) string) {
				h := NewDebianHandler(p, "http://proxy.test", upstreamURL, nil)
				return h.Routes(), func(path string) string { return h.metadataCacheKeyFor("", h.upstreamURL, path) }
			},
		},
		{
			name:      "rpm repomd.xml",
			ecosystem: "rpm",
			archive: &releaseArchive{
				releasePath: "f/39/os/repodata/repomd.xml",
				indexPath:   "f/39/os/repodata/primary.xml.gz",
				release: func(sum string, _ int) string {
					return `<?xml version="1.0" encoding="UTF-8"?>
<repomd xmlns="http://linux.duke.edu/metadata/repo"><revision>1</revision>
<data type="primary"><checksum type="sha256">` + sum + `</checksum><location href="repodata/primary.xml.gz"/></data>
</repomd>`
				},
			},
			handler: func(p *Proxy, upstreamURL string) (http.Handler, func(string) string) {
				h := NewRPMHandlerWithUpstream(p, "http://proxy.test", upstreamURL)
				return h.Routes(), h.metadataCacheKey
			},
		},
	}
}

func getBody(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
	}
	return w.Body.String()
}

// TestIndexFollowsCachedRelease checks that the index files a release file
// lists are served as that release file lists them, whatever the index's own
// cache entry says.
func TestIndexFollowsCachedRelease(t *testing.T) {
	for _, tc := range releaseIndexCases() {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.archive
			upstream := httptest.NewServer(http.HandlerFunc(a.serve))
			t.Cleanup(upstream.Close)
			proxy, db, _, _ := setupTestProxy(t)
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Hour
			proxy.HTTPClient = upstream.Client()
			h, key := tc.handler(proxy, upstream.URL)
			expire := func(path string) {
				t.Helper()
				if _, err := db.ExpireMetadataCache(tc.ecosystem, []string{key(path)}, time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}

			getBody(t, h, a.releasePath)
			if got := getBody(t, h, a.indexPath); got != indexBody(0) {
				t.Fatalf("index = %q", got)
			}

			// The index entry expires before the release file's while
			// upstream moves on: the cached release still lists generation 0.
			a.gen.Store(1)
			expire(a.indexPath)
			if got := getBody(t, h, a.indexPath); got != indexBody(0) {
				t.Errorf("index after its entry expired = %q, want the one the cached release lists", got)
			}

			// The release file is refetched while the index is still cached:
			// the index must follow it.
			expire(a.releasePath)
			getBody(t, h, a.releasePath)
			if got := getBody(t, h, a.indexPath); got != indexBody(1) {
				t.Errorf("index after release refetch = %q, want %q", got, indexBody(1))
			}
		})
	}
}

// TestIndexMismatchExpiresRelease checks that when upstream only has an
// index newer than the cached release file, the release file is refetched
// on its next request, so the next update gets a matching pair.
func TestIndexMismatchExpiresRelease(t *testing.T) {
	for _, tc := range releaseIndexCases() {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.archive
			upstream := httptest.NewServer(http.HandlerFunc(a.serve))
			t.Cleanup(upstream.Close)
			proxy, _, _, _ := setupTestProxy(t)
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Hour
			proxy.HTTPClient = upstream.Client()
			h, _ := tc.handler(proxy, upstream.URL)

			first := getBody(t, h, a.releasePath)
			a.gen.Store(1)
			if got := getBody(t, h, a.indexPath); got != indexBody(1) {
				t.Fatalf("index = %q", got)
			}
			if got := getBody(t, h, a.releasePath); got == first {
				t.Errorf("release file still the cached one, which does not list the index served")
			}
		})
	}
}
