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
	proxy, _, _ := validatorProxyWithDB(t, u, ttl)
	return proxy
}

// validatorProxyWithDB returns a proxy that dates metadata by a clock the
// test moves, starting a second after the process start it models.
func validatorProxyWithDB(t *testing.T, u *validatorUpstream, ttl time.Duration) (*Proxy, *database.DB, *testClock) {
	t.Helper()
	proxy, db, _, _ := setupTestProxy(t)
	proxy.HTTPClient = u.Client()
	proxy.CacheMetadata = true
	proxy.MetadataTTL = ttl
	clock := &testClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	restartProxy(proxy, clock)
	clock.advance(time.Second)
	return proxy, db, clock
}

// restartProxy gives proxy a new, empty rewrite cache and makes it date
// metadata by clock from the clock's current time on, as a restart of the
// process would. What the database holds survives.
func restartProxy(proxy *Proxy, clock *testClock) {
	proxy.SetMetadataRewriteCacheSize(1 << 20)
	proxy.now = clock.now
	proxy.started = clock.now()
}

// validatorReplica returns a second proxy process sharing proxy's database
// and storage, as another replica behind the same load balancer would, with
// its own rewrite cache and its own clock.
func validatorReplica(proxy *Proxy, clock *testClock) *Proxy {
	replica := NewProxy(proxy.DB, proxy.Storage, proxy.Fetcher, proxy.Resolver, proxy.Logger)
	replica.HTTPClient = proxy.HTTPClient
	replica.CacheMetadata = proxy.CacheMetadata
	replica.MetadataTTL = proxy.MetadataTTL
	replica.SetMetadataRewriteCacheSize(1 << 20)
	replica.now = clock.now
	replica.started = proxy.started
	return replica
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// npmV and composerV are upstream documents with a single version v.
func npmV(v string) string {
	return `{"name":"left-pad","versions":{"` + v + `":{"dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-` + v + `.tgz"}}}}`
}

func composerV(v string) string {
	return `{"packages":{"vendor/pkg":[{"version":"` + v + `","dist":{"type":"zip","url":"https://example.com/pkg-` + v + `.zip"}}]}}`
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
// with. It is when this process started serving the rewrite, not upstream's
// date: the rewrite also depends on configuration read at startup, so the
// same upstream document can be rewritten differently after a restart. It
// stays the same while the rewrite does.
func TestRewrittenMetadataLastModified(t *testing.T) {
	for _, c := range validatorCases {
		for _, ttl := range []time.Duration{time.Hour, 0} {
			t.Run(c.ecosystem+"/ttl="+ttl.String(), func(t *testing.T) {
				u := newValidatorUpstream(t)
				proxy, _, clock := validatorProxyWithDB(t, u, ttl)
				handler := c.handler(proxy, u)

				clock.advance(400 * time.Millisecond)
				first := serve(handler, c.path, nil)
				lastModified, err := http.ParseTime(first.Header().Get("Last-Modified"))
				if err != nil {
					t.Fatalf("Last-Modified %q: %v", first.Header().Get("Last-Modified"), err)
				}
				if want := clock.now().Truncate(time.Second); !lastModified.Equal(want) {
					t.Errorf("Last-Modified %v, want %v, the second of the rewrite", lastModified, want)
				}
				clock.advance(time.Hour)

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
				proxy, db, clock := validatorProxyWithDB(t, u, ttl)
				handler := c.handler(proxy, u)

				first := serve(handler, c.path, nil)
				firstModified := first.Header().Get("Last-Modified")
				if first.Code != http.StatusOK || firstModified == "" {
					t.Fatalf("first response: status %d, Last-Modified %q; want 200 with a Last-Modified", first.Code, firstModified)
				}

				clock.advance(time.Minute)
				u.set(npmV("1.3.1"), composerV("1.0.1"))
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

// TestRewrittenMetadataLastModifiedReturningDocument is a regression test
// for a false 304. Upstream serves A, then B, then A again; A's cached
// rewrite must not bring back the date it was first served with, which is
// earlier than B's, or a client holding B gets 304 for A.
func TestRewrittenMetadataLastModifiedReturningDocument(t *testing.T) {
	for _, c := range validatorCases {
		for _, ttl := range []time.Duration{time.Hour, 0} {
			t.Run(c.ecosystem+"/ttl="+ttl.String(), func(t *testing.T) {
				u := newValidatorUpstream(t)
				proxy, db, clock := validatorProxyWithDB(t, u, ttl)
				handler := c.handler(proxy, u)
				change := func(v string) {
					clock.advance(10 * time.Second)
					u.set(npmV(v), composerV(v))
					if ttl > 0 {
						expireMetadata(t, db, c.ecosystem, c.name)
					}
				}

				u.set(npmV("1.0.0"), composerV("1.0.0"))
				a := serve(handler, c.path, nil)
				change("2.0.0")
				b := serve(handler, c.path, nil)
				if b.Body.String() == a.Body.String() || b.Header().Get("Last-Modified") == "" {
					t.Fatalf("B: Last-Modified %q, body changed %t; want a new body with a date", b.Header().Get("Last-Modified"), b.Body.String() != a.Body.String())
				}
				change("1.0.0")

				rec := serve(handler, c.path, http.Header{"If-Modified-Since": {b.Header().Get("Last-Modified")}})
				if rec.Code != http.StatusOK || rec.Body.String() != a.Body.String() {
					t.Fatalf("A again, If-Modified-Since B's date: status %d, body is A %t; want 200 with A", rec.Code, rec.Body.String() == a.Body.String())
				}
				was, _ := http.ParseTime(b.Header().Get("Last-Modified"))
				if now, err := http.ParseTime(rec.Header().Get("Last-Modified")); err != nil || !now.After(was) {
					t.Errorf("A again: Last-Modified %q, want later than B's %q", rec.Header().Get("Last-Modified"), b.Header().Get("Last-Modified"))
				}
			})
		}
	}
}

// TestRewrittenMetadataLastModifiedSameSecond checks that a document that
// replaces another within the second the other was dated gets no date of
// its own until a later second, rather than one ahead of the clock. The
// clock-time date it would share is one a client may already hold.
func TestRewrittenMetadataLastModifiedSameSecond(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			proxy, _, clock := validatorProxyWithDB(t, u, 0)
			handler := c.handler(proxy, u)

			a := serve(handler, c.path, nil)
			aModified := a.Header().Get("Last-Modified")
			u.set(npmV("2.0.0"), composerV("2.0.0"))
			for i, header := range []http.Header{nil, {"If-Modified-Since": {aModified}}} {
				rec := serve(handler, c.path, header)
				if rec.Code != http.StatusOK || rec.Body.String() == a.Body.String() {
					t.Fatalf("request %d for B: status %d; want 200 with B", i, rec.Code)
				}
				if lm := rec.Header().Get("Last-Modified"); lm != "" {
					t.Errorf("request %d for B in A's second: Last-Modified %q, want none", i, lm)
				}
			}

			clock.advance(time.Second)
			rec := serve(handler, c.path, nil)
			lastModified, err := http.ParseTime(rec.Header().Get("Last-Modified"))
			if want := clock.now().Truncate(time.Second); err != nil || !lastModified.Equal(want) {
				t.Errorf("B a second later: Last-Modified %q, want %v", rec.Header().Get("Last-Modified"), want)
			}
		})
	}
}

// TestRewrittenMetadataLastModifiedAcrossRestart is a regression test for a
// false 304 after a restart. Dates are never ahead of the clock, and the
// restarted process dates nothing in the second it starts, the last in which
// the previous process may have dated a document.
func TestRewrittenMetadataLastModifiedAcrossRestart(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			proxy, _, clock := validatorProxyWithDB(t, u, 0)
			handler := c.handler(proxy, u)

			// Two documents in one second; the client keeps the latest date.
			latest := serve(handler, c.path, nil).Header().Get("Last-Modified")
			u.set(npmV("2.0.0"), composerV("2.0.0"))
			if lm := serve(handler, c.path, nil).Header().Get("Last-Modified"); lm != "" {
				latest = lm
			}
			if held, _ := http.ParseTime(latest); held.After(clock.now()) {
				t.Errorf("Last-Modified %q is ahead of the clock at %v", latest, clock.now())
			}

			restartProxy(proxy, clock)
			u.set(npmV("3.0.0"), composerV("3.0.0"))
			rec := serve(handler, c.path, http.Header{"If-Modified-Since": {latest}})
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "3.0.0") {
				t.Fatalf("changed document after a restart, If-Modified-Since %q: status %d; want 200 with it", latest, rec.Code)
			}
			if lm := rec.Header().Get("Last-Modified"); lm != "" {
				t.Errorf("Last-Modified %q in the second the process started, want none", lm)
			}

			clock.advance(time.Second)
			rec = serve(handler, c.path, http.Header{"If-Modified-Since": {latest}})
			if rec.Code != http.StatusOK || rec.Header().Get("Last-Modified") == "" {
				t.Errorf("a second after the restart: status %d, Last-Modified %q; want 200 with a date", rec.Code, rec.Header().Get("Last-Modified"))
			}
		})
	}
}

