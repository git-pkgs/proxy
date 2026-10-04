package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/denylist"
)

func setTestDenylist(t testing.TB, p *Proxy, packages ...string) {
	t.Helper()
	var err error
	p.Denylist, err = denylist.New(packages)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDenylistBlocksArtifactAccess(t *testing.T) {
	for _, tc := range []struct{ cached, direct bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		cached, direct := tc.cached, tc.direct
		t.Run(fmt.Sprintf("cached=%t/direct=%t", cached, direct), func(t *testing.T) {
			p, db, store, _ := setupTestProxy(t)
			if cached {
				seedPackage(t, db, store, "npm", "demo", "1.0.0", "demo.tgz", "content")
			}
			p.DirectServe = direct
			store.signedURL = "https://storage.example/demo"
			setTestDenylist(t, p, "pkg:npm/demo@1.0.0")
			ctx := context.Background()
			for _, get := range []func() (*CacheResult, error){
				func() (*CacheResult, error) { return p.GetOrFetchArtifact(ctx, "npm", "demo", "1.0.0", "demo.tgz") },
				func() (*CacheResult, error) { return p.GetCachedArtifact(ctx, "npm", "demo", "1.0.0", "demo.tgz") },
				func() (*CacheResult, error) {
					return p.GetOrFetchArtifactFromURL(ctx, "npm", "demo", "1.0.0", "demo.tgz", "https://upstream.invalid/demo")
				},
				func() (*CacheResult, error) {
					return p.GetOrFetchArtifactFromURLWithHeaders(ctx, "npm", "demo", "1.0.0", "demo.tgz", "https://upstream.invalid/demo", nil)
				},
				func() (*CacheResult, error) {
					return p.GetOrFetchArtifactFromURLWithDigest(ctx, "npm", "demo", "1.0.0", "demo.tgz", "https://upstream.invalid/demo", "sha256:abc")
				},
			} {
				result, err := get()
				if result != nil || !errors.Is(err, ErrVersionDenied) {
					t.Fatalf("result=%+v err=%v; want denial", result, err)
				}
				w := httptest.NewRecorder()
				p.serveArtifactError(w, err, "fetch failed")
				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d", w.Code)
				}
			}
			// Denying does not purge the cache. Removing the policy restores it.
			p.Denylist = nil
			result, err := p.GetCachedArtifact(ctx, "npm", "demo", "1.0.0", "demo.tgz")
			if err != nil || (result != nil) != cached {
				t.Fatalf("cache changed: result=%+v err=%v", result, err)
			}
			if result != nil && result.Reader != nil {
				_ = result.Reader.Close()
			}
		})
	}
}

func TestNPMDenylistMetadata(t *testing.T) {
	p := testProxy()
	setTestDenylist(t, p, "pkg:npm/@scope/demo@2.0.0")
	h := NewNPMHandler(p, "https://proxy.example", "")
	for _, times := range []string{"", `,"time":{"1.0.0":"2020-01-01T00:00:00Z","2.0.0":"2021-01-01T00:00:00Z"}`} {
		body := `{"versions":{"1.0.0":{},"2.0.0":{},"3.0.0-beta.1":{}},"dist-tags":{"latest":"2.0.0","beta":"2.0.0","next":"3.0.0-beta.1"}` + times + `}`
		out, err := h.rewriteMetadata("@scope/demo", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "2.0.0") || !strings.Contains(string(out), `"latest":"1.0.0"`) || !strings.Contains(string(out), `"next":"3.0.0-beta.1"`) {
			t.Fatalf("incorrect metadata: %s", out)
		}
	}
	out, err := h.rewriteMetadata("@scope/demo", []byte(`{"versions":{"2.0.0":{}},"dist-tags":{"latest":"2.0.0"}}`))
	if err != nil || strings.Contains(string(out), "2.0.0") {
		t.Fatalf("all denied: %s, %v", out, err)
	}
}

