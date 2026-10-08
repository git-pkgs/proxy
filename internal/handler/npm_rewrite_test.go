package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// TestNPMRewriteMetadataKeepsUpstreamBytes checks that only tarball URLs
// change: everything else, including numbers encoding/json would round
// through float64 and characters it would HTML-escape, is copied verbatim.
func TestNPMRewriteMetadataKeepsUpstreamBytes(t *testing.T) {
	h := &NPMHandler{proxy: testProxy(), proxyURL: "http://proxy"}
	input := `{"name":"demo","_big":12345678901234567890,"readme":"<b>&</b>",` +
		`"versions":{"1.0.0":{"version":"1.0.0","dist":{"shasum":"s","tarball":"https://registry.npmjs.org/demo/-/demo-1.0.0.tgz","integrity":"i"}}}}`

	out, err := h.rewriteMetadata("demo", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(input, "https://registry.npmjs.org/demo/-/demo-1.0.0.tgz", "http://proxy/npm/demo/-/demo-1.0.0.tgz", 1)
	if string(out) != want {
		t.Errorf("rewrite =\n%s\nwant\n%s", out, want)
	}
}

func TestNPMRewriteMetadataUnusualEntries(t *testing.T) {
	h := &NPMHandler{proxy: testProxy(), proxyURL: "http://proxy"}
	input := `{
		"versions": {
			"1.0.0": "not an object",
			"2.0.0": {"dist": "no object either"},
			"3.0.0": {"dist": {"tarball": 7}},
			"4.0.0": {"dist": {"tarball": "https://registry.npmjs.org/demo/-/renamed.tgz"}},
			"5.0.0": {"dist": {}}
		}
	}`
	out, err := h.rewriteMetadata("demo", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	for v, want := range map[string]string{
		"1.0.0": `"not an object"`,
		"2.0.0": `{"dist": "no object either"}`,
		"3.0.0": `{"dist": {"tarball": 7}}`,
		"4.0.0": `{"dist": {"tarball": "http://proxy/npm/demo/-/demo-4.0.0.tgz"}}`,
		"5.0.0": `{"dist": {}}`,
	} {
		if got := string(doc.Versions[v]); got != want {
			t.Errorf("%s = %s, want %s", v, got, want)
		}
	}
}

func TestNPMRewriteMetadataInvalidJSON(t *testing.T) {
	h := &NPMHandler{proxy: testProxy(), proxyURL: "http://proxy"}
	for _, input := range []string{`{"versions": {}`, `[1]`, `{"versions": {}} trailing`} {
		if _, err := h.rewriteMetadata("demo", []byte(input)); err == nil {
			t.Errorf("%s: expected an error", input)
		}
	}
}

func TestNPMRewriteMetadataNoVersions(t *testing.T) {
	h := &NPMHandler{proxy: testProxy(), proxyURL: "http://proxy"}
	input := `{"name":"demo","versions":[]}`
	out, err := h.rewriteMetadata("demo", []byte(input))
	if err != nil || string(out) != input {
		t.Errorf("rewrite = %s, %v; want input unchanged", out, err)
	}

	setTestDenylist(t, h.proxy, "pkg:npm/demo@1.0.0")
	if _, err := h.rewriteMetadata("demo", []byte(input)); err == nil {
		t.Error("expected an error when a denylisted package has no versions object")
	}
}

// TestNPMRewriteMetadataCooldownAndDenylist runs both filters together and
// checks that versions, their time entries and the tags pointing at them are
// all removed, with latest moved to the newest version left.
func TestNPMRewriteMetadataCooldownAndDenylist(t *testing.T) {
	now := time.Now()
	ts := func(age time.Duration) string { return now.Add(-age).UTC().Format(time.RFC3339) }

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	setTestDenylist(t, proxy, "pkg:npm/demo@2.0.0")
	h := &NPMHandler{proxy: proxy, proxyURL: "http://proxy"}

	version := func(v string) string {
		return fmt.Sprintf(`%q:{"version":%q,"dist":{"tarball":"https://registry.npmjs.org/demo/-/demo-%s.tgz"}}`, v, v, v)
	}
	input := `{"name":"demo",` +
		`"dist-tags":{"latest":"3.0.0","beta":"2.0.0","next":"1.0.0"},` +
		`"versions":{` + version("1.0.0") + `,` + version("2.0.0") + `,` + version("3.0.0") + `},` +
		`"time":{"created":"` + ts(1000*time.Hour) + `","1.0.0":"` + ts(900*time.Hour) + `","2.0.0":"` + ts(800*time.Hour) + `","3.0.0":"` + ts(time.Hour) + `"}}`

	out, err := h.rewriteMetadata("demo", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		DistTags map[string]string          `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
		Time     map[string]string          `json:"time"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(doc.Versions) != 1 || doc.Versions["1.0.0"] == nil {
		t.Errorf("versions = %v, want only 1.0.0", keysOf(doc.Versions))
	}
	if _, ok := doc.Time["2.0.0"]; ok {
		t.Error("time still lists denylisted 2.0.0")
	}
	if _, ok := doc.Time["3.0.0"]; ok {
		t.Error("time still lists 3.0.0, which is inside cooldown")
	}
	if _, ok := doc.Time["created"]; !ok {
		t.Error("time lost its created entry")
	}
	want := map[string]string{"latest": "1.0.0", "next": "1.0.0"}
	if fmt.Sprint(doc.DistTags) != fmt.Sprint(want) {
		t.Errorf("dist-tags = %v, want %v", doc.DistTags, want)
	}
	if !strings.Contains(string(doc.Versions["1.0.0"]), "http://proxy/npm/demo/-/demo-1.0.0.tgz") {
		t.Errorf("1.0.0 tarball not rewritten: %s", doc.Versions["1.0.0"])
	}
}

// TestNPMRewriteMetadataDuplicateKeys checks that a repeated top-level key
// cannot smuggle an unfiltered copy of the versions past the filters.
func TestNPMRewriteMetadataDuplicateKeys(t *testing.T) {
	proxy := testProxy()
	setTestDenylist(t, proxy, "pkg:npm/demo@2.0.0")
	h := &NPMHandler{proxy: proxy, proxyURL: "http://proxy"}
	input := `{"versions":{"2.0.0":{"dist":{"tarball":"https://registry.npmjs.org/demo/-/demo-2.0.0.tgz"}}},` +
		`"versions":{"1.0.0":{"dist":{"tarball":"https://registry.npmjs.org/demo/-/demo-1.0.0.tgz"}},"2.0.0":{"dist":{"tarball":"https://registry.npmjs.org/demo/-/demo-2.0.0.tgz"}}}}`
	out, err := h.rewriteMetadata("demo", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "2.0.0") || strings.Count(string(out), `"versions"`) != 1 {
		t.Errorf("rewrite = %s", out)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestMetadataServesRewriteWithoutReadingStorage checks that a repeat
// request inside the metadata TTL is answered from the rewrite cache by the
// stored digest alone, without reading the cached document back.
func TestMetadataServesRewriteWithoutReadingStorage(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/left-pad":
			_, _ = w.Write([]byte(`{"name":"left-pad","versions":{"1.3.0":{"dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}}}}`))
		case "/p2/vendor/pkg.json":
			_, _ = w.Write([]byte(`{"packages":{"vendor/pkg":[{"version":"1.0.0","dist":{"type":"zip","url":"https://example.com/pkg-1.0.0.zip"}}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cases := []struct {
		ecosystem, name, path string
		handler               func(*Proxy) http.Handler
	}{
		{"npm", "left-pad", "/left-pad", func(p *Proxy) http.Handler {
			return NewNPMHandler(p, "http://proxy.example", upstream.URL).Routes()
		}},
		{"composer", "vendor/pkg", "/p2/vendor/pkg.json", func(p *Proxy) http.Handler {
			return NewComposerHandlerWithUpstreams(p, "http://proxy.example", upstream.URL, upstream.URL).Routes()
		}},
	}
	for _, c := range cases {
		t.Run(c.ecosystem, func(t *testing.T) {
			upstreamCalls.Store(0)
			proxy, db, store, _ := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Hour
			proxy.SetMetadataRewriteCacheSize(1 << 20)
			handler := c.handler(proxy)

			get := func() string {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
				return rec.Body.String()
			}

			first := get()
			entry, err := db.GetMetadataCache(c.ecosystem, c.name)
			if err != nil || entry == nil || !strings.HasPrefix(entry.ContentDigest.String, "sha256:") {
				t.Fatalf("metadata row = %+v, %v; want a sha256 content digest", entry, err)
			}

			store.mu.Lock()
			store.openErr = errors.New("storage must not be read")
			store.mu.Unlock()

			if second := get(); second != first {
				t.Errorf("second response %q differs from first %q", second, first)
			}
			if got := upstreamCalls.Load(); got != 1 {
				t.Errorf("upstream calls = %d, want 1", got)
			}
		})
	}
}

// TestNPMStoredRewriteSkippedWhenStale checks that the digest fast path is
// only taken inside the metadata TTL.
func TestNPMStoredRewriteSkippedWhenStale(t *testing.T) {
	proxy, db, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.SetMetadataRewriteCacheSize(1 << 20)

	body := []byte(`{}`)
	proxy.cacheMetadataBlob(t.Context(), "npm", "demo", metadataStoragePath("npm", "demo"), &upstreamMetadata{body: body})
	key := rewriteCacheKey("npm", "http://proxy", "demo", body)
	if _, err := proxy.rewrites.rewrite(t.Context(), key, body, func(b []byte) ([]byte, error) { return b, nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := proxy.storedRewrite("npm", "demo", "http://proxy", "demo"); !ok {
		t.Fatal("fresh row: expected a stored rewrite")
	}

	entry, _ := db.GetMetadataCache("npm", "demo")
	entry.FetchedAt.Time = time.Now().Add(-2 * time.Hour)
	if err := db.UpsertMetadataCache(entry); err != nil {
		t.Fatal(err)
	}
	if _, ok := proxy.storedRewrite("npm", "demo", "http://proxy", "demo"); ok {
		t.Error("stale row: expected no stored rewrite")
	}

	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	entry.FetchedAt.Time = time.Now()
	_ = db.UpsertMetadataCache(entry)
	if _, ok := proxy.storedRewrite("npm", "demo", "http://proxy", "demo"); ok {
		t.Error("cooldown on: expected no stored rewrite")
	}
}