// TestRewrittenMetadataValidatorsWithCooldown checks that with cooldown on,
// rewritten metadata from the rewrite cache has an ETag and a Last-Modified,
// so clients that only send If-Modified-Since, such as Composer, get 304.
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
			lm := first.Header().Get("Last-Modified")
			if lm == "" {
				t.Fatal("no Last-Modified with cooldown on")
			}
			if rec := serve(handler, c.path, http.Header{"If-None-Match": {etag}}); rec.Code != http.StatusNotModified {
				t.Errorf("If-None-Match: status %d, want 304", rec.Code)
			}
			if rec := serve(handler, c.path, http.Header{"If-Modified-Since": {lm}}); rec.Code != http.StatusNotModified {
				t.Errorf("If-Modified-Since: status %d, want 304", rec.Code)
			}
		})
	}
}

// TestRewrittenMetadataWithCooldownIsGzipped checks that with cooldown on,
// clients accepting gzip get the cached compressed copy.
func TestRewrittenMetadataWithCooldownIsGzipped(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			compressibleDocuments(u)
			proxy := validatorProxy(t, u, time.Hour)
			proxy.Cooldown = &cooldown.Config{Default: "3d"}
			handler := c.handler(proxy, u)
			for range 2 {
				if rec := serve(handler, c.path, http.Header{"Accept-Encoding": {"gzip"}}); rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
					t.Errorf("status %d, Content-Encoding %q; want 200 gzip", rec.Code, rec.Header().Get("Content-Encoding"))
				}
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

	m, err := c.rewrite(t.Context(), key, []byte("doc"), 0, exclaim)
	if err != nil || string(m.content()) != "doc!" {
		t.Fatalf("rewrite = %q, %v", m.content(), err)
	}
	if want := metadataETag([]byte("doc!")); m.etag != want {
		t.Errorf("rewrite ETag = %q, want %q", m.etag, want)
	}
	if cached, ok := c.get(key); !ok || cached.metadataValidators != m.metadataValidators {
		t.Errorf("get validators = %+v, %v; want %+v", cached.metadataValidators, ok, m.metadataValidators)
	}
	if metadataETag([]byte("other")) == m.etag {
		t.Error("different output has the same ETag")
	}
}

