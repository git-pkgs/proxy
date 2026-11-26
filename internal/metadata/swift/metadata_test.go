package swift

import (
	"encoding/json"
	"strings"
	"testing"
)

var testOptions = Options{ProxyURL: "https://proxy.example", UpstreamURL: "https://registry.example/base"}

func TestRewriteRegistryURL(t *testing.T) {
	request := "https://registry.example/base/apple/swift-nio"
	tests := []struct {
		name, raw, want string
	}{
		{"absolute under upstream", "https://registry.example/base/apple/swift-nio?page=2", "https://proxy.example/swift/apple/swift-nio?page=2"},
		{"relative", "/base/apple/swift-nio/1.0.0#frag", "https://proxy.example/swift/apple/swift-nio/1.0.0#frag"},
		{"upstream root", "https://registry.example/base", "https://proxy.example/swift"},
		{"host case", "https://REGISTRY.example/base/a/b", "https://proxy.example/swift/a/b"},
		{"other host", "https://other.example/base/a/b", "https://other.example/base/a/b"},
		{"other scheme", "http://registry.example/base/a/b", "http://registry.example/base/a/b"},
		{"outside base path", "https://registry.example/basement/a", "https://registry.example/basement/a"},
		{"unparseable", "https://registry.example/%zz", "https://registry.example/%zz"},
		{"escaped path kept", "https://registry.example/base/a%2Fb/c", "https://proxy.example/swift/a%2Fb/c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RewriteRegistryURL(tt.raw, request, testOptions); got != tt.want {
				t.Errorf("RewriteRegistryURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRewriteRegistryURLBadInputs(t *testing.T) {
	raw := "https://registry.example/base/a/b"
	if got := RewriteRegistryURL(raw, "%zz://", testOptions); got != raw {
		t.Errorf("bad request URL: got %q", got)
	}
	bad := Options{ProxyURL: "https://proxy.example", UpstreamURL: "%zz://"}
	if got := RewriteRegistryURL(raw, raw, bad); got != raw {
		t.Errorf("bad upstream URL: got %q", got)
	}
}

func TestRewriteLinkHeader(t *testing.T) {
	request := "https://registry.example/base/apple/swift-nio"
	value := `<https://registry.example/base/apple/swift-nio?page=2>; rel="next", <https://other.example/x>; rel="alternate", <broken`
	want := `<https://proxy.example/swift/apple/swift-nio?page=2>; rel="next", <https://other.example/x>; rel="alternate", <broken`
	if got := RewriteLinkHeader(value, request, testOptions); got != want {
		t.Errorf("RewriteLinkHeader = %q, want %q", got, want)
	}
	if got := RewriteLinkHeader(`rel="next"`, request, testOptions); got != `rel="next"` {
		t.Errorf("no URL: got %q", got)
	}
}

func TestRewriteReleaseURLs(t *testing.T) {
	body := []byte(`{"releases":{"1.0.0":{"url":"https://registry.example/base/a/b/1.0.0"},"2.0.0+x":{"url":"u"},"3.0.0":{"problem":{}},"bad":"x"},"other":1}`)
	out, err := RewriteReleaseURLs("apple", "swift nio", body, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Releases map[string]json.RawMessage `json:"releases"`
		Other    int                        `json:"other"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Other != 1 {
		t.Errorf("other field lost: %s", out)
	}
	url := func(version string) string {
		var release map[string]any
		_ = json.Unmarshal(doc.Releases[version], &release)
		s, _ := release["url"].(string)
		return s
	}
	if got := url("1.0.0"); got != "https://proxy.example/swift/apple/swift%20nio/1.0.0" {
		t.Errorf("1.0.0 url = %q", got)
	}
	if got := url("2.0.0+x"); got != "https://proxy.example/swift/apple/swift%20nio/2.0.0+x" {
		t.Errorf("2.0.0+x url = %q", got)
	}
	if strings.Contains(string(doc.Releases["3.0.0"]), "url") {
		t.Errorf("release without url got one: %s", doc.Releases["3.0.0"])
	}
	if string(doc.Releases["bad"]) != `"x"` {
		t.Errorf("non-object release changed: %s", doc.Releases["bad"])
	}
}

func TestRewriteReleaseURLsWithoutReleases(t *testing.T) {
	body := []byte(`{"releases":[]}`)
	out, err := RewriteReleaseURLs("a", "b", body, testOptions)
	if err != nil || string(out) != string(body) {
		t.Errorf("got %s, %v", out, err)
	}
	if _, err := RewriteReleaseURLs("a", "b", []byte(`{`), testOptions); err == nil {
		t.Error("invalid JSON: want error")
	}
}

const checksum = "a948904f2f0f479b8f8197694b30184b0d2ed1c1cd2a1ec0fb85d299a192a447"

func TestSourceArchive(t *testing.T) {
	upper := strings.ToUpper(checksum)
	body := `{"resources":[{"name":"other","type":"application/zip","checksum":"x"},{"name":"source-archive","type":"text/plain","checksum":"x"},{"name":"source-archive","type":"application/zip","checksum":"` + upper + `","signing":{"signatureBase64Encoded":"c2ln","signatureFormat":"cms-1.0.0"}}]}`
	info, err := SourceArchive([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := ArchiveInfo{Checksum: checksum, Signature: "c2ln", SignatureFormat: "cms-1.0.0"}
	if info != want {
		t.Errorf("SourceArchive = %+v, want %+v", info, want)
	}

	unsigned, err := SourceArchive([]byte(`{"resources":[{"name":"source-archive","type":"application/zip","checksum":"` + checksum + `"}]}`))
	if err != nil || unsigned != (ArchiveInfo{Checksum: checksum}) {
		t.Errorf("unsigned = %+v, %v", unsigned, err)
	}
}

func TestSourceArchiveErrors(t *testing.T) {
	for name, body := range map[string]string{
		"invalid JSON":       `{`,
		"no archive":         `{"resources":[]}`,
		"bad checksum":       `{"resources":[{"name":"source-archive","type":"application/zip","checksum":"abc"}]}`,
		"incomplete signing": `{"resources":[{"name":"source-archive","type":"application/zip","checksum":"` + checksum + `","signing":{"signatureBase64Encoded":"c2ln"}}]}`,
		"signing no sig":     `{"resources":[{"name":"source-archive","type":"application/zip","checksum":"` + checksum + `","signing":{"signatureFormat":"cms-1.0.0"}}]}`,
	} {
		if _, err := SourceArchive([]byte(body)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestChecksumHelpers(t *testing.T) {
	if got, err := NormalizeChecksum(strings.ToUpper(checksum)); err != nil || got != checksum {
		t.Errorf("NormalizeChecksum = %q, %v", got, err)
	}
	for _, bad := range []string{"", "zz", checksum[:62]} {
		if _, err := NormalizeChecksum(bad); err == nil {
			t.Errorf("NormalizeChecksum(%q): want error", bad)
		}
		if got := DigestHeader(bad); got != "" {
			t.Errorf("DigestHeader(%q) = %q", bad, got)
		}
	}
	if got := DigestHeader(checksum); got != "sha-256=qUiQTy8PR5uPgZdpSzAYSw0u0cHNKh7A+4XSmaGSpEc=" {
		t.Errorf("DigestHeader = %q", got)
	}
}

func TestIdentifiers(t *testing.T) {
	for _, tt := range []struct {
		value string
		scope bool
		name  bool
	}{
		{"apple", true, true},
		{"a-b", true, true},
		{"a-b-c", true, true},
		{"a_b-c", false, true},
		{"a_b", false, true},
		{"-a", false, false},
		{"a-", false, false},
		{"a--b", false, false},
		{"a__b", false, false},
		{"a.b", false, false},
		{"", false, false},
		{"Z9", true, true},
		{strings.Repeat("a", MaxScopeLength), true, true},
		{strings.Repeat("a", MaxScopeLength+1), false, true},
		{strings.Repeat("a", MaxNameLength), false, true},
		{strings.Repeat("a", MaxNameLength+1), false, false},
	} {
		if got := ValidScope(tt.value); got != tt.scope {
			t.Errorf("ValidScope(%q) = %v", tt.value, got)
		}
		if got := ValidPackageName(tt.value); got != tt.name {
			t.Errorf("ValidPackageName(%q) = %v", tt.value, got)
		}
	}
}

func TestValidPackageReference(t *testing.T) {
	if !ValidPackageReference("apple", "swift-nio", "1.0.0") {
		t.Error("valid reference refused")
	}
	for _, version := range []string{"", ".", "..", "1/0", `1\0`} {
		if ValidPackageReference("apple", "swift-nio", version) {
			t.Errorf("version %q accepted", version)
		}
	}
	if ValidPackageReference("-a", "swift-nio", "1.0.0") || ValidPackageReference("apple", "a__b", "1.0.0") {
		t.Error("invalid scope or name accepted")
	}
	if s, n := CanonicalPackage("Apple", "Swift-NIO"); s != "apple" || n != "swift-nio" {
		t.Errorf("CanonicalPackage = %q, %q", s, n)
	}
}
