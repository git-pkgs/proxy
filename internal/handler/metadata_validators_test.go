package handler

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// validatorUpstream serves one npm packument and one Composer document whose
// bodies a test can change, with a fixed Last-Modified.
type validatorUpstream struct {
	*httptest.Server
	mu           sync.Mutex
	npm          string
	composer     string
	lastModified time.Time
}

func newValidatorUpstream(t *testing.T) *validatorUpstream {
	t.Helper()
	u := &validatorUpstream{
		npm:          `{"name":"left-pad","versions":{"1.3.0":{"dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}}}}`,
		composer:     `{"packages":{"vendor/pkg":[{"version":"1.0.0","dist":{"type":"zip","url":"https://example.com/pkg-1.0.0.zip"}}]}}`,
		lastModified: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Last-Modified", u.lastModified.Format(http.TimeFormat))
		switch r.URL.Path {
		case "/left-pad":
			_, _ = w.Write([]byte(u.npm))
		case "/p2/vendor/pkg.json":
			_, _ = w.Write([]byte(u.composer))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *validatorUpstream) set(npm, composer string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.npm, u.composer = npm, composer
}

type validatorCase struct {
	ecosystem, path string
	handler         func(*Proxy, *validatorUpstream) http.Handler
}

var validatorCases = []validatorCase{
	{"npm", "/left-pad", func(p *Proxy, u *validatorUpstream) http.Handler {
		return NewNPMHandler(p, "http://proxy.example", u.URL).Routes()
	}},
	{"composer", "/p2/vendor/pkg.json", func(p *Proxy, u *validatorUpstream) http.Handler {
		return NewComposerHandlerWithUpstreams(p, "http://proxy.example", u.URL, u.URL).Routes()
	}},
}

func validatorProxy(t *testing.T, u *validatorUpstream, ttl time.Duration) *Proxy {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = u.Client()
	proxy.CacheMetadata = true
	proxy.MetadataTTL = ttl
	proxy.SetMetadataRewriteCacheSize(1 << 20)
	return proxy
}

func serve(handler http.Handler, path string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestRewrittenMetadataETag checks that rewritten metadata carries an ETag
// that clients can revalidate with, on the first fetch, on the rewrite fast
// path and when the TTL forces a revalidation.
func TestRewrittenMetadataETag(t *testing.T) {
	for _, c := range validatorCases {
		for _, ttl := range []time.Duration{time.Hour, 0} {
			t.Run(c.ecosystem+"/ttl="+ttl.String(), func(t *testing.T) {
				u := newValidatorUpstream(t)
				handler := c.handler(validatorProxy(t, u, ttl), u)

				first := serve(handler, c.path, nil)
				etag := first.Header().Get("ETag")
				if first.Code != http.StatusOK || etag == "" {
					t.Fatalf("first response: status %d, ETag %q; want 200 with an ETag", first.Code, etag)
				}
				for _, ifNoneMatch := range []string{etag, "W/" + etag, `"other", ` + etag} {
					expectNotModified(t, serve(handler, c.path, http.Header{"If-None-Match": {ifNoneMatch}}), etag)
				}
				if rec := serve(handler, c.path, nil); rec.Header().Get("ETag") != etag || rec.Body.String() != first.Body.String() {
					t.Errorf("repeat response: ETag %q, want %q, and the same body", rec.Header().Get("ETag"), etag)
				}
			})
		}
	}
}

func expectNotModified(t *testing.T, rec *httptest.ResponseRecorder, etag string) {
	t.Helper()
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("status %d with %d bytes, want an empty 304", rec.Code, rec.Body.Len())
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Errorf("304 ETag = %q, want %q", got, etag)
	}
}

// TestRewrittenMetadataETagFollowsUpstream checks that a new upstream
// document gets a new ETag, so a client's old copy no longer matches.
func TestRewrittenMetadataETagFollowsUpstream(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			handler := c.handler(validatorProxy(t, u, 0), u)
			etag := serve(handler, c.path, nil).Header().Get("ETag")

			u.set(
				`{"name":"left-pad","versions":{"1.3.1":{"dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.1.tgz"}}}}`,
				`{"packages":{"vendor/pkg":[{"version":"1.0.1","dist":{"type":"zip","url":"https://example.com/pkg-1.0.1.zip"}}]}}`,
			)
			rec := serve(handler, c.path, http.Header{"If-None-Match": {etag}})
			if rec.Code != http.StatusOK || rec.Header().Get("ETag") == etag {
				t.Errorf("status %d, ETag %q; want 200 with a new ETag", rec.Code, rec.Header().Get("ETag"))
			}
		})
	}
}