// TestRewrittenMetadataLastModifiedWithoutRewriteCache checks that dates do
// not depend on the rewrite cache: the database records them.
func TestRewrittenMetadataLastModifiedWithoutRewriteCache(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			proxy, _, clock := validatorProxyWithDB(t, u, time.Hour)
			proxy.SetMetadataRewriteCacheSize(0)
			handler := c.handler(proxy, u)

			first := serve(handler, c.path, nil)
			if first.Code != http.StatusOK || first.Header().Get("ETag") == "" {
				t.Fatalf("status %d, ETag %q; want 200 with an ETag", first.Code, first.Header().Get("ETag"))
			}
			lastModified, err := http.ParseTime(first.Header().Get("Last-Modified"))
			if want := clock.now().Truncate(time.Second); err != nil || !lastModified.Equal(want) {
				t.Errorf("Last-Modified %q without the rewrite cache, want %v", first.Header().Get("Last-Modified"), want)
			}
			clock.advance(time.Minute)
			if rec := serve(handler, c.path, http.Header{"If-Modified-Since": {first.Header().Get("Last-Modified")}}); rec.Code != http.StatusNotModified {
				t.Errorf("If-Modified-Since Last-Modified: status %d, want 304", rec.Code)
			}
		})
	}
}

// TestRewrittenMetadataLastModifiedAcrossReplicas is a regression test for a
// false 304 between replicas sharing a database and storage. One replica
// dates a document, upstream changes in that second, and a client holding
// the date asks another replica: the date must not suppress the new body.
// Once a second has passed, both replicas date the new document alike.
func TestRewrittenMetadataLastModifiedAcrossReplicas(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			a, _, clock := validatorProxyWithDB(t, u, 0)
			b := validatorReplica(a, clock)
			replicas := map[string]http.Handler{"A": c.handler(a, u), "B": c.handler(b, u)}

			first := serve(replicas["A"], c.path, nil)
			held := first.Header().Get("Last-Modified")
			if first.Code != http.StatusOK || held == "" {
				t.Fatalf("replica A: status %d, Last-Modified %q; want 200 with a date", first.Code, held)
			}
			u.set(npmV("2.0.0"), composerV("2.0.0"))
			for _, name := range []string{"B", "A"} {
				expectChangedUndated(t, name, serve(replicas[name], c.path, http.Header{"If-Modified-Since": {held}}), first)
			}

			clock.advance(time.Second)
			shared := expectSharedDate(t, replicas, c.path, held)
			for name, handler := range replicas {
				if rec := serve(handler, c.path, http.Header{"If-Modified-Since": {shared}}); rec.Code != http.StatusNotModified {
					t.Errorf("replica %s, If-Modified-Since the shared date: status %d, want 304", name, rec.Code)
				}
				if rec := serve(handler, c.path, http.Header{"If-Modified-Since": {held}}); rec.Code != http.StatusOK {
					t.Errorf("replica %s, If-Modified-Since the old date: status %d, want 200", name, rec.Code)
				}
			}
		})
	}
}

