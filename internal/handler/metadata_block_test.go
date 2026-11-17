package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// A firewall that refuses a metadata request reaches the client with its own
// status and report when no cached copy can stand in, instead of a 502.
func TestMetadataPolicyBlockIsForwarded(t *testing.T) {
	tests := []struct {
		name string
		path string
		// cooldown turns on a cooldown, so the download is checked
		// against the metadata first.
		cooldown bool
		// setup, when set, prepares the proxy, such as by pushing a hosted
		// package.
		setup   func(t *testing.T, proxy *Proxy)
		handler func(proxy *Proxy, upstreamURL string) http.Handler
	}{
		{name: "npm packument", path: "/policy-demo", handler: func(p *Proxy, u string) http.Handler {
			return NewNPMHandler(p, "http://localhost", u).Routes()
		}},
		{name: "npm tarball with a cold cache", path: "/policy-demo/-/policy-demo-2.1.0.tgz", handler: func(p *Proxy, u string) http.Handler {
			return NewNPMHandler(p, "http://localhost", u).Routes()
		}},
		{name: "npm tarball under cooldown", path: "/policy-demo/-/policy-demo-2.1.0.tgz", cooldown: true, handler: func(p *Proxy, u string) http.Handler {
			return NewNPMHandler(p, "http://localhost", u).Routes()
		}},
		{name: "pypi simple", path: "/simple/policy-demo/", handler: func(p *Proxy, u string) http.Handler {
			return NewPyPIHandlerWithUpstreams(p, "http://localhost", u, u).Routes()
		}},
		{name: "pypi json", path: "/pypi/policy-demo/json", handler: func(p *Proxy, u string) http.Handler {
			return NewPyPIHandlerWithUpstreams(p, "http://localhost", u, u).Routes()
		}},
		{name: "composer", path: "/p2/vendor/policy-demo.json", handler: func(p *Proxy, u string) http.Handler {
			h := NewComposerHandler(p, "http://localhost")
			h.upstreamURL, h.repoURL = u, u
			return h.Routes()
		}},
		{name: "gem compact index", path: "/info/policy-demo", handler: func(p *Proxy, u string) http.Handler {
			h := NewGemHandler(p, "http://localhost")
			h.upstreamURL = u
			return h.Routes()
		}},
		{name: "gem specs", path: "/specs.4.8.gz", handler: func(p *Proxy, u string) http.Handler {
			h := NewGemHandler(p, "http://localhost")
			h.upstreamURL = u
			return h.Routes()
		}},
		{name: "hex", path: "/packages/policy_demo", handler: func(p *Proxy, u string) http.Handler {
			h := NewHexHandler(p, "http://localhost")
			h.upstreamURL, h.apiURL = u, u
			return h.Routes()
		}},
		{name: "conda", path: "/conda-forge/noarch/repodata.json", handler: func(p *Proxy, u string) http.Handler {
			h := NewCondaHandler(p, "http://localhost")
			h.upstreamURL = u
			return h.Routes()
		}},
		{name: "helm", path: "/demo/index.yaml", handler: func(p *Proxy, u string) http.Handler {
			return NewHelmHandler(p, "http://localhost", map[string]string{"demo": u}).Routes()
		}},
		{name: "chef", path: chefAPIPath + "/cookbooks/policy-demo", handler: func(p *Proxy, u string) http.Handler {
			return NewChefHandler(p, "http://localhost", u).Routes()
		}},
		{name: "cargo", path: "/po/li/policy-demo", handler: func(p *Proxy, u string) http.Handler {
			return NewCargoHandler(p, "http://localhost", u, u).Routes()
		}},
		{name: "pub", path: "/api/packages/policy_demo", handler: func(p *Proxy, u string) http.Handler {
			h := NewPubHandler(p, "http://localhost")
			h.upstreamURL = u
			return h.Routes()
		}},
		{name: "nuget versions", path: "/v3-flatcontainer/policy-demo/index.json", handler: func(p *Proxy, u string) http.Handler {
			h := NewNuGetHandler(p, "http://localhost")
			h.upstreamURL = u
			return h.Routes()
		}},
		{name: "maven metadata", path: "/org/example/policy-demo/maven-metadata.xml", handler: func(p *Proxy, u string) http.Handler {
			return NewMavenHandler(p, "http://localhost", u, u).Routes()
		}},
		{name: "terraform", path: "/providers/registry.terraform.io/hashicorp/random/index.json", handler: func(p *Proxy, u string) http.Handler {
			return NewTerraformHandler(p, map[string]string{"registry.terraform.io": u}).Routes()
		}},
		{name: "npm version", path: "/policy-demo/2.1.0", handler: func(p *Proxy, u string) http.Handler {
			return NewNPMHandler(p, "http://localhost", u).Routes()
		}},
		{name: "npm version under cooldown", path: "/policy-demo/2.1.0", cooldown: true, handler: func(p *Proxy, u string) http.Handler {
			return NewNPMHandler(p, "http://localhost", u).Routes()
		}},
		// Rewrite caching streams the packument through storage.
		{name: "npm packument streamed", path: "/policy-demo", setup: cacheRewrites, handler: func(p *Proxy, u string) http.Handler {
			return NewNPMHandler(p, "http://localhost", u).Routes()
		}},
		{name: "npm version under cooldown streamed", path: "/policy-demo/2.1.0", cooldown: true, setup: cacheRewrites, handler: func(p *Proxy, u string) http.Handler {
			return NewNPMHandler(p, "http://localhost", u).Routes()
		}},
		{name: "npm packument with a hosted version", path: "/policy-demo", setup: pushHosted("npm", "policy-demo", npmTarball(`{"name":"policy-demo","version":"1.0.0"}`)),
			handler: func(p *Proxy, u string) http.Handler {
				return NewNPMHandler(p, "http://localhost", u).Routes()
			}},
		{name: "composer with a hosted version", path: "/p2/vendor/policy-demo.json", setup: pushHosted("composer", "vendor/policy-demo", composerZip("", `{"name":"vendor/policy-demo","version":"1.0.0"}`)),
			handler: func(p *Proxy, u string) http.Handler {
				return NewComposerHandlerWithUpstreams(p, "http://localhost", u, u).Routes()
			}},
		{name: "go version list under cooldown", path: "/example.com/policy-demo/@v/list", cooldown: true, handler: func(p *Proxy, u string) http.Handler {
			return NewGoHandlerWithUpstream(p, "http://localhost", u).Routes()
		}},
		{name: "npm dist-tags", path: "/-/package/policy-demo/dist-tags", handler: func(p *Proxy, u string) http.Handler {
			return NewNPMHandler(p, "http://localhost", u).Routes()
		}},
		{name: "npm attestations", path: "/-/npm/v1/attestations/policy-demo@2.1.0", handler: func(p *Proxy, u string) http.Handler {
			return NewNPMHandler(p, "http://localhost", u).Routes()
		}},
		{name: "npm attestations under cooldown", path: "/-/npm/v1/attestations/policy-demo@2.1.0", cooldown: true, handler: func(p *Proxy, u string) http.Handler {
			return NewNPMHandler(p, "http://localhost", u).Routes()
		}},
		{name: "composer download with a cold cache", path: "/files/vendor/policy-demo/1.0.0/policy-demo-1.0.0.zip", handler: func(p *Proxy, u string) http.Handler {
			h := NewComposerHandler(p, "http://localhost")
			h.upstreamURL, h.repoURL = u, u
			return h.Routes()
		}},
		{name: "pypi download under cooldown", path: "/packages/source/p/policy-demo/policy_demo-1.0.tar.gz", cooldown: true, handler: func(p *Proxy, u string) http.Handler {
			return NewPyPIHandlerWithUpstreams(p, "http://localhost", u, u).Routes()
		}},
		{name: "nuget service index", path: "/v3/index.json", handler: func(p *Proxy, u string) http.Handler {
			h := NewNuGetHandler(p, "http://localhost")
			h.upstreamURL = u
			return h.Routes()
		}},
		{name: "nuget download under cooldown", path: "/v3-flatcontainer/policy-demo/1.0.0/policy-demo.1.0.0.nupkg", cooldown: true, handler: func(p *Proxy, u string) http.Handler {
			h := NewNuGetHandler(p, "http://localhost")
			h.upstreamURL = u
			return h.Routes()
		}},
		{name: "maven metadata under cooldown", path: "/org/example/policy-demo/maven-metadata.xml", cooldown: true, handler: func(p *Proxy, u string) http.Handler {
			return NewMavenHandler(p, "http://localhost", u, u).Routes()
		}},
		{name: "swift releases", path: "/example/policy-demo", handler: func(p *Proxy, u string) http.Handler {
			return NewSwiftHandler(p, "http://localhost", u).Routes()
		}},
		{name: "go version list", path: "/example.com/policy-demo/@v/list", handler: func(p *Proxy, u string) http.Handler {
			return NewGoHandlerWithUpstream(p, "http://localhost", u).Routes()
		}},
		{name: "go sumdb", path: "/sumdb/sum.example/lookup/example.com/policy-demo@v1.0.0", handler: func(p *Proxy, u string) http.Handler {
			h := NewGoHandlerWithUpstream(p, "http://localhost", u)
			h.SetSumDBs(map[string]string{"sum.example": u}, false)
			return h.Routes()
		}},
		{name: "arch database", path: "/core/os/x86_64/core.db", handler: func(p *Proxy, u string) http.Handler {
			return NewArchHandler(p, "http://localhost", map[string]string{"core": u}).Routes()
		}},
	}
	const reportType = "application/problem+json"
	for _, tt := range tests {
		// With the metadata cache off, documents are relayed; with it on and
		// empty, there is no cached copy to fall back to.
		for _, cacheMetadata := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cache_metadata=%t", tt.name, cacheMetadata), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set(headerContentType, reportType)
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, firewallReport)
				}))
				t.Cleanup(upstream.Close)
				proxy, _, _, _ := setupTestProxy(t)
				proxy.HTTPClient = upstream.Client()
				proxy.CacheMetadata = cacheMetadata
				if tt.cooldown {
					proxy.Cooldown = &cooldown.Config{Default: "7d"}
				}
				if tt.setup != nil {
					tt.setup(t, proxy)
				}

				w := httptest.NewRecorder()
				tt.handler(proxy, upstream.URL).ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.path, nil))

				if w.Code != http.StatusForbidden || w.Body.String() != firewallReport {
					t.Errorf("status = %d, body = %q; want 403 and the upstream report", w.Code, w.Body.String())
				}
				if got := w.Header().Get(headerContentType); got != reportType {
					t.Errorf("content-type = %q, want %q", got, reportType)
				}
			})
		}
	}
}

