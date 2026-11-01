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
	"github.com/git-pkgs/registries/fetch"
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
		out, _, err := h.rewriteMetadataKeeping("@scope/demo", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "2.0.0") || !strings.Contains(string(out), `"latest":"1.0.0"`) || !strings.Contains(string(out), `"next":"3.0.0-beta.1"`) {
			t.Fatalf("incorrect metadata: %s", out)
		}
	}
	out, _, err := h.rewriteMetadataKeeping("@scope/demo", []byte(`{"versions":{"2.0.0":{}},"dist-tags":{"latest":"2.0.0"}}`))
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
	h := NewPyPIHandlerWithUpstreams(p, "https://proxy.example", "", "")
	body := `{"info":{"name":"Demo","version":"1.0.0"},"releases":{"1.0.0":[{"url":"https://files.pythonhosted.org/packages/demo-1.0.0.tar.gz"}],"2.0.0":[]},"urls":[{"url":"https://files.pythonhosted.org/packages/demo-1.0.0.tar.gz"}]}`
	out, _, err := h.rewriteJSONMetadata([]byte(body))
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
		{"pypi", "/packages/a/b/demo-1.0.0.tar.gz", "demo-1.0.0.tar.gz", func(p *Proxy) http.Handler {
			return NewPyPIHandlerWithUpstreams(p, "http://proxy.test", "", "").Routes()
		}},
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
	out, _, err := h.rewriteMetadataKeeping("demo", []byte(body))
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
		{"pypi-json", "/pypi/demo/json", `{"releases":`, contentTypeJSON, func(p *Proxy) http.Handler {
			return NewPyPIHandlerWithUpstreams(p, "https://proxy.test", "", "").Routes()
		}},
		{"pypi-simple", "/simple/demo/", `{"files":{}}`, pypiSimpleJSON, func(p *Proxy) http.Handler {
			return NewPyPIHandlerWithUpstreams(p, "https://proxy.test", "", "").Routes()
		}},
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

// Denylist entries must match however the ecosystem spells the name or
// version in download URLs.
func TestDenylistMatchesEcosystemSpellings(t *testing.T) {
	pypi := func(p *Proxy) http.Handler {
		return NewPyPIHandlerWithUpstreams(p, "http://proxy.test", "", "").Routes()
	}
	for _, tc := range []struct {
		name, entry, path string
		ecosystem, pkg    string
		version, filename string
		handler           func(*Proxy) http.Handler
	}{
		{"pypi wheel", "pkg:pypi/zope.interface@6.0", "/packages/a/b/zope_interface-6.0-py3-none-any.whl",
			"pypi", "zope_interface", "6.0", "zope_interface-6.0-py3-none-any.whl", pypi},
		{"pypi dotted sdist", "pkg:pypi/zope-interface@6.0", "/packages/a/b/zope.interface-6.0.tar.gz",
			"pypi", "zope.interface", "6.0", "zope.interface-6.0.tar.gz", pypi},
		{"nuget", "pkg:nuget/Newtonsoft.Json@13.0.1-Beta", "/v3-flatcontainer/newtonsoft.json/13.0.1-beta/newtonsoft.json.13.0.1-beta.nupkg",
			"nuget", "newtonsoft.json", "13.0.1-beta", "newtonsoft.json.13.0.1-beta.nupkg",
			func(p *Proxy) http.Handler { return NewNuGetHandler(p, "http://proxy.test").Routes() }},
		{"gem platform", "pkg:gem/nokogiri@1.16.0", "/gems/nokogiri-1.16.0-x86_64-linux.gem",
			"gem", "nokogiri", "1.16.0-x86_64-linux", "nokogiri-1.16.0-x86_64-linux.gem",
			func(p *Proxy) http.Handler { return NewGemHandler(p, "http://proxy.test").Routes() }},
		{"conda any channel", "pkg:conda/numpy@1.26.4", "/conda-forge/linux-64/numpy-1.26.4-py311_0.conda",
			"conda", "conda-forge/numpy", "1.26.4", "numpy-1.26.4-py311_0.conda",
			func(p *Proxy) http.Handler { return NewCondaHandler(p, "http://proxy.test").Routes() }},
		{"conda channel qualifier", "pkg:conda/numpy@1.26.4?channel=conda-forge", "/conda-forge/linux-64/numpy-1.26.4-py311_0.conda",
			"conda", "conda-forge/numpy", "1.26.4", "numpy-1.26.4-py311_0.conda",
			func(p *Proxy) http.Handler { return NewCondaHandler(p, "http://proxy.test").Routes() }},
		{"gem name with digit segment", "pkg:gem/devise-2fa@1.0.0", "/gems/devise-2fa-1.0.0.gem",
			"gem", "devise-2fa", "1.0.0", "devise-2fa-1.0.0.gem",
			func(p *Proxy) http.Handler { return NewGemHandler(p, "http://proxy.test").Routes() }},
		{"go case-escaped version", "pkg:golang/github.com/example/mod@v1.0.0-RC1", "/github.com/example/mod/@v/v1.0.0-!r!c1.zip",
			"golang", "github.com/example/mod", "v1.0.0-!r!c1", "mod@v1.0.0-!r!c1.zip",
			func(p *Proxy) http.Handler { return NewGoHandler(p, "http://proxy.test").Routes() }},
		{"go lowercase rule", "pkg:golang/github.com/example/mod@v1.0.0", "/github.com/!example/mod/@v/v1.0.0.zip",
			"golang", "github.com/Example/mod", "v1.0.0", goZipFilename("github.com/Example/mod", "v1.0.0"),
			func(p *Proxy) http.Handler { return NewGoHandler(p, "http://proxy.test").Routes() }},
		{"go mixed-case rule", "pkg:golang/github.com/Example/mod@v1.0.0", "/github.com/example/mod/@v/v1.0.0.zip",
			"golang", "github.com/example/mod", "v1.0.0", "mod@v1.0.0.zip",
			func(p *Proxy) http.Handler { return NewGoHandler(p, "http://proxy.test").Routes() }},
		{"oci repository path", "pkg:oci/library/alpine@" + ociTestDigest, "/library/alpine/blobs/" + ociTestDigest,
			"oci", "library/alpine", ociTestDigest, ociTestDigest,
			func(p *Proxy) http.Handler {
				return NewContainerHandlerWithRegistry(p, "http://proxy.test", "https://registry.invalid").Routes()
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The seeded artifact would be served if the entry didn't match.
			p, db, store, fetcher := setupTestProxy(t)
			fetcher.fetchErr = errors.New("offline")
			seedPackage(t, db, store, tc.ecosystem, tc.pkg, tc.version, tc.filename, "cached content")
			setTestDenylist(t, p, tc.entry)
			w := httptest.NewRecorder()
			tc.handler(p).ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != http.StatusForbidden {
				t.Fatalf("denied download status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

const ociTestDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Packagist keeps a tag's leading v in the version it lists, and so in the
// download URL the proxy hands out.
func TestComposerDenylistMatchesTagPrefix(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p2/symfony/console.json":
			_, _ = fmt.Fprintf(w, `{"packages":{"symfony/console":[{"version":"v6.1.7","version_normalized":"6.1.7.0","dist":{"url":%q,"type":"zip"}}]}}`,
				srv.URL+"/archives/console.zip")
		case "/archives/console.zip":
			_, _ = io.WriteString(w, "archive")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	p, _, _, _ := setupTestProxy(t)
	p.Fetcher = fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	setTestDenylist(t, p, "pkg:composer/symfony/console@6.1.7")
	h := NewComposerHandlerWithUpstreams(p, "http://proxy.test", srv.URL, srv.URL).Routes()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/symfony/console/v6.1.7/console.zip", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// Swift archives are cached under pkg:generic/swift-registry/{scope}.{name},
// which is also what streaming checks.
func TestSwiftDenylistBlocksArchive(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, entry := range []string{"pkg:swift/Apple/Example@1.2.3", "pkg:generic/swift-registry/apple.example@1.2.3"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, entry), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, `{"id":"apple.example","version":"1.2.3","resources":[{"name":"source-archive","type":"application/zip","checksum":"`+strings.Repeat("0", 64)+`"}]}`)
				}))
				t.Cleanup(upstream.Close)
				p, _, _, fetcher := setupTestProxy(t)
				fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("archive")), Size: 7}
				p.StreamArtifacts = stream
				setTestDenylist(t, p, entry)
				req := httptest.NewRequest(http.MethodGet, "/apple/example/1.2.3.zip", nil)
				req.Header.Set("Accept", swiftAcceptArchive)
				w := httptest.NewRecorder()
				NewSwiftHandler(p, "http://proxy.test", upstream.URL).Routes().ServeHTTP(w, req)
				if w.Code != http.StatusForbidden || fetcher.fetchCalled {
					t.Fatalf("status=%d fetched=%t body=%s", w.Code, fetcher.fetchCalled, w.Body.String())
				}
			})
		}
	}
}