func TestCargoDenylistWithoutTimestamps(t *testing.T) {
	p := testProxy()
	setTestDenylist(t, p, "pkg:cargo/demo@1.0.0")
	h := &CargoHandler{proxy: p}
	input := "{\"name\":\"demo\",\"vers\":\"1.0.0\"}\n{\"name\":\"demo\",\"vers\":\"2.0.0\"}\n"
	for _, cd := range []CooldownPolicy{nil, &cooldown.Config{Default: "3d"}} {
		p.Cooldown = cd
		w := httptest.NewRecorder()
		h.applyCooldownFiltering(w, []byte(input))
		if w.Body.String() != "{\"name\":\"demo\",\"vers\":\"2.0.0\"}\n" {
			t.Fatalf("index: %s", w.Body.String())
		}
	}
}

func TestPyPIDenylistSimpleRepresentations(t *testing.T) {
	for _, tc := range []struct{ name, contentType, body string }{
		{"html", "text/html", `<html><a href='https://files.pythonhosted.org/packages/a/b/demo-1.0.0.tar.gz#sha256=abc'><b>download</b></a><a href="https://files.pythonhosted.org/packages/a/b/demo-2.0.0.tar.gz">demo-2.0.0.tar.gz</a></html>`},
		{"json", pypiSimpleJSON, `{"meta":{"api-version":"1.0"},"files":[{"filename":"demo-1.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/a/b/demo-1.0.0.tar.gz"},{"filename":"demo-2.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/a/b/demo-2.0.0.tar.gz"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/simple/demo/" {
					t.Fatalf("denylist must not need timestamp metadata: %s", r.URL.Path)
				}
				return pypiHTTPResponse(r, tc.contentType, tc.body), nil
			})
			p.CacheMetadata = true
			p.MetadataTTL = time.Hour
			// Seed raw metadata before configuring the denylist.
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/simple/demo/", nil))
			setTestDenylist(t, p, "pkg:pypi/Demo@1.0.0")
			p.HTTPClient = &http.Client{Transport: pypiRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("offline")
			})}
			for _, ttl := range []time.Duration{time.Hour, 0} {
				p.MetadataTTL = ttl
				w = httptest.NewRecorder()
				h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/simple/demo/", nil))
				if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "1.0.0") || !strings.Contains(w.Body.String(), "2.0.0") {
					t.Fatalf("ttl=%s status=%d body=%s", ttl, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestPyPIDenylistJSONMetadata(t *testing.T) {
	p := testProxy()
	setTestDenylist(t, p, "pkg:pypi/demo@1.0.0")
	h := NewPyPIHandler(p, "https://proxy.example")
	body := `{"info":{"name":"Demo","version":"1.0.0"},"releases":{"1.0.0":[{"url":"https://files.pythonhosted.org/packages/demo-1.0.0.tar.gz"}],"2.0.0":[]},"urls":[{"url":"https://files.pythonhosted.org/packages/demo-1.0.0.tar.gz"}]}`
	out, err := h.rewriteJSONMetadata([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(out, &metadata); err != nil {
		t.Fatal(err)
	}
	if _, exists := metadata["releases"].(map[string]any)["1.0.0"]; exists || len(metadata["urls"].([]any)) != 0 {
		t.Fatalf("denied release survived: %s", out)
	}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pypi/demo/1.0.0/json", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("denied version metadata status=%d", w.Code)
	}
}

func TestDenylistDownloadRoutes(t *testing.T) {
	for _, tc := range []struct {
		ecosystem, path, filename string
		handler                   func(*Proxy) http.Handler
	}{
		{"npm", "/demo/-/demo-1.0.0.tgz", "demo-1.0.0.tgz", func(p *Proxy) http.Handler { return NewNPMHandler(p, "http://proxy.test", "").Routes() }},
		{"pypi", "/packages/a/b/demo-1.0.0.tar.gz", "demo-1.0.0.tar.gz", func(p *Proxy) http.Handler { return NewPyPIHandler(p, "http://proxy.test").Routes() }},
		{"cargo", "/crates/demo/1.0.0/download", "demo-1.0.0.crate", func(p *Proxy) http.Handler { return NewCargoHandler(p, "http://proxy.test", "", "").Routes() }},
		{"deb", "/pool/main/d/demo/demo_1.0.0_amd64.deb", "demo_1.0.0_amd64.deb", func(p *Proxy) http.Handler { return NewDebianHandler(p, "http://proxy.test", "", nil).Routes() }},
	} {
		t.Run(tc.ecosystem, func(t *testing.T) {
			p, db, store, _ := setupTestProxy(t)
			seedPackage(t, db, store, tc.ecosystem, "demo", "1.0.0", tc.filename, "cached content")
			setTestDenylist(t, p, "pkg:"+tc.ecosystem+"/demo@1.0.0")
			w := httptest.NewRecorder()
			tc.handler(p).ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "denylist") {
				t.Fatalf("denied download status=%d body=%s", w.Code, w.Body.String())
			}
			// An allowed version remains readable while the policy is enabled.
			filename := strings.ReplaceAll(tc.filename, "1.0.0", "2.0.0")
			seedPackage(t, db, store, tc.ecosystem, "demo", "2.0.0", filename, "allowed content")
			result, err := p.GetCachedArtifact(context.Background(), tc.ecosystem, "demo", "2.0.0", filename)
			if err != nil || result == nil {
				t.Fatalf("allowed download: result=%v err=%v", result, err)
			}
			defer func() { _ = result.Reader.Close() }()
			body, err := io.ReadAll(result.Reader)
			if err != nil || string(body) != "allowed content" {
				t.Fatalf("allowed body=%s err=%v", body, err)
			}
		})
	}
}

func TestDenylistAndCooldownCompose(t *testing.T) {
	p := testProxy()
	p.Cooldown = &cooldown.Config{Default: "3d"}
	setTestDenylist(t, p, "pkg:npm/demo@1.0.0")
	h := NewNPMHandler(p, "https://proxy.test", "")
	body := fmt.Sprintf(`{"versions":{"1.0.0":{},"2.0.0":{},"3.0.0":{}},"time":{"1.0.0":"2020-01-01T00:00:00Z","2.0.0":"2021-01-01T00:00:00Z","3.0.0":%q},"dist-tags":{"latest":"3.0.0"}}`, time.Now().UTC().Format(time.RFC3339))
	out, err := h.rewriteMetadata("demo", []byte(body))
	if err != nil || strings.Contains(string(out), "1.0.0") || strings.Contains(string(out), "3.0.0") || !strings.Contains(string(out), `"latest":"2.0.0"`) {
		t.Fatalf("combined policy: %s, %v", out, err)
	}
}

func TestCargoDenylistLargeEntry(t *testing.T) {
	p := testProxy()
	setTestDenylist(t, p, "pkg:cargo/demo@1.0.0")
	h := &CargoHandler{proxy: p}
	line := fmt.Sprintf("{\"name\":\"demo\",\"vers\":\"2.0.0\",\"extra\":%q}\n", strings.Repeat("x", 70<<10))
	w := httptest.NewRecorder()
	h.applyCooldownFiltering(w, []byte(line))
	if w.Body.String() != line {
		t.Fatal("large allowed index entry was truncated")
	}
}

func TestDenylistMalformedMetadataFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, contentType string
		handler                       func(*Proxy) http.Handler
	}{
		{"npm", "/demo", `{"versions":`, contentTypeJSON, func(p *Proxy) http.Handler { return NewNPMHandler(p, "https://proxy.test", "").Routes() }},
		{"pypi-json", "/pypi/demo/json", `{"releases":`, contentTypeJSON, func(p *Proxy) http.Handler { return NewPyPIHandler(p, "https://proxy.test").Routes() }},
		{"pypi-simple", "/simple/demo/", `{"files":{}}`, pypiSimpleJSON, func(p *Proxy) http.Handler { return NewPyPIHandler(p, "https://proxy.test").Routes() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProxy()
			setTestDenylist(t, p, "pkg:npm/demo@1.0.0", "pkg:pypi/demo@1.0.0")
			p.HTTPClient = &http.Client{Transport: pypiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				return pypiHTTPResponse(r, tc.contentType, tc.body), nil
			})}
			w := httptest.NewRecorder()
			tc.handler(p).ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
