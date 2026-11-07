package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGemDownloadSharesPublishTimeMetadata(t *testing.T) {
	u, proxy, h := gemCacheSetup(t)
	wantStatus(t, serveGet(t, h, gemInfoPath), http.StatusOK, "")
	seedPackage(t, proxy.DB, proxy.Storage.(*mockStorage), "gem", "testgem", "1.0.0", "testgem-1.0.0.gem", "gem bytes")
	wantStatus(t, serveGet(t, h, "/gems/testgem-1.0.0.gem"), http.StatusOK, "")
	if n, _ := u.counts(gemVersionsPath); n != 1 {
		t.Fatalf("versions API fetched %d times, want 1", n)
	}
}

type downloadPublishTimeCase struct {
	name       string
	down       bool
	absent     bool
	untimed    bool
	refresh    bool
	wantStatus int
}

func TestDownloadCooldownRefreshesMissingPublishTimes(t *testing.T) {
	for _, c := range downloadCooldownCases() {
		if c.ecosystem != "gem" && c.ecosystem != "hex" {
			continue
		}
		for _, scenario := range []downloadPublishTimeCase{
			{name: "refreshed times", refresh: true, wantStatus: http.StatusNotFound},
			{name: "stale times and listed version", down: true, wantStatus: http.StatusNotFound},
			{name: "stale times and absent version", down: true, absent: true, wantStatus: http.StatusOK},
			{name: "fresh times without version", wantStatus: http.StatusOK},
			{name: "stale times with untimed version", down: true, untimed: true, wantStatus: http.StatusOK},
		} {
			for _, cached := range []bool{false, true} {
				if !cached && scenario.wantStatus == http.StatusOK {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/cached=%t", c.ecosystem, scenario.name, cached), func(t *testing.T) {
					assertDownloadPublishTime(t, c, scenario, cached)
				})
			}
		}
	}
}

func assertDownloadPublishTime(t *testing.T, c downloadCooldownCase, scenario downloadPublishTimeCase, cached bool) {
	t.Helper()
	u := newCooldownCountingUpstream(t)
	proxy := cooldownCacheProxy(t, u)
	fetcher := proxy.Fetcher.(*mockFetcher)
	fetcher.fetchErr = errors.New("unchecked artifact download")
	h := c.handler(proxy, u.URL)
	docPath, timesPath := "/info/demo", "/api/v1/versions/demo.json"
	if c.ecosystem == "hex" {
		docPath, timesPath = "/packages/demo", "/api/packages/demo"
	}
	oldTimes := map[string]time.Time{cooldownOldVersion: time.Now().Add(-30 * 24 * time.Hour)}
	if scenario.untimed {
		oldTimes[cooldownRecentVersion] = time.Time{}
	}
	setDownloadMetadata(t, u, c, docPath, oldTimes)
	setDownloadMetadata(t, u, c, timesPath, oldTimes)
	wantStatus(t, serveGet(t, h, docPath), http.StatusOK, "")

	newTimes := map[string]time.Time{
		cooldownOldVersion: oldTimes[cooldownOldVersion],
		cooldownUntimed:    time.Now().Add(-time.Minute),
	}
	if !scenario.absent {
		newTimes[cooldownRecentVersion] = time.Now().Add(-time.Hour)
	}
	setDownloadMetadata(t, u, c, docPath, newTimes)
	expireCachedMetadata(t, proxy, c.ecosystem, docPath[1:])
	if scenario.refresh {
		setDownloadMetadata(t, u, c, timesPath, newTimes)
	}
	u.setDown(timesPath, scenario.down)
	if cached {
		c.seedCachedArtifact(t, proxy, cooldownRecentVersion)
	}
	wantStatus(t, serveGet(t, h, c.path(cooldownRecentVersion)), scenario.wantStatus, "")
	if fetcher.fetchCalled {
		t.Error("fetched an unchecked artifact")
	}
	if n, _ := u.counts(timesPath); n != 2 {
		t.Errorf("publish times fetched %d times, want 2", n)
	}
}

func setDownloadMetadata(t *testing.T, u *cooldownCountingUpstream, c downloadCooldownCase, path string, times map[string]time.Time) {
	t.Helper()
	w := httptest.NewRecorder()
	c.metadata(w, httptest.NewRequest(http.MethodGet, path, nil), times)
	wantStatus(t, w, http.StatusOK, "")
	u.set(path, w.Body.Bytes())
}

func TestHexDownloadSharesPublishTimeMetadata(t *testing.T) {
	u, proxy, h, _ := hexCacheSetup(t, 0)
	wantStatus(t, serveGet(t, h, hexPackagePath), http.StatusOK, "")
	seedPackage(t, proxy.DB, proxy.Storage.(*mockStorage), "hex", "testpkg", "1.0.0", "testpkg-1.0.0.tar", "hex bytes")
	wantStatus(t, serveGet(t, h, "/tarballs/testpkg-1.0.0.tar"), http.StatusOK, "")
	if n, _ := u.counts(hexAPIPath); n != 1 {
		t.Fatalf("Hex API fetched %d times, want 1", n)
	}
}
