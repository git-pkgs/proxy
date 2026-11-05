package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	registryclient "github.com/git-pkgs/registries/client"
)

func TestDownloadCooldownLookupFailures(t *testing.T) {
	for _, c := range downloadCooldownCases() {
		for _, failure := range []string{"upstream error", "invalid metadata", "rate limited"} {
			for _, cached := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/cached=%t", c.ecosystem, failure, cached), func(t *testing.T) {
					assertDownloadLookupFailure(t, c, failure, cached)
				})
			}
		}
	}
}

func assertDownloadLookupFailure(t *testing.T, c downloadCooldownCase, failure string, cached bool) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failure == "upstream error" {
			w.WriteHeader(http.StatusInternalServerError)
		}
		_, _ = w.Write([]byte("invalid metadata"))
	}))
	t.Cleanup(upstream.Close)
	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	proxy.HTTPClient = upstream.Client()
	if failure == "rate limited" {
		proxy.HTTPClient = &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			return nil, &registryclient.HeldError{Host: "upstream.example", Until: time.Now().Add(time.Minute), RetryAfter: time.Minute}
		})}
	}
	if cached {
		c.seedCachedArtifact(t, proxy, cooldownRecentVersion)
	}
	w := serveGet(t, c.handler(proxy, upstream.URL), c.path(cooldownRecentVersion))
	want := http.StatusBadGateway
	if failure == "rate limited" {
		want = http.StatusServiceUnavailable
	}
	if cached {
		want = http.StatusOK
		if w.Body.Len() == 0 {
			t.Fatal("cached artifact body is empty")
		}
	}
	wantStatus(t, w, want, "")
	if fetcher.fetchCalled {
		t.Fatal("lookup failure fetched an unchecked artifact")
	}
}

func TestDownloadPublishIndexesHaveShortTTLWithoutMetadataCache(t *testing.T) {
	for _, c := range downloadCooldownCases() {
		if c.ecosystem != "conda" && c.ecosystem != "helm" {
			continue
		}
		t.Run(c.ecosystem, func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				c.metadata(w, r, map[string]time.Time{
					cooldownOldVersion:    time.Now().Add(-30 * 24 * time.Hour),
					cooldownRecentVersion: time.Now().Add(-time.Hour),
				})
			}))
			t.Cleanup(upstream.Close)
			proxy, _, _, _ := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.Cooldown = &cooldown.Config{Default: "7d"}
			clock := &testClock{t: time.Now()}
			proxy.now = clock.now
			c.seedCachedArtifact(t, proxy, cooldownOldVersion)
			c.seedCachedArtifact(t, proxy, cooldownRecentVersion)
			h := c.handler(proxy, upstream.URL)
			for range 3 {
				wantStatus(t, serveGet(t, h, c.path(cooldownOldVersion)), http.StatusOK, "")
				wantStatus(t, serveGet(t, h, c.path(cooldownRecentVersion)), http.StatusNotFound, "")
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("index downloads within TTL = %d, want 1", n)
			}
			clock.advance(time.Minute + time.Second)
			wantStatus(t, serveGet(t, h, c.path(cooldownRecentVersion)), http.StatusNotFound, "")
			if n := calls.Load(); n != 2 {
				t.Errorf("index downloads after expiry = %d, want 2", n)
			}
		})
	}
}
