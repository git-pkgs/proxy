package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testPackument is a minimal npm packument the proxy can rewrite.
const testPackument = `{"name":"testpkg","versions":{"1.0.0":{"name":"testpkg","version":"1.0.0","dist":{"tarball":"https://registry.npmjs.org/testpkg/-/testpkg-1.0.0.tgz"}}}}`

// newStaleMetadataUpstream serves testPackument until status holds a non-zero
// HTTP status, which it answers instead.
func newStaleMetadataUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var status atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code := status.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = io.WriteString(w, testPackument)
	}))
	t.Cleanup(server.Close)
	return server, &status
}

// newStaleMetadataHandler returns an npm handler over an expiring metadata
// cache, with the rewrite cache enabled when rewriteCache is true.
func newStaleMetadataHandler(t *testing.T, upstreamURL string, rewriteCache bool) *NPMHandler {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Millisecond
	proxy.HTTPClient = http.DefaultClient
	if rewriteCache {
		proxy.SetMetadataRewriteCacheSize(1 << 20)
	}
	return NewNPMHandler(proxy, "http://proxy.test", upstreamURL)
}

// primeStaleMetadataCache fetches the packument once and lets the entry expire,
// so the next request goes upstream again.
func primeStaleMetadataCache(t *testing.T, h *NPMHandler) string {
	t.Helper()
	w := serveNPM(t, h, http.MethodGet, "/testpkg", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("priming request: status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	time.Sleep(5 * time.Millisecond)
	return w.Body.String()
}

func TestNPMMetadataStaleWarningAfterUpstreamFailure(t *testing.T) {
	for _, rewriteCache := range []bool{false, true} {
		t.Run(fmt.Sprintf("rewrite cache=%v", rewriteCache), func(t *testing.T) {
			upstream, status := newStaleMetadataUpstream(t)
			h := newStaleMetadataHandler(t, upstream.URL, rewriteCache)
			primed := primeStaleMetadataCache(t, h)

			status.Store(http.StatusBadGateway)
			w := serveNPM(t, h, http.MethodGet, "/testpkg", nil, nil)

			if w.Code != http.StatusOK {
				t.Fatalf("stale request: status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			if w.Body.String() != primed {
				t.Errorf("stale body = %q, want the cached %q", w.Body.String(), primed)
			}
			if got := w.Header().Get("Warning"); got != `110 - "Response is Stale"` {
				t.Errorf("Warning = %q, want the stale warning", got)
			}
		})
	}
}

func TestNPMMetadataFreshResponseHasNoStaleWarning(t *testing.T) {
	upstream, _ := newStaleMetadataUpstream(t)
	h := newStaleMetadataHandler(t, upstream.URL, true)
	primed := primeStaleMetadataCache(t, h)

	w := serveNPM(t, h, http.MethodGet, "/testpkg", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != primed {
		t.Errorf("body = %q, want %q", w.Body.String(), primed)
	}
	if got := w.Header().Get("Warning"); got != "" {
		t.Errorf("Warning = %q, want none on a fresh response", got)
	}
}

// TestNPMMetadataPassesDefinitiveUpstreamAnswersThrough checks that an
// unpublished package or rejected credentials reach the client instead of the
// cached copy.
func TestNPMMetadataPassesDefinitiveUpstreamAnswersThrough(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			upstream, status := newStaleMetadataUpstream(t)
			h := newStaleMetadataHandler(t, upstream.URL, true)
			primed := primeStaleMetadataCache(t, h)

			status.Store(int32(code))
			w := serveNPM(t, h, http.MethodGet, "/testpkg", nil, nil)

			if w.Code != code {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, code, w.Body.String())
			}
			if w.Body.String() == primed {
				t.Error("the cached packument was served for a definitive upstream answer")
			}
			if got := w.Header().Get("Warning"); got != "" {
				t.Errorf("Warning = %q, want none", got)
			}
			var body map[string]any
			if err := json.NewDecoder(strings.NewReader(w.Body.String())).Decode(&body); err != nil {
				t.Fatalf("response body is not JSON: %v", err)
			}
			if _, ok := body["error"]; !ok {
				t.Errorf("response body %v missing \"error\" key", body)
			}
		})
	}
}

// TestNPMMetadataServesStaleOnRetryableStatus guards the offline fallback for
// rate limits and outages, which must still serve the cached copy.
func TestNPMMetadataServesStaleOnRetryableStatus(t *testing.T) {
	upstream, status := newStaleMetadataUpstream(t)
	h := newStaleMetadataHandler(t, upstream.URL, true)
	primed := primeStaleMetadataCache(t, h)

	status.Store(http.StatusServiceUnavailable)
	w := serveNPM(t, h, http.MethodGet, "/testpkg", nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != primed {
		t.Errorf("body = %q, want the cached %q", w.Body.String(), primed)
	}
	if got := w.Header().Get("Warning"); got != `110 - "Response is Stale"` {
		t.Errorf("Warning = %q, want the stale warning", got)
	}
}

// TestGenericMetadataKeepsStaleFallback guards the documented fallback for
// ecosystems other than npm (#486): a cached document is still served when
// upstream answers 404/401/403. Only npm relays those answers.
func TestGenericMetadataKeepsStaleFallback(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			upstream, status := newStaleMetadataUpstream(t)

			proxy, _, _, _ := setupTestProxy(t)
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Millisecond
			proxy.HTTPClient = http.DefaultClient

			upstreamURL := upstream.URL + "/testpkg"
			primed := primeGenericMetadataCache(t, proxy, upstreamURL)
			time.Sleep(5 * time.Millisecond)

			status.Store(int32(code))
			w := httptest.NewRecorder()
			proxy.ProxyCached(w, httptest.NewRequest(http.MethodGet, "/testpkg", nil), upstreamURL, "composer", "testpkg")

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 from the cached copy; body: %s", w.Code, w.Body.String())
			}
			if w.Body.String() != primed {
				t.Errorf("body = %q, want the cached %q", w.Body.String(), primed)
			}
			if got := w.Header().Get("Warning"); got != `110 - "Response is Stale"` {
				t.Errorf("Warning = %q, want the stale warning", got)
			}
		})
	}
}

// primeGenericMetadataCache fetches a metadata document once and returns the
// body, so a later request can be compared against it.
func primeGenericMetadataCache(t *testing.T, proxy *Proxy, upstreamURL string) string {
	t.Helper()
	w := httptest.NewRecorder()
	proxy.ProxyCached(w, httptest.NewRequest(http.MethodGet, "/testpkg", nil), upstreamURL, "composer", "testpkg")
	if w.Code != http.StatusOK {
		t.Fatalf("priming request: status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}
