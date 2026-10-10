package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/database"
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

func (u *validatorUpstream) setLastModified(lastModified time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastModified = lastModified
}

type validatorCase struct {
	ecosystem, name, path string
	handler               func(*Proxy, *validatorUpstream) http.Handler
}

var validatorCases = []validatorCase{
	{"npm", "left-pad", "/left-pad", func(p *Proxy, u *validatorUpstream) http.Handler {
		return NewNPMHandler(p, "http://proxy.example", u.URL).Routes()
	}},
	{"composer", "vendor/pkg", "/p2/vendor/pkg.json", func(p *Proxy, u *validatorUpstream) http.Handler {
		return NewComposerHandlerWithUpstreams(p, "http://proxy.example", u.URL, u.URL).Routes()
	}},
}

func validatorProxy(t *testing.T, u *validatorUpstream, ttl time.Duration) *Proxy {
	t.Helper()
	proxy, _ := validatorProxyWithDB(t, u, ttl)
	return proxy
}

func validatorProxyWithDB(t *testing.T, u *validatorUpstream, ttl time.Duration) (*Proxy, *database.DB) {
	t.Helper()
	proxy, db, _, _ := setupTestProxy(t)
	proxy.HTTPClient = u.Client()
	proxy.CacheMetadata = true
	proxy.MetadataTTL = ttl
	proxy.SetMetadataRewriteCacheSize(1 << 20)
	return proxy, db
}

