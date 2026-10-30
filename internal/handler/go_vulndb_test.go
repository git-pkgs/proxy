package handler

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	testVulnDBDB      = `{"modified":"2026-10-21T00:27:29Z"}`
	testVulnDBModules = `[{"path":"golang.org/x/text","vulns":[{"id":"GO-2021-0113","modified":"2026-10-21T00:27:29Z","fixed":"0.3.7"}]}]`
	testVulnDBEntry   = `{"schema_version":"1.3.1","id":"GO-2021-0113","modified":"2026-10-21T00:27:29Z"}`
)

func gzipString(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(zw, s); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// testVulnDBFiles is a small vulnerability database laid out as vuln.go.dev
// serves it: every file as .json and .json.gz, the latter as gzip data with a
// JSON content type and no Content-Encoding.
func testVulnDBFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{
		"/index/db.json":               testVulnDBDB,
		"/index/modules.json":          testVulnDBModules,
		"/index/vulns.json":            `[{"id":"GO-2021-0113","modified":"2026-10-21T00:27:29Z"}]`,
		"/ID/GO-2021-0113.json":        testVulnDBEntry,
		"/ID/GHSA-ppp9-7jff-5vj2.json": testVulnDBEntry,
	}
	all := make(map[string]string, 2*len(files))
	for path, body := range files {
		all[path] = body
		all[path+".gz"] = gzipString(t, body)
	}
	return all
}

func newVulnDBTestHandler(t *testing.T, vulnDBURL string, cacheMetadata bool, ttl time.Duration) *GoHandler {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	proxy.CacheMetadata = cacheMetadata
	proxy.MetadataTTL = ttl
	h := NewGoHandler(proxy, "http://proxy.example")
	h.SetVulnDB(vulnDBURL)
	return h
}

func TestGoVulnDB_DefaultsToVulnGoDev(t *testing.T) {
	h := NewGoHandler(nil, "http://proxy.example")
	if h.vulnDBURL != defaultGoVulnDBURL {
		t.Errorf("vulnDBURL = %q, want %q", h.vulnDBURL, defaultGoVulnDBURL)
	}
	h.SetVulnDB("")
	if h.vulnDBURL != defaultGoVulnDBURL {
		t.Errorf("after SetVulnDB(\"\"): vulnDBURL = %q, want %q", h.vulnDBURL, defaultGoVulnDBURL)
	}
	h.SetVulnDB("https://vulndb.example/")
	if h.vulnDBURL != "https://vulndb.example" {
		t.Errorf("vulnDBURL = %q, want the trailing slash trimmed", h.vulnDBURL)
	}
}

func TestGoVulnDB_ServesFilesUnchanged(t *testing.T) {
	files := testVulnDBFiles(t)
	for _, cacheMetadata := range []bool{true, false} {
		db := newFakeSumDB(t, files)
		h := newVulnDBTestHandler(t, db.server.URL, cacheMetadata, time.Hour)
		for path, want := range files {
			w := serveGoRequest(h, "/vulndb"+path)
			if w.Code != http.StatusOK || w.Body.String() != want {
				t.Errorf("cache_metadata=%v %s: status = %d, body = %q, want 200 with the upstream bytes", cacheMetadata, path, w.Code, w.Body.String())
			}
			if enc := w.Header().Get(headerContentEncoding); enc != "" {
				t.Errorf("cache_metadata=%v %s: Content-Encoding = %q, want none", cacheMetadata, path, enc)
			}
		}
	}
}

func TestGoVulnDB_HeadForSchemaCheck(t *testing.T) {
	db := newFakeSumDB(t, testVulnDBFiles(t))
	h := newVulnDBTestHandler(t, db.server.URL, true, time.Hour)

	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/vulndb/index/modules.json.gz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("HEAD index/modules.json.gz: status = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/golang.org/x/text/@v/list", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("HEAD on a module path: status = %d, want 405", w.Code)
	}
}

func TestGoVulnDB_CachedWithinTTL(t *testing.T) {
	db := newFakeSumDB(t, testVulnDBFiles(t))
	h := newVulnDBTestHandler(t, db.server.URL, true, time.Hour)

	for range 3 {
		if w := serveGoRequest(h, "/vulndb/index/modules.json.gz"); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	}
	if n := db.requests("/index/modules.json.gz"); n != 1 {
		t.Errorf("upstream requests = %d, want 1", n)
	}
}

