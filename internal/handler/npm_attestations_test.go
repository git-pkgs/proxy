package handler

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

const testAttestations = `{"attestations":[{"predicateType":"https://slsa.dev/provenance/v1","bundle":{}}]}`

// attestedPackument returns a packument whose version lists attestations at
// attestationsURL, resolved against the upstream's URL when it starts with
// "/", and was published age ago.
func attestedPackument(name, version, attestationsURL string, age time.Duration) func(string) string {
	published := time.Now().Add(-age).UTC().Format(time.RFC3339)
	return func(baseURL string) string {
		if strings.HasPrefix(attestationsURL, "/") {
			attestationsURL = baseURL + attestationsURL
		}
		dist := `{"tarball":"` + baseURL + "/" + name + `/-/x-` + version + `.tgz"`
		if attestationsURL != "" {
			dist += `,"attestations":{"url":"` + attestationsURL + `","provenance":{"predicateType":"https://slsa.dev/provenance/v1"}}`
		}
		dist += "}"
		return `{"name":"` + name + `","dist-tags":{"latest":"` + version + `"},` +
			`"versions":{"` + version + `":{"version":"` + version + `","dist":` + dist + `}},` +
			`"time":{"` + version + `":"` + published + `"}}`
	}
}

func attestationsFile(string) string { return testAttestations }

func newAttestationsProxy(t *testing.T) *Proxy {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	return proxy
}