// expireMetadata ages a metadata cache row past any TTL, so the next request
// refreshes it from upstream.
func expireMetadata(t *testing.T, db *database.DB, ecosystem, name string) {
	t.Helper()
	entry, err := db.GetMetadataCache(ecosystem, name)
	if err != nil || entry == nil {
		t.Fatalf("metadata row %s/%s = %v, %v", ecosystem, name, entry, err)
	}
	entry.FetchedAt.Time = time.Now().Add(-48 * time.Hour)
	if err := db.UpsertMetadataCache(entry); err != nil {
		t.Fatal(err)
	}
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
// with. It is when this process made the rewrite, not upstream's date: the
// rewrite also depends on configuration read at startup, so the same upstream
// document can be rewritten differently after a restart.
func TestRewrittenMetadataLastModified(t *testing.T) {
	for _, c := range validatorCases {
		for _, ttl := range []time.Duration{time.Hour, 0} {
			t.Run(c.ecosystem+"/ttl="+ttl.String(), func(t *testing.T) {
				u := newValidatorUpstream(t)
				handler := c.handler(validatorProxy(t, u, ttl), u)

				before := time.Now().Truncate(time.Second)
				first := serve(handler, c.path, nil)
				lastModified, err := http.ParseTime(first.Header().Get("Last-Modified"))
				if err != nil {
					t.Fatalf("Last-Modified %q: %v", first.Header().Get("Last-Modified"), err)
				}
				if lastModified.Before(before) || lastModified.After(time.Now()) {
					t.Errorf("Last-Modified %v, want the time of the rewrite, at or after %v", lastModified, before)
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

// TestRewrittenMetadataLastModifiedFollowsRefresh is a regression test for a
// false 304. Upstream dates both documents before this process started, the
// cached one and the newer one a refresh fetches, and neither may give the
// newer document a Last-Modified a client holding the older one already has:
// If-Modified-Since from the first response must fetch the new body.
func TestRewrittenMetadataLastModifiedFollowsRefresh(t *testing.T) {
	for _, c := range validatorCases {
		for _, ttl := range []time.Duration{time.Hour, 0} {
			t.Run(c.ecosystem+"/ttl="+ttl.String(), func(t *testing.T) {
				u := newValidatorUpstream(t)
				proxy, db := validatorProxyWithDB(t, u, ttl)
				handler := c.handler(proxy, u)

				first := serve(handler, c.path, nil)
				firstModified := first.Header().Get("Last-Modified")
				if first.Code != http.StatusOK || firstModified == "" {
					t.Fatalf("first response: status %d, Last-Modified %q; want 200 with a Last-Modified", first.Code, firstModified)
				}

				u.set(
					`{"name":"left-pad","versions":{"1.3.1":{"dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.1.tgz"}}}}`,
					`{"packages":{"vendor/pkg":[{"version":"1.0.1","dist":{"type":"zip","url":"https://example.com/pkg-1.0.1.zip"}}]}}`,
				)
				u.setLastModified(u.lastModified.Add(24 * time.Hour))
				expireMetadata(t, db, c.ecosystem, c.name)

				rec := serve(handler, c.path, http.Header{"If-Modified-Since": {firstModified}})
				if rec.Code != http.StatusOK || rec.Body.String() == first.Body.String() {
					t.Fatalf("If-Modified-Since from before the refresh: status %d, body changed %t; want 200 with the new body",
						rec.Code, rec.Body.String() != first.Body.String())
				}
				was, _ := http.ParseTime(firstModified)
				now, err := http.ParseTime(rec.Header().Get("Last-Modified"))
				if err != nil || !now.After(was) {
					t.Errorf("Last-Modified after the refresh = %q, want later than %q", rec.Header().Get("Last-Modified"), firstModified)
				}
				if rec := serve(handler, c.path, http.Header{"If-Modified-Since": {rec.Header().Get("Last-Modified")}}); rec.Code != http.StatusNotModified {
					t.Errorf("If-Modified-Since the new Last-Modified: status %d, want 304", rec.Code)
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
	if want := metadataETag([]byte("doc!")); etag.etag != want {
		t.Errorf("rewrite ETag = %q, want %q", etag.etag, want)
	}
	if _, cached, ok := c.get(key); !ok || cached != etag {
		t.Errorf("get validators = %+v, %v; want %+v", cached, ok, etag)
	}
	if metadataETag([]byte("other")) == etag.etag {
		t.Error("different output has the same ETag")
	}
}

// TestRewriteCacheLastModified checks the Last-Modified the rewrite cache
// gives each rewrite: the second it was made, kept while it stays cached, and
// always later than any earlier rewrite of the same package, so that a client
// holding one representation never gets 304 for another.
func TestRewriteCacheLastModified(t *testing.T) {
	c := newRewriteCache(8)
	clock := time.Date(2026, 10, 10, 12, 0, 0, 400_000_000, time.UTC)
	c.now = func() time.Time { return clock }
	second := clock.Truncate(time.Second)
	identity := func(b []byte) ([]byte, error) { return b, nil }
	rewrite := func(name, doc string) time.Time {
		t.Helper()
		_, v, err := c.rewrite(t.Context(), rewriteCacheKey("npm", "http://proxy", name, []byte(doc)), []byte(doc), identity)
		if err != nil {
			t.Fatal(err)
		}
		return v.lastModified
	}

	if got := rewrite("demo", "a"); !got.Equal(second) {
		t.Errorf("first rewrite: Last-Modified %v, want %v", got, second)
	}
	clock = clock.Add(300 * time.Millisecond)
	if got := rewrite("demo", "a"); !got.Equal(second) {
		t.Errorf("cached rewrite: Last-Modified %v, want %v as before", got, second)
	}

	// A new document in the same second must not share the date.
	b := rewrite("demo", "b")
	if !b.After(second) {
		t.Errorf("new document in the same second: Last-Modified %v, want later than %v", b, second)
	}
	// Another package is not held back by it.
	if got := rewrite("other", "x"); !got.Equal(second) {
		t.Errorf("other package: Last-Modified %v, want %v", got, second)
	}

	// Evicting what came before does not let a later rewrite reuse its date.
	for _, doc := range []string{"ccc", "ddd", "eee"} {
		rewrite("filler", doc)
	}
	if _, _, ok := c.get(rewriteCacheKey("npm", "http://proxy", "demo", []byte("b"))); ok {
		t.Fatal("expected b to be evicted")
	}
	if got := rewrite("demo", "b"); !got.After(b) {
		t.Errorf("rewrite after eviction: Last-Modified %v, want later than %v", got, b)
	}

	// Once the clock moves on, dates follow it again instead of drifting ahead.
	clock = clock.Add(10 * time.Second)
	if got, want := rewrite("demo", "f"), clock.Truncate(time.Second); !got.Equal(want) {
		t.Errorf("later rewrite: Last-Modified %v, want %v", got, want)
	}
}

// TestRewrittenMetadataLastModifiedNeedsRewriteCache checks that without the
// rewrite cache there is no Last-Modified: nothing records when a rewrite was
// first made, so no date can be relied on to change with it.
func TestRewrittenMetadataLastModifiedNeedsRewriteCache(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			proxy := validatorProxy(t, u, time.Hour)
			proxy.SetMetadataRewriteCacheSize(0)
			rec := serve(c.handler(proxy, u), c.path, nil)
			if rec.Code != http.StatusOK || rec.Header().Get("ETag") == "" {
				t.Fatalf("status %d, ETag %q; want 200 with an ETag", rec.Code, rec.Header().Get("ETag"))
			}
			if lm := rec.Header().Get("Last-Modified"); lm != "" {
				t.Errorf("Last-Modified = %q without the rewrite cache, want none", lm)
			}
		})
	}
}

// cooldownUpstream sets documents with one version published long ago and
// one published two hours ago, so a cooldown of a day hides the recent one
// and a cooldown of an hour keeps it.
func cooldownUpstream(t *testing.T) *validatorUpstream {
	t.Helper()
	recent := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	u := newValidatorUpstream(t)
	u.set(
		`{"name":"left-pad","dist-tags":{"latest":"1.4.0"},"versions":{`+
			`"1.3.0":{"dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}},`+
			`"1.4.0":{"dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-1.4.0.tgz"}}},`+
			`"time":{"1.3.0":"2016-01-01T00:00:00Z","1.4.0":"`+recent+`"}}`,
		`{"packages":{"vendor/pkg":[`+
			`{"version":"1.0.0","time":"2016-01-01T00:00:00Z","dist":{"type":"zip","url":"https://example.com/pkg-1.0.0.zip"}},`+
			`{"version":"1.1.0","time":"`+recent+`","dist":{"type":"zip","url":"https://example.com/pkg-1.1.0.zip"}}]}}`,
	)
	return u
}

// TestRewrittenMetadataETagWithCooldownFollowsKeptVersions checks that with
// cooldown on, the ETag changes when a version leaves cooldown although the
// upstream document stays the same, and comes back when the same versions do.
func TestRewrittenMetadataETagWithCooldownFollowsKeptVersions(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := cooldownUpstream(t)
			proxy := validatorProxy(t, u, time.Hour)
			handler := c.handler(proxy, u)

			proxy.Cooldown = &cooldown.Config{Default: "1d"}
			etag := serve(handler, c.path, nil).Header().Get("ETag")

			proxy.Cooldown = &cooldown.Config{Default: "1h"}
			rec := serve(handler, c.path, http.Header{"If-None-Match": {etag}})
			if rec.Code != http.StatusOK || rec.Header().Get("ETag") == etag {
				t.Errorf("after a version left cooldown: status %d, ETag %q; want 200 with a new ETag", rec.Code, rec.Header().Get("ETag"))
			}

			proxy.Cooldown = &cooldown.Config{Default: "1d"}
			expectNotModified(t, serve(handler, c.path, http.Header{"If-None-Match": {etag}}), etag)
		})
	}
}

// TestRewrittenMetadataETagWithCooldownComesFromInputs checks that with
// cooldown on, the ETag is derived from what fixes the rewrite rather than
// from the rewritten body, when the metadata cache knows the upstream
// digest, and falls back to the body when it does not.
func TestRewrittenMetadataETagWithCooldownComesFromInputs(t *testing.T) {
	for _, c := range validatorCases {
		for _, cacheMetadata := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/cacheMetadata=%t", c.ecosystem, cacheMetadata), func(t *testing.T) {
				u := cooldownUpstream(t)
				proxy := validatorProxy(t, u, time.Hour)
				proxy.CacheMetadata = cacheMetadata
				proxy.Cooldown = &cooldown.Config{Default: "1d"}
				handler := c.handler(proxy, u)

				rec := serve(handler, c.path, nil)
				bodyETag := metadataETag(rec.Body.Bytes())
				if got := rec.Header().Get("ETag"); (got == bodyETag) == cacheMetadata {
					t.Errorf("ETag %q, body hash %q; want them to differ only with a known digest", got, bodyETag)
				}
				etag := rec.Header().Get("ETag")
				expectNotModified(t, serve(handler, c.path, http.Header{"If-None-Match": {etag}}), etag)
			})
		}
	}
}

func TestKeptVersionsETag(t *testing.T) {
	base := keptVersionsETag("sha256:abc", "http://proxy", "left-pad", []string{"1.0.0", "1.1.0"})
	if got := keptVersionsETag("sha256:abc", "http://proxy", "left-pad", []string{"1.1.0", "1.0.0"}); got != base {
		t.Errorf("kept versions in another order: ETag %q, want %q", got, base)
	}
	for name, etag := range map[string]string{
		"digest":        keptVersionsETag("sha256:abd", "http://proxy", "left-pad", []string{"1.0.0", "1.1.0"}),
		"proxy URL":     keptVersionsETag("sha256:abc", "http://other", "left-pad", []string{"1.0.0", "1.1.0"}),
		"name":          keptVersionsETag("sha256:abc", "http://proxy", "right-pad", []string{"1.0.0", "1.1.0"}),
		"kept versions": keptVersionsETag("sha256:abc", "http://proxy", "left-pad", []string{"1.0.0"}),
		"field bounds":  keptVersionsETag("sha256:abc", "http://proxy", "left-pad", []string{"1.0.0\x001.1.0"}),
	} {
		if etag == base {
			t.Errorf("a different %s gives the same ETag", name)
		}
	}
	if !strings.HasPrefix(base, `"`) || !strings.HasSuffix(base, `"`) {
		t.Errorf("ETag %q is not a quoted strong validator", base)
	}
}
