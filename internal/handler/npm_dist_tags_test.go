package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// taggedPackument has 1.0.0 published a month ago and 2.0.0 an hour ago,
// tagged latest.
func taggedPackument(name string) func(string) string {
	month := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	hour := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	return func(baseURL string) string {
		return `{"name":"` + name + `","dist-tags":{"latest":"2.0.0","stable":"1.0.0"},"versions":{` +
			`"1.0.0":{"version":"1.0.0","dist":{"tarball":"` + baseURL + `/x/-/x-1.0.0.tgz"}},` +
			`"2.0.0":{"version":"2.0.0","dist":{"tarball":"` + baseURL + `/x/-/x-2.0.0.tgz"}}},` +
			`"time":{"1.0.0":"` + month + `","2.0.0":"` + hour + `"}}`
	}
}

func decodeDistTags(t *testing.T, code int, body []byte) map[string]string {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", code, body)
	}
	var tags map[string]string
	if err := json.Unmarshal(body, &tags); err != nil {
		t.Fatalf("dist-tags are not a JSON object: %v: %s", err, body)
	}
	return tags
}

func TestNPMDistTags(t *testing.T) {
	upstream := newNPMUpstream(t, map[string]func(string) string{"/demo": taggedPackument("demo")}, http.StatusOK)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	w := serveNPM(t, h, http.MethodGet, "/-/package/demo/dist-tags", nil, nil)
	tags := decodeDistTags(t, w.Code, w.Body.Bytes())
	if tags["latest"] != "2.0.0" || tags["stable"] != "1.0.0" || len(tags) != 2 {
		t.Errorf("dist-tags = %v", tags)
	}
}

// TestNPMDistTagsFollowCooldown checks that latest never names a version
// cooldown holds back, as in the packument.
func TestNPMDistTagsFollowCooldown(t *testing.T) {
	upstream := newNPMUpstream(t, map[string]func(string) string{"/demo": taggedPackument("demo")}, http.StatusOK)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	w := serveNPM(t, h, http.MethodGet, "/-/package/demo/dist-tags", nil, nil)
	if tags := decodeDistTags(t, w.Code, w.Body.Bytes()); tags["latest"] != "1.0.0" {
		t.Errorf("dist-tags = %v, want latest 1.0.0", tags)
	}
}

func TestNPMDistTagsRoutedPackage(t *testing.T) {
	public := newNPMUpstream(t, map[string]func(string) string{"/@example/widgets": routedTestPackument("9.9.9", "")}, http.StatusOK)
	private := newNPMUpstream(t, map[string]func(string) string{routedNPMPath: routedTestPackument("1.0.0", "/registry")}, http.StatusOK)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	h := newRoutedNPMHandler(proxy, public, private)

	w := serveNPM(t, h, http.MethodGet, "/-/package/@example%2fwidgets/dist-tags", nil, nil)
	if tags := decodeDistTags(t, w.Code, w.Body.Bytes()); tags["latest"] != "1.0.0" {
		t.Errorf("dist-tags = %v, want the route's latest 1.0.0", tags)
	}
	if got := public.requested(); len(got) != 0 {
		t.Errorf("default registry was queried for a routed package: %v", got)
	}
}

func TestNPMDistTagsHostedOnlyScope(t *testing.T) {
	f := newNPMHostedFixture(t, nil)
	f.proxy.HostedNPMScopes = []string{"@acme"}
	f.push(t, "@acme/private", "1.10.0", `{"name":"@acme/private","version":"1.10.0"}`)
	f.push(t, "@acme/private", "1.9.0", `{"name":"@acme/private","version":"1.9.0"}`)

	rec := f.get(t, "/-/package/@acme%2fprivate/dist-tags")
	if tags := decodeDistTags(t, rec.Code, rec.Body.Bytes()); tags["latest"] != "1.10.0" {
		t.Errorf("dist-tags = %v, want latest 1.10.0", tags)
	}
	if rec := f.get(t, "/-/package/@acme%2fother/dist-tags"); rec.Code != http.StatusNotFound {
		t.Errorf("unpushed package status = %d, want 404", rec.Code)
	}
	if *f.upstreamRequests != 0 {
		t.Errorf("hosted-only scope sent %d requests upstream", *f.upstreamRequests)
	}
}

func TestNPMDistTagsErrors(t *testing.T) {
	upstream := newNPMUpstream(t, map[string]func(string) string{}, http.StatusOK)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	tests := []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, "/-/package/missing/dist-tags", http.StatusNotFound},
		{http.MethodGet, "/-/package/@scope/dist-tags", http.StatusBadRequest},
		{http.MethodGet, "/-/package/a/b/c/dist-tags", http.StatusBadRequest},
		{http.MethodGet, "/-/package/..%2f..%2fetc/dist-tags", http.StatusBadRequest},
		{http.MethodPut, "/-/package/demo/dist-tags/beta", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/-/package/demo/dist-tags/beta", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		if w := serveNPM(t, h, tt.method, tt.target, nil, nil); w.Code != tt.want {
			t.Errorf("%s %s: status = %d, want %d", tt.method, tt.target, w.Code, tt.want)
		}
	}
	if got := upstream.requested(); len(got) != 1 {
		t.Errorf("upstream requests = %v, want only the missing package", got)
	}
}

// TestNPMDistTagsWithPushedVersion checks that a push next to upstream
// versions leaves the tags as upstream has them, as in the packument.
func TestNPMDistTagsWithPushedVersion(t *testing.T) {
	f := newNPMHostedFixture(t, map[string]string{"/request": requestPackument})
	f.push(t, "request", "2.88.4", `{"name":"request","version":"2.88.4"}`)

	rec := f.get(t, "/-/package/request/dist-tags")
	if tags := decodeDistTags(t, rec.Code, rec.Body.Bytes()); tags["latest"] != "2.88.2" {
		t.Errorf("dist-tags = %v, want upstream's latest 2.88.2", tags)
	}
}
