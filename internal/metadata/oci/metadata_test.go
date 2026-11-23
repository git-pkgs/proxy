package oci

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestRewriteLink(t *testing.T) {
	base := LinkOptions{
		ProxyURL:    "https://proxy.example/base/",
		RegistryURL: "https://registry.example",
		RequestPath: "/library/alpine/tags/list",
	}
	withNS := base
	withNS.Namespace = "docker.io"
	badProxy := base
	badProxy.ProxyURL = "://bad"
	badRegistry := base
	badRegistry.RegistryURL = "://bad"
	userProxy := base
	userProxy.ProxyURL = "https://u:p@proxy.example"

	tests := []struct {
		name string
		link string
		o    LinkOptions
		want string
	}{
		{"empty", "", base, ""},
		{"relative", `</v2/library/alpine/tags/list?last=3.19&n=2>; rel="next"`, base,
			`<https://proxy.example/base/v2/library/alpine/tags/list?last=3.19&n=2>; rel="next"`},
		{"query only", `<?last=3.19>; rel="next"`, base,
			`<https://proxy.example/base/v2/library/alpine/tags/list?last=3.19>; rel="next"`},
		{"absolute upstream", `<https://registry.example/v2/other/tags/list?n=1>; rel="next"`, base,
			`<https://proxy.example/base/v2/library/alpine/tags/list?n=1>; rel="next"`},
		{"other host", `<https://elsewhere.example/v2/x?n=1>; rel="next"`, base,
			`<https://elsewhere.example/v2/x?n=1>; rel="next"`},
		{"other scheme", `<http://registry.example/v2/x?n=1>; rel="next"`, base,
			`<http://registry.example/v2/x?n=1>; rel="next"`},
		{"outside v2", `</docs?n=1>; rel="help"`, base, `</docs?n=1>; rel="help"`},
		{"protocol relative", `<//registry.example/v2/x>; rel="next"`, base, `<//registry.example/v2/x>; rel="next"`},
		{"unparseable target", "<%zz>; rel=\"next\"", base, "<%zz>; rel=\"next\""},
		{"namespace kept", `</v2/x?n=1>; rel="next"`, withNS,
			`<https://proxy.example/base/v2/library/alpine/tags/list?n=1&ns=docker.io>; rel="next"`},
		{"bad proxy URL", `</v2/x>; rel="next"`, badProxy, `</v2/x>; rel="next"`},
		{"bad registry URL", `</v2/x>; rel="next"`, badRegistry, `</v2/x>; rel="next"`},
		{"proxy userinfo", `</v2/x>; rel="next"`, userProxy, `<https://u:p@proxy.example/v2/library/alpine/tags/list>; rel="next"`},
		{"two targets", `</v2/a?n=1>; rel="next", <https://elsewhere.example/>; rel="x"`, base,
			`<https://proxy.example/base/v2/library/alpine/tags/list?n=1>; rel="next", <https://elsewhere.example/>; rel="x"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RewriteLink(tt.link, tt.o); got != tt.want {
				t.Errorf("RewriteLink(%q) = %q, want %q", tt.link, got, tt.want)
			}
		})
	}
}

func TestRewriteLinkEncodesRequestPath(t *testing.T) {
	// RequestPath is the decoded request path, which the rewritten target
	// encodes again in place of the upstream's encoded path.
	o := LinkOptions{ProxyURL: "https://proxy.example", RegistryURL: "https://registry.example", RequestPath: "/a b/tags/list"}
	got := RewriteLink(`</v2/x%2Fy/tags/list>`, o)
	if got != `<https://proxy.example/v2/a%20b/tags/list>` {
		t.Errorf("got %q", got)
	}
}

func TestAccepts(t *testing.T) {
	tests := []struct {
		accept, contentType string
		want                bool
	}{
		{ImageManifestMediaType, ImageManifestMediaType, true},
		{ImageManifestMediaType, ImageIndexMediaType, false},
		{"*/*", ImageIndexMediaType, true},
		{"application/*", ImageIndexMediaType, true},
		{"text/*", ImageIndexMediaType, false},
		{"*/json", "application/json", true},
		{"*/xml", "application/json", false},
		{"APPLICATION/VND.OCI.IMAGE.INDEX.V1+JSON", ImageIndexMediaType, true},
		{ImageIndexMediaType + ";q=0", ImageIndexMediaType, false},
		{"*/*, " + ImageIndexMediaType + ";q=0", ImageIndexMediaType, false},
		{"*/*;q=0, " + ImageIndexMediaType, ImageIndexMediaType, true},
		{"application/*;q=0, */*", ImageIndexMediaType, false},
		{"*/*;q=0, application/*", ImageIndexMediaType, true},
		{"application/*, application/json;q=0", "application/json", false},
		{ImageIndexMediaType + ";q=0.5", ImageIndexMediaType, true},
		{ImageIndexMediaType + ";q=2", ImageIndexMediaType, false},
		{ImageIndexMediaType + ";q=-1", ImageIndexMediaType, false},
		{ImageIndexMediaType + ";q=x", ImageIndexMediaType, false},
		{"application/json;charset=utf-8", "application/json", false},
		{"application/json;charset=utf-8", "application/json; charset=utf-8", true},
		{"application/json;charset=utf-8;q=0, application/json", "application/json; charset=utf-8", false},
		{"application/json;q=0, application/json;charset=utf-8", "application/json; charset=utf-8", true},
		{"application/json;a=1;q=0, application/json;a=1;b=2", "application/json; a=1; b=2", true},
		{"application/json;a=1;b=2;q=0, application/json;a=1", "application/json; a=1; b=2", false},
		{"bad;;", ImageIndexMediaType, false},
		{"json", "application/json", false},
		{ImageIndexMediaType, "bad;;", false},
		{ImageIndexMediaType, "nojsontype", false},
		{"", ImageIndexMediaType, false},
	}
	for _, tt := range tests {
		if got := Accepts(tt.accept, tt.contentType); got != tt.want {
			t.Errorf("Accepts(%q, %q) = %v, want %v", tt.accept, tt.contentType, got, tt.want)
		}
	}
}

func TestAcceptsIndex(t *testing.T) {
	for accept, want := range map[string]bool{
		ImageIndexMediaType:         true,
		DockerManifestListMediaType: true,
		ImageManifestMediaType:      false,
		"*/*":                       true,
	} {
		if got := AcceptsIndex(accept); got != want {
			t.Errorf("AcceptsIndex(%q) = %v, want %v", accept, got, want)
		}
	}
}

func TestNames(t *testing.T) {
	tests := []struct {
		accept string
		want   bool
	}{
		{ImageIndexMediaType, true},
		{ImageManifestMediaType + ", " + ImageIndexMediaType, true},
		{"*/*", false},
		{ImageIndexMediaType + ";q=0", false},
		{ImageIndexMediaType + ";q=0, */*", false},
		{"bad;;, " + ImageIndexMediaType, true},
	}
	for _, tt := range tests {
		if got := Names(tt.accept, ImageIndexMediaType); got != tt.want {
			t.Errorf("Names(%q) = %v, want %v", tt.accept, got, tt.want)
		}
	}
}

func TestIsIndexMediaType(t *testing.T) {
	for contentType, want := range map[string]bool{
		ImageIndexMediaType:                 true,
		DockerManifestListMediaType:         true,
		ImageIndexMediaType + "; charset=x": true,
		ImageManifestMediaType:              false,
		"bad;;":                             false,
		"":                                  false,
	} {
		if got := IsIndexMediaType(contentType); got != want {
			t.Errorf("IsIndexMediaType(%q) = %v, want %v", contentType, got, want)
		}
	}
}

func TestNormalizeAccept(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{" , ", ""},
		{"B/X, a/y", "a/y,b/x"},
		{"a/y, a/y", "a/y"},
		{"a/y;q=1", "a/y"},
		{"a/y;q=1.0", "a/y"},
		{"a/y;q=0.50", "a/y;q=0.5"},
		{"a/y;q=bad", "a/y;q=bad"},
		{"a/y;Z=1;b=2", "a/y;b=2;z=1"},
		{"BAD;;", "bad;;"},
		{"a/y, ,", "a/y"},
		{"e/e, d/d, c/c, b/b, a/a, f/f", "a/a,b/b,c/c,d/d,e/e,f/f"},
	}
	for _, tt := range tests {
		if got := NormalizeAccept(tt.in); got != tt.want {
			t.Errorf("NormalizeAccept(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDefaultManifestAccept(t *testing.T) {
	for _, mediaType := range []string{ImageManifestMediaType, ImageIndexMediaType, DockerManifestMediaType, DockerManifestListMediaType, DockerSignedManifestType} {
		if !Names(DefaultManifestAccept, mediaType) {
			t.Errorf("DefaultManifestAccept doesn't name %s", mediaType)
		}
	}
}

func TestManifestRefs(t *testing.T) {
	d := func(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"empty", "", nil},
		{"not JSON", "{", nil},
		{"none", `{"schemaVersion":2}`, nil},
		{"index", `{"manifests":[{"digest":"` + d('a') + `"},{"digest":"` + d('b') + `"}]}`, []string{d('a'), d('b')}},
		{"image", `{"config":{"digest":"` + d('c') + `"},"layers":[{"digest":"` + d('a') + `"}]}`, []string{d('a'), d('c')}},
		{"duplicates and bad digests", `{"layers":[{"digest":"` + d('a') + `"},{"digest":"` + d('a') + `"},{"digest":"md5:x"}]}`, []string{d('a')}},
		{"sha512", `{"layers":[{"digest":"sha512:` + strings.Repeat("f", 128) + `"}]}`, []string{"sha512:" + strings.Repeat("f", 128)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ManifestRefs([]byte(tt.body)); !slices.Equal(got, tt.want) {
				t.Errorf("ManifestRefs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManifestRefsLimit(t *testing.T) {
	build := func(n int) []byte {
		var b strings.Builder
		b.WriteString(`{"layers":[`)
		for i := range n {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"digest":"sha256:`)
			b.WriteString(strings.Repeat("0", 56))
			b.WriteString(strings.Repeat("0", 8-len(strconv.Itoa(i))))
			b.WriteString(strconv.Itoa(i))
			b.WriteString(`"}`)
		}
		b.WriteString("]}")
		return []byte(b.String())
	}
	if got := ManifestRefs(build(MaxManifestRefs)); len(got) != MaxManifestRefs {
		t.Errorf("at the limit: %d refs, want %d", len(got), MaxManifestRefs)
	}
	if got := ManifestRefs(build(MaxManifestRefs + 1)); got != nil {
		t.Errorf("over the limit: %d refs, want none", len(got))
	}
}