// TestRewrittenMetadataLastModified checks the validator Composer revalidates
// with. It is the upstream Last-Modified, but never earlier than the start of
// this process: a restart can change the rewrite without upstream changing.
func TestRewrittenMetadataLastModified(t *testing.T) {
	for _, c := range validatorCases {
		for _, ttl := range []time.Duration{time.Hour, 0} {
			t.Run(c.ecosystem+"/ttl="+ttl.String(), func(t *testing.T) {
				u := newValidatorUpstream(t)
				handler := c.handler(validatorProxy(t, u, ttl), u)

				first := serve(handler, c.path, nil)
				lastModified, err := http.ParseTime(first.Header().Get("Last-Modified"))
				if err != nil {
					t.Fatalf("Last-Modified %q: %v", first.Header().Get("Last-Modified"), err)
				}
				if lastModified.Before(processStartedAt.Truncate(time.Second)) {
					t.Errorf("Last-Modified %v is before the process started at %v", lastModified, processStartedAt)
				}

				current := serve(handler, c.path, http.Header{"If-Modified-Since": {first.Header().Get("Last-Modified")}})
				if current.Code != http.StatusNotModified || current.Body.Len() != 0 {
					t.Errorf("If-Modified-Since Last-Modified: status %d with %d bytes, want an empty 304", current.Code, current.Body.Len())
				}

				older := lastModified.Add(-time.Hour).Format(http.TimeFormat)
				if rec := serve(handler, c.path, http.Header{"If-Modified-Since": {older}}); rec.Code != http.StatusOK {
					t.Errorf("If-Modified-Since an hour earlier: status %d, want 200", rec.Code)
				}

				// If-None-Match takes precedence over If-Modified-Since.
				rec := serve(handler, c.path, http.Header{
					"If-None-Match":     {`"other"`},
					"If-Modified-Since": {first.Header().Get("Last-Modified")},
				})
				if rec.Code != http.StatusOK {
					t.Errorf("mismatched If-None-Match with a current If-Modified-Since: status %d, want 200", rec.Code)
				}
			})
		}
	}
}

// TestRewrittenMetadataValidatorsWithCooldown checks that with cooldown on,
// rewritten metadata keeps an exact ETag but has no Last-Modified: cooldown
// changes the rewrite as versions age in, while upstream stays the same.
func TestRewrittenMetadataValidatorsWithCooldown(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			proxy := validatorProxy(t, u, time.Hour)
			proxy.Cooldown = &cooldown.Config{Default: "3d"}
			handler := c.handler(proxy, u)

			first := serve(handler, c.path, nil)
			etag := first.Header().Get("ETag")
			if etag == "" {
				t.Fatal("no ETag with cooldown on")
			}
			if lm := first.Header().Get("Last-Modified"); lm != "" {
				t.Errorf("Last-Modified = %q with cooldown on, want none", lm)
			}
			if rec := serve(handler, c.path, http.Header{"If-None-Match": {etag}}); rec.Code != http.StatusNotModified {
				t.Errorf("If-None-Match: status %d, want 304", rec.Code)
			}
			future := time.Now().Add(time.Hour).Format(http.TimeFormat)
			if rec := serve(handler, c.path, http.Header{"If-Modified-Since": {future}}); rec.Code != http.StatusOK {
				t.Errorf("If-Modified-Since: status %d, want 200", rec.Code)
			}
		})
	}
}

// TestUnrewrittenMetadataHasNoValidators checks that metadata proxied as it
// is, because rewriting it failed, does not claim the rewrite's validators.
func TestUnrewrittenMetadataHasNoValidators(t *testing.T) {
	u := newValidatorUpstream(t)
	u.set(`{"name":"left-pad","versions":`, `{"packages":`)
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			rec := serve(c.handler(validatorProxy(t, u, time.Hour), u), c.path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if etag, lm := rec.Header().Get("ETag"), rec.Header().Get("Last-Modified"); etag != "" || lm != "" {
				t.Errorf("ETag %q, Last-Modified %q; want neither on unrewritten metadata", etag, lm)
			}
		})
	}
}

func TestRewriteCacheETag(t *testing.T) {
	c := newRewriteCache(1 << 20)
	exclaim := func(b []byte) ([]byte, error) { return []byte(string(b) + "!"), nil }
	key := rewriteCacheKey("npm", "http://proxy", "demo", []byte("doc"))

	out, etag, err := c.rewrite(t.Context(), key, []byte("doc"), exclaim)
	if err != nil || string(out) != "doc!" {
		t.Fatalf("rewrite = %q, %v", out, err)
	}
	if want := metadataETag([]byte("doc!")); etag != want {
		t.Errorf("rewrite ETag = %q, want %q", etag, want)
	}
	if _, cachedETag, ok := c.get(key); !ok || cachedETag != etag {
		t.Errorf("get ETag = %q, %v; want %q", cachedETag, ok, etag)
	}
	if metadataETag([]byte("other")) == etag {
		t.Error("different output has the same ETag")
	}
}
