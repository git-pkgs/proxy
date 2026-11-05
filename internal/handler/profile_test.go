package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

func profilePolicy(t *testing.T, duration string) *cooldown.Policy {
	t.Helper()
	policy, err := cooldown.NewPolicy(&cooldown.Config{Default: duration}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestProfilesCoalesceUncachedMetadata(t *testing.T) {
	u := newMetadataUpstream(t, http.StatusOK)
	p := metadataTestProxy(t, u)
	views := []*Proxy{p.WithCooldown(profilePolicy(t, "3d"), 72*time.Hour), p.WithCooldown(profilePolicy(t, "7d"), 168*time.Hour)}
	first := startMetadataCalls(views[0], context.Background(), 1, u.URL, contentTypeJSON)
	<-u.entered
	second := startMetadataCalls(views[1], context.Background(), 1, u.URL, contentTypeJSON)
	key := metadataCoalesceKey("npm", "left-pad", u.URL, contentTypeJSON, "", false)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(u.release) }) })
	waitForMetadataWaiters(t, p, key, 1)
	release.Do(func() { close(u.release) })
	for _, calls := range [][]chan metadataCall{first, second} {
		result := <-calls[0]
		if result.err != nil || !strings.Contains(string(result.body), "accept") {
			t.Fatalf("metadata = %s, error %v", result.body, result.err)
		}
	}
	if n := u.calls.Load(); n != 1 {
		t.Fatalf("upstream requests = %d, want 1", n)
	}
}

func TestProfilesCoalesceArtifactRequests(t *testing.T) {
	proxy, db, _, _ := setupTestProxy(t)
	if err := db.SetVersionPublishedAt("pkg:cargo/demo@1.0.0", "pkg:cargo/demo", time.Now().Add(-10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	fetcher := &countingFetcher{content: "crate bytes", delay: fetchHoldTime}
	proxy.Fetcher = fetcher
	views := []*Proxy{proxy.WithCooldown(profilePolicy(t, "3d"), 72*time.Hour), proxy.WithCooldown(profilePolicy(t, "7d"), 168*time.Hour)}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		for _, view := range views {
			wg.Go(func() {
				<-start
				h := NewCargoHandler(view, "http://proxy.test", "https://index.crates.io", "https://static.crates.io/crates")
				w := httptest.NewRecorder()
				h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/crates/demo/1.0.0/download", nil))
				if w.Code != http.StatusOK || w.Body.String() != "crate bytes" {
					t.Errorf("artifact response %d: %s", w.Code, w.Body)
				}
			})
		}
	}
	close(start)
	wg.Wait()
	if n := fetcher.calls.Load(); n != 1 {
		t.Fatalf("upstream fetches = %d, want 1", n)
	}
}

type profileClockPolicy struct {
	*cooldown.Policy
	now func() time.Time
}

func (p profileClockPolicy) IsAllowed(ecosystem, packagePURL string, publishedAt time.Time) bool {
	return p.Evaluate(ecosystem, packagePURL, publishedAt, p.now()).Allowed
}

func TestProfileExpiryReleasesVersionsIndependently(t *testing.T) {
	u, proxy, _ := gemCacheSetup(t)
	clock := &testClock{t: time.Now()}
	proxy.now = clock.now
	proxy.SetMetadataRewriteCacheSize(1 << 20)
	view := proxy.WithCooldown(profileClockPolicy{profilePolicy(t, "3d"), clock.now}, 72*time.Hour)
	h := NewGemHandlerWithUpstream(view, "http://proxy.test/cooldown/3d", u.URL).Routes()
	strictView := proxy.WithCooldown(profileClockPolicy{profilePolicy(t, "7d"), clock.now}, 168*time.Hour)
	strict := NewGemHandlerWithUpstream(strictView, "http://proxy.test/cooldown/7d", u.URL).Routes()
	first := getWith(t, h, gemInfoPath, nil)
	if first.Code != http.StatusOK || first.Body.String() != gemFiltered {
		t.Fatalf("initial response %d: %s", first.Code, first.Body)
	}
	if first := getWith(t, strict, gemInfoPath, nil); first.Code != http.StatusOK || first.Body.String() != gemFiltered {
		t.Fatalf("initial strict response %d: %s", first.Code, first.Body)
	}
	clock.advance(4 * 24 * time.Hour)
	second := getWith(t, h, gemInfoPath, nil)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "2.0.0 |") {
		t.Fatalf("version did not leave cooldown: %d %s", second.Code, second.Body)
	}
	if second := getWith(t, strict, gemInfoPath, nil); second.Code != http.StatusOK || second.Body.String() != gemFiltered {
		t.Fatalf("strict profile released version early: %d %s", second.Code, second.Body)
	}
	if n, _ := u.counts(gemInfoPath); n != 1 {
		t.Errorf("expiry refetched raw metadata %d times", n)
	}
}