// expectChangedUndated checks that a replica answered a conditional request
// with a body and ETag other than first's, and no date: the second first was
// dated in may not date another representation.
func expectChangedUndated(t *testing.T, replica string, rec *httptest.ResponseRecorder, first *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Body.String() == first.Body.String() {
		t.Fatalf("replica %s after upstream changed: status %d; want 200 with the new body", replica, rec.Code)
	}
	if rec.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Errorf("replica %s: the new body has the old ETag", replica)
	}
	if lm := rec.Header().Get("Last-Modified"); lm != "" {
		t.Errorf("replica %s: Last-Modified %q in the second the old body was dated, want none", replica, lm)
	}
}

// expectSharedDate checks that every replica serves the same Last-Modified,
// later than held, and returns it.
func expectSharedDate(t *testing.T, replicas map[string]http.Handler, path, held string) string {
	t.Helper()
	was, _ := http.ParseTime(held)
	shared := ""
	for name, handler := range replicas {
		lm := serve(handler, path, nil).Header().Get("Last-Modified")
		if now, err := http.ParseTime(lm); err != nil || !now.After(was) {
			t.Fatalf("replica %s: Last-Modified %q, want later than %q", name, lm, held)
		}
		if shared != "" && lm != shared {
			t.Fatalf("replica %s: Last-Modified %q, another replica serves %q", name, lm, shared)
		}
		shared = lm
	}
	return shared
}

// TestRewrittenMetadataLastModifiedFromBeforeTheFix checks that dates clients
// hold from before dates were shared cannot suppress a body. Those were never
// later than the clock of the process that gave them, which stopped before
// this one started, so no date is given in the second this process started
// and every later one is later than any of them.
func TestRewrittenMetadataLastModifiedFromBeforeTheFix(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			a, _, clock := validatorProxyWithDB(t, u, 0)
			restartProxy(a, clock)
			b := validatorReplica(a, clock)
			held := clock.now().Truncate(time.Second).Format(http.TimeFormat)

			for name, handler := range map[string]http.Handler{"A": c.handler(a, u), "B": c.handler(b, u)} {
				rec := serve(handler, c.path, http.Header{"If-Modified-Since": {held}})
				if rec.Code != http.StatusOK || rec.Header().Get("Last-Modified") != "" {
					t.Errorf("replica %s in its first second, If-Modified-Since %q: status %d, Last-Modified %q; want 200 without a date",
						name, held, rec.Code, rec.Header().Get("Last-Modified"))
				}
			}

			clock.advance(time.Second)
			for name, handler := range map[string]http.Handler{"A": c.handler(a, u), "B": c.handler(b, u)} {
				rec := serve(handler, c.path, http.Header{"If-Modified-Since": {held}})
				was, _ := http.ParseTime(held)
				now, err := http.ParseTime(rec.Header().Get("Last-Modified"))
				if rec.Code != http.StatusOK || err != nil || !now.After(was) {
					t.Errorf("replica %s a second later, If-Modified-Since %q: status %d, Last-Modified %q; want 200 with a later date",
						name, held, rec.Code, rec.Header().Get("Last-Modified"))
				}
			}
		})
	}
}

