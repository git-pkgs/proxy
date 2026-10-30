package handler

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// compressibleDocuments sets upstream documents with enough versions that
// gzip makes them smaller.
func compressibleDocuments(u *validatorUpstream) {
	var npm, composer []string
	for i := range 50 {
		v := fmt.Sprintf("1.0.%d", i)
		npm = append(npm, `"`+v+`":{"dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-`+v+`.tgz"}}`)
		composer = append(composer, `{"version":"`+v+`","dist":{"type":"zip","url":"https://example.com/pkg-`+v+`.zip"}}`)
	}
	u.set(
		`{"name":"left-pad","versions":{`+strings.Join(npm, ",")+`}}`,
		`{"packages":{"vendor/pkg":[`+strings.Join(composer, ",")+`]}}`,
	)
}

func gunzip(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading gzip body: %v", err)
	}
	return string(body)
}

var acceptGzip = http.Header{"Accept-Encoding": {"gzip, deflate, br"}}

// TestRewrittenMetadataGzip checks that a client accepting gzip gets the
// cached compressed copy of rewritten metadata, on the first fetch and on
// the rewrite fast path, with an ETag of its own so it never matches the
// uncompressed representation.
func TestRewrittenMetadataGzip(t *testing.T) {
	for _, c := range validatorCases {
		for _, ttl := range []time.Duration{time.Hour, 0} {
			t.Run(c.ecosystem+"/ttl="+ttl.String(), func(t *testing.T) {
				u := newValidatorUpstream(t)
				compressibleDocuments(u)
				handler := c.handler(validatorProxy(t, u, ttl), u)

				identity := serve(handler, c.path, nil)
				for range 2 {
					expectGzipOf(t, serve(handler, c.path, acceptGzip), identity)
				}
			})
		}
	}
}

// expectGzipOf checks that rec is the gzip representation of the
// uncompressed response identity.
func expectGzipOf(t *testing.T, rec, identity *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d, Content-Encoding %q; want 200 with gzip", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.Len() >= identity.Body.Len() {
		t.Errorf("gzip body is %d bytes, uncompressed %d", rec.Body.Len(), identity.Body.Len())
	}
	if got := gunzip(t, rec); got != identity.Body.String() {
		t.Errorf("decompressed body differs from the uncompressed response")
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
	if etag := rec.Header().Get("ETag"); etag == "" || etag == identity.Header().Get("ETag") {
		t.Errorf("gzip ETag %q must differ from the uncompressed ETag %q", etag, identity.Header().Get("ETag"))
	}
	if rec.Header().Get("Last-Modified") != identity.Header().Get("Last-Modified") {
		t.Errorf("Last-Modified %q, want %q", rec.Header().Get("Last-Modified"), identity.Header().Get("Last-Modified"))
	}
}

// TestRewrittenMetadataGzipRevalidation checks that each representation
// revalidates with its own ETag, also when a reverse proxy weakened it.
func TestRewrittenMetadataGzipRevalidation(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			compressibleDocuments(u)
			handler := c.handler(validatorProxy(t, u, time.Hour), u)
			gzipETag := serve(handler, c.path, acceptGzip).Header().Get("ETag")
			identityETag := serve(handler, c.path, nil).Header().Get("ETag")

			for _, ifNoneMatch := range []string{gzipETag, "W/" + gzipETag} {
				rec := serve(handler, c.path, http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {ifNoneMatch}})
				expectNotModified(t, rec, gzipETag)
				if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
					t.Errorf("304 Vary = %q, want Accept-Encoding", got)
				}
			}
			if rec := serve(handler, c.path, http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {identityETag}}); rec.Code != http.StatusOK {
				t.Errorf("gzip request revalidating the uncompressed ETag: status %d, want 200", rec.Code)
			}
			if rec := serve(handler, c.path, http.Header{"If-None-Match": {gzipETag}}); rec.Code != http.StatusOK {
				t.Errorf("uncompressed request revalidating the gzip ETag: status %d, want 200", rec.Code)
			}
		})
	}
}

