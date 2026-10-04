package handler

import (
	"net/http"
	"strings"
	"testing"

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