func TestNPMAttestationsServedAndCached(t *testing.T) {
	upstream := newNPMUpstream(t, map[string]func(string) string{
		"/sigstore":                             attestedPackument("sigstore", "3.0.0", "/-/npm/v1/attestations/sigstore@3.0.0", 30*24*time.Hour),
		"/-/npm/v1/attestations/sigstore@3.0.0": attestationsFile,
	}, http.StatusOK)
	h := NewNPMHandler(newAttestationsProxy(t), "http://proxy.test", upstream.URL)

	for range 2 {
		w := serveNPM(t, h, http.MethodGet, "/-/npm/v1/attestations/sigstore@3.0.0", nil, nil)
		if w.Code != http.StatusOK || w.Body.String() != testAttestations {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
	}
	fetched := 0
	for _, p := range upstream.requested() {
		if p == "/-/npm/v1/attestations/sigstore@3.0.0" {
			fetched++
		}
	}
	if fetched != 1 {
		t.Errorf("attestations fetched %d times, want once: %v", fetched, upstream.requested())
	}
}

func TestNPMAttestationsRoutedPackageStaysOnRoute(t *testing.T) {
	public := newNPMUpstream(t, map[string]func(string) string{}, http.StatusOK)
	private := newNPMUpstream(t, map[string]func(string) string{
		routedNPMPath: attestedPackument("@example/widgets", "1.0.0", "/registry/-/npm/v1/attestations/@example%2fwidgets@1.0.0", 0),
		"/registry/-/npm/v1/attestations/@example/widgets@1.0.0": attestationsFile,
	}, http.StatusOK)
	h := newRoutedNPMHandler(newAttestationsProxy(t), public, private)

	for _, target := range []string{
		"/-/npm/v1/attestations/@example%2fwidgets@1.0.0",
		"/-/npm/v1/attestations/@example/widgets@1.0.0",
	} {
		w := serveNPM(t, h, http.MethodGet, target, nil, nil)
		if w.Code != http.StatusOK || w.Body.String() != testAttestations {
			t.Errorf("%s: status = %d, body = %s", target, w.Code, w.Body.String())
		}
	}
	if got := public.requested(); len(got) != 0 {
		t.Errorf("default registry was queried for a routed package: %v", got)
	}
}

// TestNPMAttestationsOnlyAskThePackageRegistry checks that an attestations
// URL on another host is not followed: the proxy asks the package's registry
// at its standard path instead.
func TestNPMAttestationsOnlyAskThePackageRegistry(t *testing.T) {
	other := newNPMUpstream(t, map[string]func(string) string{"/-/npm/v1/attestations/demo@1.0.0": attestationsFile}, http.StatusOK)
	upstream := newNPMUpstream(t, map[string]func(string) string{
		"/demo":                             attestedPackument("demo", "1.0.0", other.URL+"/-/npm/v1/attestations/demo@1.0.0", 0),
		"/-/npm/v1/attestations/demo@1.0.0": attestationsFile,
	}, http.StatusOK)
	h := NewNPMHandler(newAttestationsProxy(t), "http://proxy.test", upstream.URL)

	w := serveNPM(t, h, http.MethodGet, "/-/npm/v1/attestations/demo@1.0.0", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := other.requested(); len(got) != 0 {
		t.Errorf("host named in the packument was queried: %v", got)
	}
	if !slices.Contains(upstream.requested(), "/-/npm/v1/attestations/demo@1.0.0") {
		t.Errorf("registry not asked for attestations: %v", upstream.requested())
	}
}

func TestNPMAttestationsWithheld(t *testing.T) {
	tests := []struct {
		name       string
		packument  func(string) string
		setup      func(t *testing.T, p *Proxy)
		wantStatus int
	}{
		{"version has none", attestedPackument("demo", "1.0.0", "", 0), nil, http.StatusNotFound},
		{"version in cooldown", attestedPackument("demo", "1.0.0", "/-/npm/v1/attestations/demo@1.0.0", time.Hour),
			func(_ *testing.T, p *Proxy) { p.Cooldown = &cooldown.Config{Default: "7d"} }, http.StatusNotFound},
		{"version denied", attestedPackument("demo", "1.0.0", "/-/npm/v1/attestations/demo@1.0.0", 0),
			func(t *testing.T, p *Proxy) { setTestDenylist(t, p, "pkg:npm/demo@1.0.0") }, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newNPMUpstream(t, map[string]func(string) string{
				"/demo":                             tt.packument,
				"/-/npm/v1/attestations/demo@1.0.0": attestationsFile,
			}, http.StatusOK)
			proxy := newAttestationsProxy(t)
			if tt.setup != nil {
				tt.setup(t, proxy)
			}
			h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

			w := serveNPM(t, h, http.MethodGet, "/-/npm/v1/attestations/demo@1.0.0", nil, nil)
			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if slices.Contains(upstream.requested(), "/-/npm/v1/attestations/demo@1.0.0") {
				t.Error("attestations were fetched for a withheld version")
			}
		})
	}
}

func TestNPMAttestationsRejectsInvalidPaths(t *testing.T) {
	upstream := newNPMUpstream(t, map[string]func(string) string{}, http.StatusOK)
	h := NewNPMHandler(newAttestationsProxy(t), "http://proxy.test", upstream.URL)

	for _, target := range []string{
		"/-/npm/v1/attestations/demo",
		"/-/npm/v1/attestations/demo@",
		"/-/npm/v1/attestations/@demo@1.0.0",
		"/-/npm/v1/attestations/@scope/a/b@1.0.0",
		"/-/npm/v1/attestations/demo@1.0.0/x",
		"/-/npm/v1/attestations/..%2f..%2fetc@1.0.0",
	} {
		w := serveNPM(t, h, http.MethodGet, target, nil, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, w.Code)
		}
	}
	if got := upstream.requested(); len(got) != 0 {
		t.Errorf("upstream queried for invalid paths: %v", got)
	}
}

func TestNPMMetadataRewritesAttestationsURL(t *testing.T) {
	const attestations = "/registry/-/npm/v1/attestations/@example%2fwidgets@1.0.0"
	upstream := newNPMUpstream(t, map[string]func(string) string{
		routedNPMPath: attestedPackument("@example/widgets", "1.0.0", attestations, 0),
		routedNPMPath + "/1.0.0": func(baseURL string) string {
			return `{"name":"@example/widgets","version":"1.0.0","dist":{"attestations":{"url":"` + baseURL + attestations +
				`"},"tarball":"` + baseURL + `/registry/@example/widgets/-/widgets-1.0.0.tgz"}}`
		},
	}, http.StatusOK)
	h := newRoutedNPMHandler(newAttestationsProxy(t), upstream, upstream)

	for _, target := range []string{"/@example%2fwidgets", "/@example%2fwidgets/1.0.0"} {
		w := serveNPM(t, h, http.MethodGet, target, nil, nil)
		want := `"url":"http://proxy.test/npm/-/npm/v1/attestations/@example%2Fwidgets@1.0.0"`
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: status %d, attestations URL not rewritten: %s", target, w.Code, w.Body.String())
		}
	}
}
