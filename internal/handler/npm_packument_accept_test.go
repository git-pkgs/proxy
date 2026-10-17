package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

// TestNPMTarballKeepsCachedPackumentFormat checks that a tarball download
// revalidates the packument in the format the metadata route cached it in,
// rather than replacing the abbreviated document every client is served with
// the full one.
func TestNPMTarballKeepsCachedPackumentFormat(t *testing.T) {
	const abbreviated = "application/vnd.npm.install-v1+json"
	var (
		mu      sync.Mutex
		accepts []string
	)
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept := r.Header.Get("Accept")
		mu.Lock()
		accepts = append(accepts, accept)
		mu.Unlock()
		contentType, etag := contentTypeJSON, `"full"`
		if strings.HasPrefix(accept, abbreviated) {
			contentType, etag = abbreviated, `"abbreviated"`
		}
		w.Header().Set(headerETag, etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set(headerContentType, contentType)
		_, _ = fmt.Fprintf(w, `{"name":"demo","versions":{"1.0.0":{"name":"demo","version":"1.0.0","dist":{"tarball":%q}}}}`,
			upstream.URL+"/demo/-/demo-1.0.0.tgz")
	}))
	t.Cleanup(upstream.Close)

	proxy, db, _, artifactFetcher := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Nanosecond // the tarball request revalidates
	proxy.HTTPClient = upstream.Client()
	artifactFetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package")), ContentType: "application/gzip"}
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	if w := serveNPM(t, h, http.MethodGet, "/demo", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("metadata: status %d: %s", w.Code, w.Body.String())
	}
	if w := serveNPM(t, h, http.MethodGet, "/demo/-/demo-1.0.0.tgz", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("tarball: status %d: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(accepts) != 2 {
		t.Fatalf("upstream packument requests = %d (%q), want 2", len(accepts), accepts)
	}
	for _, accept := range accepts {
		if accept != npmAcceptDefault {
			t.Errorf("packument requested with Accept %q, want %q", accept, npmAcceptDefault)
		}
	}
	entry, err := db.GetMetadataCache("npm", "demo")
	if err != nil || entry == nil {
		t.Fatalf("cache row: %v, %v", entry, err)
	}
	if entry.ContentType.String != abbreviated {
		t.Errorf("cached packument Content-Type = %q, want %q", entry.ContentType.String, abbreviated)
	}
}