func TestGoVulnDB_RefreshedAfterTTL(t *testing.T) {
	db := newFakeSumDB(t, testVulnDBFiles(t))
	h := newVulnDBTestHandler(t, db.server.URL, true, time.Nanosecond)

	serveGoRequest(h, "/vulndb/ID/GO-2021-0113.json")
	updated := `{"id":"GO-2021-0113","modified":"2026-10-27T00:00:00Z"}`
	db.setFile("/ID/GO-2021-0113.json", updated)
	if w := serveGoRequest(h, "/vulndb/ID/GO-2021-0113.json"); w.Body.String() != updated {
		t.Errorf("body = %q, want the modified entry %q", w.Body.String(), updated)
	}
}

func TestGoVulnDB_StaleFallbackWhenUnreachable(t *testing.T) {
	files := testVulnDBFiles(t)
	db := newFakeSumDB(t, files)
	h := newVulnDBTestHandler(t, db.server.URL, true, time.Nanosecond)

	for path := range files {
		if w := serveGoRequest(h, "/vulndb"+path); w.Code != http.StatusOK {
			t.Fatalf("%s online: status = %d, want 200", path, w.Code)
		}
	}

	db.down.Store(true)
	for path, want := range files {
		w := serveGoRequest(h, "/vulndb"+path)
		if w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s offline: status = %d, body = %q, want 200 with the cached copy", path, w.Code, w.Body.String())
		}
		if w.Header().Get("Warning") == "" {
			t.Errorf("%s offline: no Warning header, want the response marked stale", path)
		}
	}
	if w := serveGoRequest(h, "/vulndb/ID/GO-2099-0001.json.gz"); w.Code != http.StatusBadGateway {
		t.Errorf("uncached entry offline: status = %d, want 502", w.Code)
	}
}

func TestGoVulnDB_RefusesNonJSON(t *testing.T) {
	const blockPage = "<html>blocked by policy</html>"
	db := newFakeSumDB(t, map[string]string{
		"/index/db.json":           blockPage,
		"/index/db.json.gz":        blockPage,
		"/index/modules.json.gz":   gzipString(t, blockPage),
		"/ID/GO-2021-0113.json.gz": gzipString(t, testVulnDBEntry)[:20],
	})
	h := newVulnDBTestHandler(t, db.server.URL, true, time.Hour)

	for _, path := range []string{"/index/db.json", "/index/db.json.gz", "/index/modules.json.gz", "/ID/GO-2021-0113.json.gz"} {
		if w := serveGoRequest(h, "/vulndb"+path); w.Code != http.StatusBadGateway {
			t.Errorf("%s: status = %d, want 502", path, w.Code)
		}
	}

	// Nothing bad was cached: once upstream serves the real file, it is used.
	db.setFile("/index/db.json", testVulnDBDB)
	if w := serveGoRequest(h, "/vulndb/index/db.json"); w.Code != http.StatusOK || w.Body.String() != testVulnDBDB {
		t.Errorf("after fix: status = %d, body = %q, want 200 with %q", w.Code, w.Body.String(), testVulnDBDB)
	}
}

func TestGoVulnDB_NotFound(t *testing.T) {
	db := newFakeSumDB(t, nil)
	h := newVulnDBTestHandler(t, db.server.URL, true, time.Hour)
	if w := serveGoRequest(h, "/vulndb/ID/GO-2099-0001.json.gz"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGoVulnDB_InvalidPathsNeverReachUpstream(t *testing.T) {
	db := newFakeSumDB(t, nil)
	h := newVulnDBTestHandler(t, db.server.URL, true, time.Hour)

	for _, target := range []string{
		"/vulndb",
		"/vulndb/",
		"/vulndb/index",
		"/vulndb/index/db",
		"/vulndb/index/db.json.zip",
		"/vulndb/index/other.json",
		"/vulndb/vulndb.zip",
		"/vulndb/ID/",
		"/vulndb/ID/.json",
		"/vulndb/ID/GO-2021-0113",
		"/vulndb/ID/GO-2021-0113.txt",
		"/vulndb/ID/../index/db.json",
		"/vulndb/ID/..%2Findex%2Fdb.json",
		"/vulndb/ID/GO.2021.json",
		"/vulndb/ID/a/b.json",
		"/vulndb/ID/GO%3F.json",
		"/vulndb/ID/" + string(bytes.Repeat([]byte("A"), goVulnDBMaxIDLength+1)) + ".json",
	} {
		if w := serveGoRequest(h, target); w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, w.Code)
		}
	}
	if n := db.totalRequests(); n != 0 {
		t.Errorf("upstream requests = %d, want 0", n)
	}
}
