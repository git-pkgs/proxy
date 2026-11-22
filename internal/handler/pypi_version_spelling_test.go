package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

var pypiWheelSpellings = []string{
	"newpkg-2.0.0-py3-none-any.whl",
	"newpkg-2.0.0.0-py3-none-any.whl",
	"newpkg-2.0-py3-none-any.whl",
	"newpkg-V2.0.0-py3-none-any.whl",
	"newpkg-\u00a02.0.0-py3-none-any.whl",
	"newpkg-2.0.0\u2003-py3-none-any.whl",
}

func pypiSpellingRequest(t *testing.T, proxy *Proxy, upstreamURL, filename string) int {
	t.Helper()
	h := &PyPIHandler{proxy: proxy, upstreamURL: upstreamURL, proxyURL: "http://localhost"}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/packages/packages/ab/cd/ef0123456789/"+url.PathEscape(filename), nil))
	return w.Code
}

// TestPyPIDownloadCooldownMatchesVersionSpellings checks every spelling of
// a release in cooldown is withheld, not only the one the JSON API uses.
func TestPyPIDownloadCooldownMatchesVersionSpellings(t *testing.T) {
	recent := time.Now().Add(-time.Hour).Format(time.RFC3339)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"releases": {"2.0.0": [{"upload_time_iso_8601": "`+recent+`"}]}}`)
	}))
	defer upstream.Close()
	for _, filename := range pypiWheelSpellings {
		t.Run(filename, func(t *testing.T) {
			proxy, _, _, fetcher := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.Cooldown = &cooldown.Config{Default: "7d"}
			fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("wheel"))}
			if code := pypiSpellingRequest(t, proxy, upstream.URL, filename); code != http.StatusNotFound {
				t.Errorf("status %d, want 404", code)
			}
		})
	}
}

// TestPyPIDenylistMatchesVersionSpellings checks a denylist entry blocks
// every spelling of its version, and an entry spelled otherwise blocks the
// normalized one.
func TestPyPIDenylistMatchesVersionSpellings(t *testing.T) {
	for _, entry := range []string{"pkg:pypi/newpkg@2.0.0", "pkg:pypi/newpkg@2.0"} {
		for _, filename := range pypiWheelSpellings {
			t.Run(entry+"/"+filename, func(t *testing.T) {
				proxy, _, _, fetcher := setupTestProxy(t)
				setTestDenylist(t, proxy, entry)
				fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("wheel"))}
				if code := pypiSpellingRequest(t, proxy, "https://upstream.invalid", filename); code != http.StatusForbidden {
					t.Errorf("status %d, want 403", code)
				}
			})
		}
	}
}

// TestPyPIReleaseTimeTakesTheNewestOfEqualReleases checks releases that
// compare equal, as old projects can have, decide by the newest of them.
func TestPyPIReleaseTimeTakesTheNewestOfEqualReleases(t *testing.T) {
	older, newer := time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour)
	times := map[string]time.Time{"1.0": older, "1.0.0": newer, "2.0": {}}
	for range 20 {
		for _, version := range []string{"1.0", "1.0.0", "1.0.0.0"} {
			if got := releaseTime(times, version); !got.Equal(newer) {
				t.Fatalf("releaseTime(%q) = %v, want %v", version, got, newer)
			}
		}
	}
	if got := releaseTime(times, "3.0"); !got.IsZero() {
		t.Errorf("releaseTime(3.0) = %v, want the zero time", got)
	}
}
