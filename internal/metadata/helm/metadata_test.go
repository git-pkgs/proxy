package helm

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// dayPolicy holds back releases younger than a day at the evaluation time.
type dayPolicy struct {
	enabled bool
	purls   []string
}

func (p *dayPolicy) Enabled() bool { return p.enabled }

func (p *dayPolicy) Evaluate(eco, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision {
	p.purls = append(p.purls, eco+" "+packagePURL)
	return cooldown.Decision{Allowed: evaluatedAt.Sub(publishedAt) >= 24*time.Hour}
}

var (
	digestA = strings.Repeat("a", 64)
	digestB = strings.Repeat("b", 64)
	digestC = strings.Repeat("c", 64)
	now     = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
)

const testIndex = `apiVersion: v1
entries:
  demo:
    - version: 2.0.0
      created: "2026-11-23T06:00:00Z"
      digest: sha256:` + "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB" + `
      urls: [demo-2.0.0.tgz]
    - version: 1.0.0
      created: "2026-11-01T00:00:00Z"
      digest: ` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `
      urls: [charts/demo-1.0.0.tgz, "https://mirror.example/demo-1.0.0.tgz"]
    - version: 0.9.0
      digest: ` + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" + `
      urls: ["oci://ghcr.io/acme/demo:0.9.0"]
    - version: 0.1.0
      digest: nope
      urls: [demo-0.1.0.tgz]
`

func rewrite(t *testing.T, o Options) (string, []string) {
	t.Helper()
	if o.ProxyURL == "" {
		o.ProxyURL = "https://proxy.example"
	}
	if o.Repository == "" {
		o.Repository = "my repo"
	}
	if o.UpstreamURL == "" {
		o.UpstreamURL = "https://charts.example/sub"
	}
	out, kept, err := Rewrite([]byte(testIndex), o)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	return string(out), kept
}

func TestRewritePointsURLsAtTheProxy(t *testing.T) {
	var malformed []string
	out, kept := rewrite(t, Options{
		OCIRegistries: []OCIRegistry{{Host: "ghcr.io", Prefix: "/upstream/gh"}},
		OnMalformed:   func(chart string, err error) { malformed = append(malformed, chart+": "+err.Error()) },
	})
	for _, want := range []string{
		"https://proxy.example/helm/my%20repo/charts/" + digestB + "/demo-2.0.0.tgz",
		"https://proxy.example/helm/my%20repo/charts/" + digestA + "/demo-1.0.0.tgz",
		"oci://proxy.example/upstream/gh/acme/demo:0.9.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "charts.example") || strings.Contains(out, "mirror.example") || strings.Contains(out, "0.1.0") {
		t.Errorf("output keeps an upstream URL or the malformed release:\n%s", out)
	}
	want := []string{"demo@" + digestB, "demo@" + digestA, "demo@" + digestC}
	if !slices.Equal(kept, want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
	if len(malformed) != 1 || !strings.Contains(malformed[0], "demo: chart \"demo\" has invalid digest") {
		t.Errorf("malformed = %v, want the release with the bad digest", malformed)
	}
}

func TestRewriteCooldownAtEvaluatedAt(t *testing.T) {
	policy := &dayPolicy{enabled: true}
	out, kept := rewrite(t, Options{Cooldown: policy, EvaluatedAt: now})
	if strings.Contains(out, "2.0.0") {
		t.Errorf("release created 6h before EvaluatedAt is listed:\n%s", out)
	}
	want := []string{"demo@" + digestA, "demo@" + digestC}
	if !slices.Equal(kept, want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
	// Releases without a creation time aren't evaluated.
	if len(policy.purls) != 2 || policy.purls[0] != "helm pkg:helm/demo" {
		t.Errorf("evaluated %v, want pkg:helm/demo twice", policy.purls)
	}

	// A day later the release is out of cooldown.
	_, kept = rewrite(t, Options{Cooldown: policy, EvaluatedAt: now.Add(24 * time.Hour)})
	if len(kept) != 3 {
		t.Errorf("kept = %v a day later, want all three", kept)
	}
}

func TestRewriteWithoutCooldown(t *testing.T) {
	for name, policy := range map[string]CooldownPolicy{"nil": nil, "disabled": &dayPolicy{}} {
		_, kept := rewrite(t, Options{Cooldown: policy, EvaluatedAt: now})
		if len(kept) != 3 {
			t.Errorf("%s policy: kept = %v, want all three", name, kept)
		}
	}
}

func TestRewriteRefusesBadIndexes(t *testing.T) {
	for name, body := range map[string]string{
		"not yaml":      "entries: [",
		"no entries":    "apiVersion: v1\n",
		"releases list": "entries:\n  demo: {version: 1}\n",
	} {
		if _, _, err := Rewrite([]byte(body), Options{}); err == nil {
			t.Errorf("%s: Rewrite accepted it", name)
		}
		if _, err := BuildLookup("https://charts.example", []byte(body)); err == nil {
			t.Errorf("%s: BuildLookup accepted it", name)
		}
	}
}

func TestBuildLookup(t *testing.T) {
	lookup, err := BuildLookup("https://charts.example/sub", []byte(testIndex))
	if err != nil {
		t.Fatal(err)
	}
	a := lookup.ByDigest[digestA]
	if len(a) != 1 || a[0].Name != "demo" || !a[0].Created.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("ByDigest[a] = %+v", a)
	}
	wantDownloads := []Download{
		{URL: "https://charts.example/sub/charts/demo-1.0.0.tgz", Filename: "demo-1.0.0.tgz"},
		{URL: "https://mirror.example/demo-1.0.0.tgz", Filename: "demo-1.0.0.tgz"},
	}
	if !slices.Equal(a[0].Downloads, wantDownloads) {
		t.Errorf("downloads = %+v, want %+v", a[0].Downloads, wantDownloads)
	}
	// OCI references aren't HTTP downloads, but the release is listed.
	if c := lookup.ByDigest[digestC]; len(c) != 1 || len(c[0].Downloads) != 0 {
		t.Errorf("ByDigest[c] = %+v, want one entry without downloads", c)
	}
	oci := lookup.ByRelease[Release{"demo", "0.9.0"}]
	if !oci.First.OCI || oci.First.URL != "oci://ghcr.io/acme/demo:0.9.0" || oci.Digest != digestC {
		t.Errorf("ByRelease 0.9.0 = %+v", oci)
	}
	one := lookup.ByRelease[Release{"demo", "1.0.0"}]
	if one.First != wantDownloads[0] || one.Digest != digestA || !one.Created.Equal(a[0].Created) {
		t.Errorf("ByRelease 1.0.0 = %+v", one)
	}
	if _, ok := lookup.ByRelease[Release{"demo", "0.1.0"}]; ok {
		t.Error("the malformed release is listed")
	}
	if !lookup.Lists(digestA, "demo-1.0.0.tgz") || lookup.Lists(digestA, "demo-2.0.0.tgz") || lookup.Lists(digestC, "") {
		t.Error("Lists doesn't match the HTTP downloads")
	}
}

func TestBuildLookupKeepsTheFirstReleaseOfAVersion(t *testing.T) {
	index := "entries:\n  demo:\n" +
		"    - {version: 1.0.0, digest: " + digestA + ", urls: [first.tgz]}\n" +
		"    - {version: 1.0.0, digest: " + digestB + ", urls: [second.tgz]}\n" +
		"    - {digest: " + digestC + ", urls: [unversioned.tgz]}\n" +
		"    - {version: \"\", digest: " + strings.Repeat("d", 64) + ", urls: [blank.tgz]}\n"
	lookup, err := BuildLookup("https://charts.example", []byte(index))
	if err != nil {
		t.Fatal(err)
	}
	if got := lookup.ByRelease[Release{"demo", "1.0.0"}]; got.Digest != digestA || got.First.Filename != "first.tgz" {
		t.Errorf("ByRelease = %+v, want the first listed", got)
	}
	if len(lookup.ByRelease) != 1 || len(lookup.ByDigest[digestC]) != 1 || len(lookup.ByDigest[strings.Repeat("d", 64)]) != 1 {
		t.Errorf("lookup = %+v", lookup)
	}
}

func TestParseReleaseRefusesBadDownloads(t *testing.T) {
	for name, urls := range map[string]string{
		"no urls":         "[]",
		"not a tgz":       "[demo.zip]",
		"ftp":             "[\"ftp://charts.example/demo.tgz\"]",
		"nested":          "[[demo.tgz]]",
		"oci no path":     "[\"oci://ghcr.io\"]",
		"oci no host":     "[\"oci:///acme/demo\"]",
		"oci query":       "[\"oci://ghcr.io/acme/demo?x=1\"]",
		"oci forcequery":  "[\"oci://ghcr.io/acme/demo?\"]",
		"oci fragment":    "[\"oci://ghcr.io/acme/demo#x\"]",
		"oci userinfo":    "[\"oci://u:p@ghcr.io/acme/demo\"]",
		"oci traversal":   "[\"oci://ghcr.io/acme/../demo\"]",
		"oci encoded dot": "[\"oci://ghcr.io/acme/%2e%2e/demo\"]",
		"bad url":         "[\"https://charts.example/%zz.tgz\"]",
		"trailing slash":  "[\"https://charts.example/\"]",
		"oci backslash":   "[\"oci://ghcr.io/acme/..\\\\demo\"]",
	} {
		index := "entries:\n  demo:\n    - {digest: " + digestA + ", urls: " + urls + "}\n"
		var got error
		_, _, err := Rewrite([]byte(index), Options{UpstreamURL: "https://charts.example", OnMalformed: func(_ string, err error) { got = err }})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == nil {
			t.Errorf("%s: release accepted", name)
		}
	}
	index := "entries:\n  demo:\n    - {digest: " + digestA + ", created: yesterday, urls: [demo.tgz]}\n"
	var got error
	if _, _, err := Rewrite([]byte(index), Options{UpstreamURL: "https://charts.example", OnMalformed: func(_ string, err error) { got = err }}); err != nil || got == nil {
		t.Errorf("bad creation time: err %v, malformed %v", err, got)
	}
}

func TestOCIProxyReference(t *testing.T) {
	registries := []OCIRegistry{{Host: "registry.example", Prefix: "/upstream/r"}, {Host: "docker.io"}}
	for _, tc := range []struct{ proxy, ref, want string }{
		{"https://proxy.example", "oci://REGISTRY.example/acme/demo", "oci://proxy.example/upstream/r/acme/demo"},
		{"https://proxy.example", "oci://docker.io/acme/demo", "oci://proxy.example/acme/demo"},
		{"https://proxy.example", "oci://docker.io/upstream/x", "oci://docker.io/upstream/x"},
		{"https://proxy.example", "oci://other.example/acme/demo", "oci://other.example/acme/demo"},
		{"https://proxy.example/base", "oci://docker.io/acme/demo", "oci://docker.io/acme/demo"},
		{"", "oci://docker.io/acme/demo", "oci://docker.io/acme/demo"},
		{"https://proxy.example", "oci://docker.io/%zz", "oci://docker.io/%zz"},
		{"%zz", "oci://docker.io/acme/demo", "oci://docker.io/acme/demo"},
	} {
		if got := OCIProxyReference(tc.proxy, registries, tc.ref); got != tc.want {
			t.Errorf("OCIProxyReference(%q, %q) = %q, want %q", tc.proxy, tc.ref, got, tc.want)
		}
	}
}

func TestNormalizeDigest(t *testing.T) {
	if got, ok := NormalizeDigest("SHA256:" + strings.ToUpper(digestA)); !ok || got != digestA {
		t.Errorf("NormalizeDigest = %q, %t", got, ok)
	}
	for _, bad := range []string{"", digestA[1:], digestA + "a", strings.Repeat("g", 64), strings.Repeat("/", 64)} {
		if _, ok := NormalizeDigest(bad); ok {
			t.Errorf("NormalizeDigest(%q) accepted", bad)
		}
	}
}

func TestIndexEntries(t *testing.T) {
	if _, err := IndexEntries(nil); err == nil {
		t.Error("nil document accepted")
	}
	document, _, err := ParseIndex([]byte("entries:\n  demo: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := IndexEntries(document)
	entries.Content = entries.Content[:1]
	if _, err := IndexEntries(document); err == nil {
		t.Error("incomplete mapping accepted")
	}
	if _, _, err := ParseIndex([]byte("- a\n")); err == nil {
		t.Error("sequence document accepted")
	}
	if _, _, err := ParseIndex([]byte("entries: [a]\n")); err == nil {
		t.Error("sequence entries accepted")
	}
	if MappingValue(nil, "x") != nil || MappingValue(entries, "missing") != nil {
		t.Error("MappingValue found a missing key")
	}
}