// TestRewrittenMetadataWithoutGzip checks that clients that do not accept
// gzip get the uncompressed document, which still varies on Accept-Encoding.
func TestRewrittenMetadataWithoutGzip(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := newValidatorUpstream(t)
			compressibleDocuments(u)
			handler := c.handler(validatorProxy(t, u, time.Hour), u)

			for _, acceptEncoding := range []string{"", "identity", "gzip;q=0", "br"} {
				header := http.Header{}
				if acceptEncoding != "" {
					header.Set("Accept-Encoding", acceptEncoding)
				}
				rec := serve(handler, c.path, header)
				if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" {
					t.Errorf("Accept-Encoding %q: status %d, Content-Encoding %q; want 200 uncompressed", acceptEncoding, rec.Code, rec.Header().Get("Content-Encoding"))
				}
				if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
					t.Errorf("Accept-Encoding %q: Vary = %q, want Accept-Encoding", acceptEncoding, got)
				}
			}
		})
	}
}

// TestRewrittenMetadataGzipOnlyFromRewriteCache checks that rewrites made
// outside the cache, with it disabled or with cooldown on, stay uncompressed:
// compressing them on every request is better left to a reverse proxy.
func TestRewrittenMetadataGzipOnlyFromRewriteCache(t *testing.T) {
	setups := map[string]func(*Proxy){
		"cache disabled": func(p *Proxy) { p.SetMetadataRewriteCacheSize(0) },
		"cooldown without cache": func(p *Proxy) {
			p.SetMetadataRewriteCacheSize(0)
			p.Cooldown = &cooldown.Config{Default: "3d"}
		},
	}
	for _, c := range validatorCases {
		for name, setup := range setups {
			t.Run(c.ecosystem+"/"+name, func(t *testing.T) {
				u := newValidatorUpstream(t)
				compressibleDocuments(u)
				proxy := validatorProxy(t, u, time.Hour)
				setup(proxy)
				handler := c.handler(proxy, u)

				for range 2 {
					if rec := serve(handler, c.path, acceptGzip); rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" {
						t.Errorf("status %d, Content-Encoding %q; want 200 uncompressed", rec.Code, rec.Header().Get("Content-Encoding"))
					}
				}
			})
		}
	}
}

// TestRewriteCache_KeepsGzipCopyOnlyWhenSmaller checks that the cache keeps
// only a compressed copy of a rewrite, counted against its size, unless
// compressing does not make the rewrite smaller, and that the plain document
// is still there for callers that need it.
func TestRewriteCache_KeepsGzipCopyOnlyWhenSmaller(t *testing.T) {
	c := newRewriteCache(1 << 20)
	identity := func(b []byte) ([]byte, error) { return b, nil }
	large := []byte(strings.Repeat(`{"version":"1.0.0"},`, 100))

	for _, doc := range [][]byte{large, []byte("abc")} {
		key := rewriteCacheKey("npm", "http://proxy", string(doc[:3]), doc)
		if _, err := c.rewrite(t.Context(), key, doc, 0, identity); err != nil {
			t.Fatal(err)
		}
	}

	m, ok := c.get(rewriteCacheKey("npm", "http://proxy", string(large[:3]), large))
	if !ok || m.gzipped == nil {
		t.Fatalf("large rewrite: cached %v, gzip copy %d bytes; want a gzip copy", ok, len(m.gzipped))
	}
	if m.plain != nil {
		t.Errorf("large rewrite: cached %d plain bytes besides the gzip copy, want none", len(m.plain))
	}
	if c.size != int64(len(m.gzipped)+3) {
		t.Errorf("cache size = %d, want the gzip copy and the small rewrite (%d)", c.size, len(m.gzipped)+3)
	}
	if !bytes.Equal(m.content(), large) || m.size != len(large) {
		t.Errorf("large rewrite content = %d bytes, size %d; want the %d bytes rewritten", len(m.content()), m.size, len(large))
	}
	if small, ok := c.get(rewriteCacheKey("npm", "http://proxy", "abc", []byte("abc"))); !ok || small.gzipped != nil || string(small.content()) != "abc" {
		t.Errorf("small rewrite: cached %v, gzip copy %d bytes, content %q; want abc uncompressed", ok, len(small.gzipped), small.content())
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := map[string]bool{
		"":                        false,
		"identity":                false,
		"br, deflate":             false,
		"gzip":                    true,
		"GZIP":                    true,
		"x-gzip":                  true,
		"deflate, gzip;q=1.0":     true,
		"gzip; q=0.5":             true,
		"gzip;q=0":                false,
		"gzip;q=0.000":            false,
		"*":                       true,
		"*;q=0":                   false,
		"br;q=1, *;q=0.5":         true,
		"gzip;q=0, *":             false,
		"gzip;q=invalid, deflate": false,
	}
	for header, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Accept-Encoding", header)
		}
		if got := acceptsGzip(r); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}