// cacheRewrites turns on rewrite caching and a metadata TTL, under which
// npm streams packuments through storage.
func cacheRewrites(_ *testing.T, p *Proxy) {
	p.SetMetadataRewriteCacheSize(1 << 20)
	p.MetadataTTL = time.Hour
}

// pushHosted returns a setup that pushes archive as version 1.0.0 of name.
func pushHosted(ecosystem, name string, archive []byte) func(*testing.T, *Proxy) {
	return func(t *testing.T, p *Proxy) {
		t.Helper()
		p.HostedEnabled = true
		target, err := HostedTargetFor(ecosystem, name, "1.0.0", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.PushLocalArtifact(context.Background(), ecosystem, target, bytes.NewReader(archive), "", ""); err != nil {
			t.Fatal(err)
		}
	}
}

// Only a policy denial is forwarded: another refusal of a metadata request
// is still a 502.
func TestMetadataServerErrorIsStillBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentType, contentTypeJSON)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, firewallReport)
	}))
	t.Cleanup(upstream.Close)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()

	w := httptest.NewRecorder()
	NewNPMHandler(proxy, "http://localhost", upstream.URL).Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/policy-demo", nil))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

// A cached copy still stands in for a document upstream now refuses.
func TestMetadataPolicyBlockServesTheCachedCopy(t *testing.T) {
	const packument = `{"name":"policy-demo","versions":{}}`
	var refuse bool
	var refused int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentType, contentTypeJSON)
		if refuse {
			refused++
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, firewallReport)
			return
		}
		_, _ = io.WriteString(w, packument)
	}))
	t.Cleanup(upstream.Close)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.CacheMetadata = true
	h := NewNPMHandler(proxy, "http://localhost", upstream.URL).Routes()

	for i, want := range []bool{false, true} {
		refuse = want
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/policy-demo", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %q; want 200", i+1, w.Code, w.Body.String())
		}
	}
	if refused == 0 {
		t.Error("the second request wasn't sent upstream")
	}
}
