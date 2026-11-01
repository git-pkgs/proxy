package handler

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/registries/fetch"
)

const firewallReport = `{"status":403,"title":"Sonatype Firewall Report","detail":"Sonatype has identified this component as potentially malicious and blocked the download."}`

// blockedByUpstream is the error the fetch layer produces for an upstream that
// refuses an artifact with a policy denial.
func blockedByUpstream(status int, body string) error {
	return fmt.Errorf("fetching from upstream: %w", fmt.Errorf("unexpected status %d: %s", status, body))
}

func TestUpstreamStatus(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
		wantOK   bool
	}{
		{
			name:     "wrapped policy denial",
			err:      blockedByUpstream(http.StatusForbidden, firewallReport),
			wantCode: http.StatusForbidden,
			wantBody: firewallReport,
			wantOK:   true,
		},
		{
			name:     "unwrapped policy denial",
			err:      errors.New("unexpected status 409: conflict"),
			wantCode: http.StatusConflict,
			wantBody: "conflict",
			wantOK:   true,
		},
		{
			name:   "transport error carries no status",
			err:    errors.New("dial tcp 127.0.0.1:9: connect: connection refused"),
			wantOK: false,
		},
		{
			name:   "fetch layer sentinel carries no status",
			err:    fetch.ErrUpstreamDown,
			wantOK: false,
		},
		{
			name:   "no error",
			err:    nil,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body, ok := upstreamStatus(tt.err)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d", code, tt.wantCode)
			}
			if body != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func TestServeArtifactErrorForwardsPolicyBlock(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
		wantCT     string
	}{
		{
			name:       "403 with a report body is forwarded verbatim",
			err:        blockedByUpstream(http.StatusForbidden, firewallReport),
			wantStatus: http.StatusForbidden,
			wantBody:   firewallReport,
			wantCT:     contentTypeJSON,
		},
		{
			name:       "409 with a plain-text body is forwarded verbatim",
			err:        blockedByUpstream(http.StatusConflict, "blocked by policy"),
			wantStatus: http.StatusConflict,
			wantBody:   "blocked by policy",
			wantCT:     "text/plain; charset=utf-8",
		},
		{
			name:       "an upstream 5xx is still a proxy failure",
			err:        blockedByUpstream(http.StatusInternalServerError, "boom"),
			wantStatus: http.StatusBadGateway,
			wantBody:   "failed to fetch package\n",
			wantCT:     "text/plain; charset=utf-8",
		},
		{
			name:       "a transport error is still a proxy failure",
			err:        errors.New("dial tcp 127.0.0.1:9: connect: connection refused"),
			wantStatus: http.StatusBadGateway,
			wantBody:   "failed to fetch package\n",
			wantCT:     "text/plain; charset=utf-8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, _, _, _ := setupTestProxy(t)
			w := httptest.NewRecorder()
			proxy.serveArtifactError(w, tt.err, "failed to fetch package")

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if got := w.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			if got := w.Header().Get(headerContentType); got != tt.wantCT {
				t.Errorf("content-type = %q, want %q", got, tt.wantCT)
			}
		})
	}
}

// A policy denial from the upstream reaches the Maven client with the
// upstream's own status and report body, through the real fetch layer.
func TestMavenHandler_ForwardsUpstreamPolicyBlock(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentType, contentTypeJSON)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, firewallReport)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	artifactFetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()))
	t.Cleanup(func() { _ = artifactFetcher.Close() })
	proxy.Fetcher = artifactFetcher
	proxy.HTTPClient = upstream.Client()

	h := NewMavenHandler(proxy, "http://localhost", upstream.URL, upstream.URL)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/org/sonatype/maven-policy-demo/1.1.0/maven-policy-demo-1.1.0.jar", nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
	if got := w.Body.String(); got != firewallReport {
		t.Errorf("body = %q, want %q", got, firewallReport)
	}
}

// npm keeps its JSON error shape, but a policy denial is passed through as the
// upstream sent it rather than as a generic 502.
func TestNPMHandler_ForwardsUpstreamPolicyBlock(t *testing.T) {
	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = io.WriteString(w, `{"name":"policy-demo","versions":{"2.1.0":{"dist":{"tarball":"`+
			upstreamURL+`/policy-demo/-/policy-demo-2.1.0.tgz"}}}}`)
	}))
	defer upstream.Close()
	upstreamURL = upstream.URL

	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	fetcher.fetchErrByURL = map[string]error{
		upstreamURL + "/policy-demo/-/policy-demo-2.1.0.tgz": blockedByUpstream(http.StatusForbidden, firewallReport),
	}

	h := NewNPMHandler(proxy, "http://localhost", upstreamURL)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/policy-demo/-/policy-demo-2.1.0.tgz", nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
	if got := w.Body.String(); got != firewallReport {
		t.Errorf("body = %q, want %q", got, firewallReport)
	}
}

// A refused blob is reported the way an OCI registry reports a denial, not as
// a proxy failure.
func TestContainerHandler_BlobPolicyBlockIsDenied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, firewallReport)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	artifactFetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()))
	t.Cleanup(func() { _ = artifactFetcher.Close() })
	proxy.Fetcher = artifactFetcher
	proxy.HTTPClient = upstream.Client()

	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost"}
	digest := sha256Digest([]byte("blocked layer"))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/library/nginx/blobs/"+digest, nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "DENIED") {
		t.Errorf("body = %q, want an OCI DENIED error", w.Body.String())
	}
}