// TestRewrittenMetadataLastModifiedWithClockSkew checks that a replica whose
// clock is behind gives no date ahead of its own clock, neither for a new
// document nor for one another replica dated.
func TestRewrittenMetadataLastModifiedWithClockSkew(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			a, _, clock := validatorProxyWithDB(t, u, 0)
			behind := &testClock{t: clock.now().Add(-2 * time.Second)}
			b := validatorReplica(a, behind)
			handlerA, handlerB := c.handler(a, u), c.handler(b, u)

			held := serve(handlerA, c.path, nil).Header().Get("Last-Modified")
			u.set(npmV("2.0.0"), composerV("2.0.0"))
			if rec := serve(handlerB, c.path, http.Header{"If-Modified-Since": {held}}); rec.Code != http.StatusOK || rec.Header().Get("Last-Modified") != "" {
				t.Fatalf("lagging replica, new document: status %d, Last-Modified %q; want 200 without a date", rec.Code, rec.Header().Get("Last-Modified"))
			}

			clock.advance(time.Second)
			dated := serve(handlerA, c.path, nil).Header().Get("Last-Modified")
			if dated == "" {
				t.Fatal("replica A gave no date a second later")
			}
			if lm := serve(handlerB, c.path, nil).Header().Get("Last-Modified"); lm != "" {
				t.Errorf("lagging replica: Last-Modified %q ahead of its clock at %v, want none", lm, behind.now())
			}
			behind.advance(3 * time.Second)
			if lm := serve(handlerB, c.path, nil).Header().Get("Last-Modified"); lm != dated {
				t.Errorf("lagging replica once its clock caught up: Last-Modified %q, want %q", lm, dated)
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
// upstream document stays the same, once the cached rewrite has expired, and
// comes back when the same versions do.
func TestRewrittenMetadataETagWithCooldownFollowsKeptVersions(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := cooldownUpstream(t)
			proxy, _, clock := validatorProxyWithDB(t, u, time.Hour)
			handler := c.handler(proxy, u)

			proxy.Cooldown = &cooldown.Config{Default: "1d"}
			etag := serve(handler, c.path, nil).Header().Get("ETag")

			proxy.Cooldown = &cooldown.Config{Default: "1h"}
			if rec := serve(handler, c.path, http.Header{"If-None-Match": {etag}}); rec.Code != http.StatusNotModified {
				t.Errorf("within the rewrite's lifetime: status %d, want 304 from the cached rewrite", rec.Code)
			}
			clock.advance(maxCooldownRewriteTTL)
			rec := serve(handler, c.path, http.Header{"If-None-Match": {etag}})
			if rec.Code != http.StatusOK || rec.Header().Get("ETag") == etag {
				t.Errorf("after a version left cooldown: status %d, ETag %q; want 200 with a new ETag", rec.Code, rec.Header().Get("ETag"))
			}

			proxy.Cooldown = &cooldown.Config{Default: "1d"}
			clock.advance(maxCooldownRewriteTTL)
			expectNotModified(t, serve(handler, c.path, http.Header{"If-None-Match": {etag}}), etag)
		})
	}
}

// TestRewrittenMetadataETagWithCooldownComesFromInputs checks that with
// cooldown on and no rewrite cache, the ETag is derived from what fixes the
// rewrite rather than from the rewritten body, when the metadata cache knows
// the upstream digest, and falls back to the body when it does not.
func TestRewrittenMetadataETagWithCooldownComesFromInputs(t *testing.T) {
	for _, c := range validatorCases {
		for _, cacheMetadata := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/cacheMetadata=%t", c.ecosystem, cacheMetadata), func(t *testing.T) {
				u := cooldownUpstream(t)
				proxy := validatorProxy(t, u, time.Hour)
				proxy.CacheMetadata = cacheMetadata
				proxy.SetMetadataRewriteCacheSize(0)
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
