package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/cooldownpolicy"
)

func TestNuGetCooldownPackagePatterns(t *testing.T) {
	for _, tc := range []struct {
		name, defaultDuration, pattern string
		exact                          map[string]string
		blocked                        bool
	}{
		{"pattern enables cooldown", "", "14d", nil, true},
		{"pattern exempts package", "14d", "0", nil, false},
		{"exact overrides exemption", "", "0", map[string]string{"pkg:nuget/testpkg": "14d"}, true},
		{"exact exempts package", "", "14d", map[string]string{"pkg:nuget/testpkg": "0"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, db, store, fetcher := setupTestProxy(t)
			policy, err := cooldownpolicy.New(&cooldown.Config{Default: tc.defaultDuration, Packages: tc.exact}, map[string]string{"pkg:nuget/test*": tc.pattern})
			if err != nil {
				t.Fatal(err)
			}
			p.Cooldown = policy
			seedPackage(t, db, store, "nuget", "testpkg", "2.0.0", "testpkg.2.0.0.nupkg", "cached package")
			requests := 0
			upstream := newNuGetCooldownUpstream(t, &requests)
			defer upstream.Close()
			p.HTTPClient = upstream.Client()
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
			list := nugetGet(t, h.Routes(), "/v3-flatcontainer/TestPkg/index.json", http.StatusOK)
			if strings.Contains(list.Body.String(), "2.0.0") == tc.blocked {
				t.Fatalf("incorrect version list: %s", list.Body.String())
			}
			status := http.StatusOK
			if tc.blocked {
				status = http.StatusNotFound
			}
			nugetGet(t, h.Routes(), "/v3-flatcontainer/TestPkg/2.0.0/testpkg.2.0.0.nupkg", status)
			if fetcher.fetchCalled {
				t.Fatal("cached or withheld download fetched upstream artifact")
			}
		})
	}
}

func TestNPMCooldownPatternExemptionRespectsDenylist(t *testing.T) {
	p, db, store, fetcher := setupTestProxy(t)
	var err error
	p.Cooldown, err = cooldownpolicy.New(&cooldown.Config{Default: "7d"}, map[string]string{"pkg:npm/@example/*": "0"})
	if err != nil {
		t.Fatal(err)
	}
	setTestDenylist(t, p, "pkg:npm/@example/widget@1.0.0")
	published := time.Now().Add(-time.Hour)
	for _, version := range []string{"1.0.0", "2.0.0"} {
		seedPackage(t, db, store, "npm", "@example/widget", version, "widget-"+version+".tgz", "cached package")
		if err := db.SetVersionPublishedAt("pkg:npm/%40example/widget@"+version, "pkg:npm/%40example/widget", published); err != nil {
			t.Fatal(err)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/@example/widget" {
			t.Errorf("unexpected upstream request: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name":"@example/widget","dist-tags":{"latest":"1.0.0"},"versions":{"1.0.0":{},"2.0.0":{}},"time":{"1.0.0":%q,"2.0.0":%q}}`, published.Format(time.RFC3339), published.Format(time.RFC3339))
	}))
	defer upstream.Close()
	p.HTTPClient = upstream.Client()
	h := NewNPMHandler(p, "http://proxy.test", upstream.URL)
	server := httptest.NewServer(h.Routes())
	defer server.Close()
	for _, tc := range []struct {
		path             string
		status           int
		contains, absent string
	}{
		{"/@example%2Fwidget", http.StatusOK, `"latest":"2.0.0"`, "1.0.0"},
		{"/@example%2Fwidget/-/widget-1.0.0.tgz", http.StatusForbidden, "denylist", "cached package"},
		{"/@example%2Fwidget/-/widget-2.0.0.tgz", http.StatusOK, "cached package", "denylist"},
	} {
		response, err := server.Client().Get(server.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != tc.status || !strings.Contains(string(body), tc.contains) || strings.Contains(string(body), tc.absent) {
			t.Errorf("GET %s: status=%d body=%s error=%v", tc.path, response.StatusCode, body, err)
		}
	}
	if fetcher.fetchCalled {
		t.Error("cached or denied downloads fetched an upstream artifact")
	}
}
