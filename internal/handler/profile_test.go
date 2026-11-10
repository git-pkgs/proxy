package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/denylist"
	"github.com/git-pkgs/proxy/internal/scanner"
	registryclient "github.com/git-pkgs/registries/client"
)

func TestWithCooldownCopiesProxyFields(t *testing.T) {
	p, _, _, _ := setupTestProxy(t)
	p.RouteFor = func(string, string) *ArtifactRoute { return nil }
	p.Cooldown = profilePolicy(t, "1d")
	p.ShortestCooldown = 24 * time.Hour
	p.Denylist = &denylist.Policy{}
	p.CacheMetadata = true
	p.MetadataTTL = time.Hour
	p.MetadataMaxSize = 1024
	p.MetadataStreamMaxSize = 2048
	p.GradleReadOnly = true
	p.HostedEnabled = true
	p.GradleMaxUploadSize = 4096
	p.NPMFullMetadata = true
	p.DirectServe = true
	p.DirectServeTTL = time.Minute
	p.DirectServeBaseURL = "https://storage.example.com"
	p.KeepLatestImages = 2
	p.RateLimits = registryclient.NewRateLimitGate(http.DefaultTransport)
	p.HostedNPMScopes = []string{"@example"}
	p.HostedComposerVendors = []string{"example"}
	p.StreamArtifacts = true
	p.Scanners = &scanner.Group{}
	p.ScanSigningKey = []byte("signing-key")
	p.ScanFetchBaseURL = "http://scanner.example.com"
	p.SetMetadataRewriteCacheSize(1024)
	p.inFlight = map[string]*inflightFetch{"artifact": {}}
	p.detachSlots = make(chan struct{}, 1)
	p.inFlightMeta = map[string]*inflightMetadata{"metadata": {}}
	p.publishIndexes.entries = map[string]publishIndexEntry{"document": {}}
	p.cooldownWatch.active = map[*cooldownRecorder]struct{}{{}: {}}

	policy := profilePolicy(t, "3d")
	view := p.WithCooldown(policy, 72*time.Hour)
	if view.Cooldown != policy || view.ShortestCooldown != 72*time.Hour || view.shared != p {
		t.Fatal("view did not select its policy and shared proxy")
	}
	original, copied := reflect.ValueOf(p).Elem(), reflect.ValueOf(view).Elem()
	for i := range original.NumField() {
		name := original.Type().Field(i).Name
		before, after := original.Field(i), copied.Field(i)
		switch name {
		case "Cooldown", "ShortestCooldown", "shared":
			continue
		case "fetchMu", "inFlight", "detachSlots", "metaMu", "inFlightMeta", "publishIndexes", "cooldownWatch", "viewCooldown":
			if !after.IsZero() {
				t.Errorf("view field %s is not empty", name)
			}
			continue
		}
		if before.IsZero() {
			t.Errorf("populate Proxy.%s to check whether WithCooldown copies it", name)
			continue
		}
		switch before.Kind() {
		case reflect.Func, reflect.Slice:
			if before.Pointer() != after.Pointer() {
				t.Errorf("WithCooldown did not copy %s", name)
			}
			if before.Kind() == reflect.Slice && (before.Len() != after.Len() || before.Cap() != after.Cap()) {
				t.Errorf("WithCooldown changed slice bounds for %s", name)
			}
		default:
			if !before.Equal(after) {
				t.Errorf("WithCooldown did not copy %s", name)
			}
		}
	}
}

func TestProfilesShareDownloadPublishIndexes(t *testing.T) {
	for _, c := range downloadCooldownCases() {
		if c.ecosystem != "conda" && c.ecosystem != "helm" {
			continue
		}
		t.Run(c.ecosystem, func(t *testing.T) {
			var calls atomic.Int64
			times := map[string]time.Time{cooldownRecentVersion: time.Now().Add(-4 * 24 * time.Hour)}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				c.metadata(w, r, times)
			}))
			t.Cleanup(upstream.Close)
			p, _, _, fetcher := setupTestProxy(t)
			p.HTTPClient = upstream.Client()
			p.Cooldown = profilePolicy(t, "1d")
			fetcher.fetchErr = errors.New("artifact must be served from the cache")
			c.seedCachedArtifact(t, p, cooldownRecentVersion)
			permissive := p.WithCooldown(profilePolicy(t, "3d"), 72*time.Hour)
			strict := p.WithCooldown(profilePolicy(t, "7d"), 168*time.Hour)
			nested := permissive.WithCooldown(profilePolicy(t, "5d"), 120*time.Hour)
			for _, request := range []struct {
				proxy *Proxy
				want  int
			}{
				{permissive, http.StatusOK},
				{strict, http.StatusNotFound},
				{p, http.StatusOK},
				{nested, http.StatusNotFound},
			} {
				w := serveGet(t, c.handler(request.proxy, upstream.URL), c.path(cooldownRecentVersion))
				wantStatus(t, w, request.want, "")
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("index downloads across profiles = %d, want 1", n)
			}
			if n := len(p.publishIndexes.entries); n != 1 {
				t.Errorf("shared publish indexes = %d, want 1", n)
			}
			if fetcher.fetchCalled {
				t.Error("cached artifact was fetched again")
			}
		})
	}
}

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