// A denied digest is refused as a manifest as well as a blob, whether the
// client names it or a tag resolves to it.
func TestContainerDenylistBlocksManifests(t *testing.T) {
	manifest := `{"schemaVersion":2}`
	digest := sha256Digest([]byte(manifest))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerContentType, "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = io.WriteString(w, manifest)
	}))
	t.Cleanup(upstream.Close)
	for _, tc := range []struct{ entry, reference string }{
		{"pkg:oci/library/alpine@" + digest, digest},
		{"pkg:oci/library/alpine@" + digest, "3.19"},
		{"pkg:oci/library/alpine@3.19", "3.19"},
	} {
		t.Run(tc.entry+"/"+tc.reference, func(t *testing.T) {
			p := testProxy()
			p.HTTPClient = upstream.Client()
			h := NewContainerHandlerWithRegistry(p, "http://proxy.test", upstream.URL).Routes()
			// Cache the manifest first, then deny it.
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/library/alpine/manifests/"+tc.reference, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("allowed status=%d", w.Code)
			}
			setTestDenylist(t, p, tc.entry)
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				w = httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(method, "/library/alpine/manifests/"+tc.reference, nil))
				if w.Code != http.StatusForbidden {
					t.Fatalf("%s status=%d body=%s", method, w.Code, w.Body.String())
				}
			}
			w = httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/library/alpine/manifests/other", nil))
			if strings.HasPrefix(tc.entry, "pkg:oci/library/alpine@3.19") && w.Code != http.StatusOK {
				t.Fatalf("other tag status=%d", w.Code)
			}
		})
	}
}

func TestPyPIDenylistSimpleMatchesSeparators(t *testing.T) {
	body := `<html><a href="https://files.pythonhosted.org/packages/a/b/zope_interface-6.0-py3-none-any.whl">x</a><a href="https://files.pythonhosted.org/packages/a/b/zope_interface-7.0-py3-none-any.whl">y</a></html>`
	h, p := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		return pypiHTTPResponse(r, "text/html", body), nil
	})
	setTestDenylist(t, p, "pkg:pypi/zope.interface@6.0")
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/simple/zope-interface/", nil))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "6.0") || !strings.Contains(w.Body.String(), "7.0") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
