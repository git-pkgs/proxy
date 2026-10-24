package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestConanMetadataKeysKeepReferencesApart checks that Conan references whose
// paths only differ in where '/' and '_' fall, or in a query, are cached
// apart. Names and versions may contain '_'.
func TestConanMetadataKeysKeepReferencesApart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path+"?"+r.URL.RawQuery)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	srv := httptest.NewServer(NewConanHandlerWithUpstream(proxy, testProxyURL, upstream.URL).Routes())
	defer srv.Close()

	for _, pair := range [][2]string{
		{"/v2/conans/foo/bar_1.0/_/_/latest", "/v2/conans/foo_bar/1.0/_/_/latest"},
		{"/v2/conans/foo/bar_1.0/_/_/revisions/r1/files/conanfile.py", "/v2/conans/foo_bar/1.0/_/_/revisions/r1/files/conanfile.py"},
		{"/v2/conans/search?q=zlib", "/v2/conans/search_q=zlib"},
	} {
		for _, target := range pair {
			resp, err := http.Get(srv.URL + target)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			want := target
			if !strings.Contains(want, "?") {
				want += "?"
			}
			if string(body) != want {
				t.Errorf("GET %s served %q, want %q", target, body, want)
			}
		}
	}
}

// TestMetadataCacheKeysUnambiguous checks that the path-based metadata cache
// keys of the other ecosystems keep paths apart that differ only in where
// '/' and '_' fall.
func TestMetadataCacheKeysUnambiguous(t *testing.T) {
	const a, b = "x/foo/bar_1.0/file", "x/foo_bar/1.0/file"

	maven := mavenSource{upstreamURL: "https://repo.test"}
	debian := NewDebianHandler(nil, "http://proxy.test", "https://deb.test", nil)
	rpm := NewRPMHandlerWithUpstream(nil, "http://proxy.test", "https://rpm.test")
	cran := NewCRANHandlerWithUpstream(nil, "http://proxy.test", "https://cran.test")
	conda := NewCondaHandlerWithUpstream(nil, "http://proxy.test", "https://conda.test")

	for name, key := range map[string]func(string) string{
		"maven":  maven.metadataCacheKey,
		"debian": func(p string) string { return debian.metadataCacheKeyFor("", debian.upstreamURL, p) },
		"rpm":    func(p string) string { return rpm.metadataCacheKey(p) },
		"cran":   func(p string) string { return cran.metadataCacheKey("/" + p) },
		"conda":  func(p string) string { return conda.metadataCacheKey("/" + p) },
	} {
		if key(a) == key(b) {
			t.Errorf("%s: %s and %s share metadata cache key %q", name, a, b, key(a))
		}
	}
}
