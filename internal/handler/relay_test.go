package handler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/git-pkgs/cooldown"
	"github.com/go-chi/chi/v5/middleware"
)

// Exercise the actual routes as well as the two shared entry points. Error
// responses also reach relay branches in handlers that normally parse metadata.
func relayTestRoutes(proxy *Proxy, upstream string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/upstream", func(w http.ResponseWriter, r *http.Request) {
		proxy.ProxyUpstream(w, r, upstream, nil)
	})
	mux.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) {
		proxy.ProxyFile(w, r, upstream)
	})
	mux.HandleFunc("/metadata", func(w http.ResponseWriter, r *http.Request) {
		proxy.ProxyCached(w, r, upstream, "test", "index")
	})
	const proxyURL = "http://proxy.local"
	mount := func(prefix string, h http.Handler) {
		mux.Handle(prefix+"/", http.StripPrefix(prefix, h))
	}
	mount("/nuget", NewNuGetHandlerWithUpstreams(proxy, proxyURL, upstream, upstream).Routes())
	mount("/conan", NewConanHandlerWithUpstream(proxy, proxyURL, upstream).Routes())
	mount("/composer", NewComposerHandlerWithUpstreams(proxy, proxyURL, upstream, upstream).Routes())
	mount("/pypi", NewPyPIHandlerWithUpstreams(proxy, proxyURL, upstream, upstream).Routes())
	mount("/gem", NewGemHandlerWithUpstream(proxy, proxyURL, upstream).Routes())
	mount("/conda", NewCondaHandlerWithUpstream(proxy, proxyURL, upstream).Routes())
	mount("/hex", NewHexHandlerWithUpstreams(proxy, proxyURL, upstream, upstream).Routes())
	mount("/swift", NewSwiftHandler(proxy, proxyURL, upstream).Routes())
	mount("/v2", NewContainerHandlerWithRegistry(proxy, proxyURL, upstream).Routes())
	// Match the production recovery middleware: it must not swallow the abort.
	return middleware.Recoverer(mux)
}

func TestRelayRoutes(t *testing.T) {
	for _, route := range []string{
		"/upstream", "/file", "/metadata", "/nuget/query",
		"/conan/v2/files/demo/1.0/user/stable/rev/recipe/other.txt",
		"/composer/search.json", "/pypi/simple/", "/gem/api/v1/dependencies",
		"/gem/info/demo", "/conda/conda-forge/noarch/repodata.json",
		"/hex/packages/demo", "/swift/scope/demo/1.0.0/Package.swift",
		"/v2/library/demo/manifests/latest", "/v2/library/demo/tags/list",
	} {
		t.Run(route, func(t *testing.T) {
			for _, truncated := range []bool{false, true} {
				t.Run(fmt.Sprintf("truncated=%v", truncated), func(t *testing.T) {
					testRelayRoute(t, route, truncated)
				})
			}
		})
	}
}

func testRelayRoute(t *testing.T, route string, truncated bool) {
	t.Helper()
	// Large enough to commit downstream headers before a body-copy failure.
	body := strings.Repeat("x", 64*1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nConnection: X-Private\r\nX-Private: secret\r\nTransfer-Encoding: chunked\r\nTrailer: X-Checksum, X-Private\r\n\r\n%x\r\n%s\r\n", len(body), body)
		if !truncated {
			_, _ = fmt.Fprint(rw, "0\r\nX-Checksum: verified\r\nX-Late: discovered-at-eof\r\nX-Private: still-secret\r\n\r\n")
		}
		if err := rw.Flush(); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	proxy := testProxy()
	proxy.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy.HTTPClient = upstream.Client()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	downstream := httptest.NewServer(relayTestRoutes(proxy, upstream.URL))
	defer downstream.Close()

	resp, err := downstream.Client().Get(downstream.URL + route)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if resp.Header.Get("Connection") != "" || resp.Header.Get("X-Private") != "" || resp.Trailer.Get("X-Private") != "" {
		t.Fatalf("connection-scoped fields leaked: headers=%v trailers=%v", resp.Header, resp.Trailer)
	}
	if truncated {
		if readErr == nil {
			t.Fatal("truncated upstream was delivered as a complete response")
		}
		return
	}
	if readErr != nil || string(got) != body {
		t.Fatalf("body length = %d, want %d; error = %v", len(got), len(body), readErr)
	}
	if resp.Trailer.Get("X-Checksum") != "verified" || resp.Trailer.Get("X-Late") != "discovered-at-eof" {
		t.Fatalf("trailers not relayed: %v", resp.Trailer)
	}
	if route == "/pypi/simple/" && !strings.Contains(resp.Header.Get("Vary"), "Accept") {
		t.Error("PyPI lost Vary: Accept")
	}
}

func TestRelayResponseHeaders(t *testing.T) {
	headers := http.Header{
		"Connection": {"keep-alive, x-private", " X-Second, Content-Length "},
		"X-Private":  {"secret"}, "X-Second": {"secret"},
		"Keep-Alive": {"timeout=5"}, "Proxy-Connection": {"keep-alive"},
		"Proxy-Authenticate": {"challenge"}, "Proxy-Authorization": {"credentials"},
		"Te": {"trailers"}, "Trailer": {"X-Untrusted"},
		"Transfer-Encoding": {"chunked"}, "Upgrade": {"websocket"},
		"Content-Length": {"999"}, "Content-Type": {"application/octet-stream"},
		"Content-Encoding": {"gzip"}, "Etag": {`"v1"`},
		"Set-Cookie": {"a=1", "b=2"}, "Www-Authenticate": {"Bearer realm=test"},
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader("body"))}
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "local")
	testProxy().relayResponse(w, httptest.NewRequest(http.MethodGet, "/", nil), resp, nil)
	for _, name := range []string{
		"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
		"X-Private", "X-Second", "Content-Length",
	} {
		if got := w.Header().Get(name); got != "" {
			t.Errorf("hop-by-hop header %s survived: %q", name, got)
		}
	}
	for _, name := range []string{"Content-Type", "Content-Encoding", "Etag", "Set-Cookie", "Www-Authenticate"} {
		if got := strings.Join(w.Header().Values(name), ","); got != strings.Join(headers.Values(name), ",") {
			t.Errorf("end-to-end header %s changed: %q", name, got)
		}
	}
	if w.Header().Get("X-Request-ID") != "local" || w.Body.String() != "body" {
		t.Fatal("local header or response body changed")
	}
	if headers.Get("X-Private") != "secret" {
		t.Fatal("relay mutated the upstream response headers")
	}
}

func TestRelayResponseBodiless(t *testing.T) {
	for _, tt := range []struct {
		method string
		status int
		length string
	}{
		{http.MethodHead, http.StatusOK, "99"},
		{http.MethodGet, http.StatusNoContent, ""},
		{http.MethodGet, http.StatusResetContent, "0"},
		{http.MethodGet, http.StatusNotModified, "99"},
		{http.MethodGet, http.StatusEarlyHints, ""},
	} {
		t.Run(fmt.Sprintf("%s/%d", tt.method, tt.status), func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tt.status, Header: http.Header{"Content-Length": {"99"}},
				Body:    io.NopCloser(iotest.ErrReader(errors.New("body must not be read"))),
				Trailer: http.Header{"X-Checksum": {"ignored"}},
			}
			w := httptest.NewRecorder()
			testProxy().relayResponse(w, httptest.NewRequest(tt.method, "/", nil), resp, nil)
			if w.Code != tt.status || w.Body.Len() != 0 || w.Header().Get("Trailer") != "" {
				t.Fatalf("unexpected bodiless response: %d %v %q", w.Code, w.Header(), w.Body.String())
			}
			if got := w.Header().Get("Content-Length"); got != tt.length {
				t.Errorf("Content-Length = %q, want %q", got, tt.length)
			}
		})
	}
}

type relayFailWriter struct{ http.ResponseWriter }

func (w relayFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("downstream disconnected")
}

func TestRelayResponseCopyFailure(t *testing.T) {
	for _, downstreamError := range []bool{false, true} {
		t.Run(fmt.Sprintf("downstreamError=%v", downstreamError), func(t *testing.T) {
			var logs bytes.Buffer
			proxy := testProxy()
			proxy.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			resp := &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header),
				Request: httptest.NewRequest(http.MethodGet, "http://upstream.test/file", nil),
				Body:    io.NopCloser(io.MultiReader(strings.NewReader("part"), iotest.ErrReader(io.ErrUnexpectedEOF))),
			}
			var w http.ResponseWriter = httptest.NewRecorder()
			wantBytes := "bytes=4"
			if downstreamError {
				w = relayFailWriter{httptest.NewRecorder()}
				wantBytes = "bytes=0"
			}
			defer func() {
				if got := recover(); got != http.ErrAbortHandler {
					t.Errorf("panic = %v, want http.ErrAbortHandler", got)
				}
				for _, field := range []string{"url=http://upstream.test/file", "status=200", wantBytes, "error="} {
					if !strings.Contains(logs.String(), field) {
						t.Errorf("log missing %q: %s", field, logs.String())
					}
				}
			}()
			proxy.relayResponse(w, httptest.NewRequest(http.MethodGet, "/", nil), resp, nil)
		})
	}
}

func TestRelayResponseTrailerValidation(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Connection": {"X-Private"}, "Content-Length": {"4"}},
		Body:       io.NopCloser(strings.NewReader("body")),
		Trailer: http.Header{
			"X-Checksum": {"verified"}, "X-Private": {"secret"},
			"Content-Length": {"999"}, "Content-Type": {"bad/type"},
			"Authorization": {"secret"}, "Transfer-Encoding": {"chunked"},
			"If-Match": {"secret"}, "Invalid Name": {"invalid"},
		},
	}
	w := httptest.NewRecorder()
	testProxy().relayResponse(w, httptest.NewRequest(http.MethodGet, "/", nil), resp, nil)
	result := w.Result()
	defer func() { _ = result.Body.Close() }()
	if result.Header.Get("Content-Length") != "" {
		t.Error("declared trailers must remove Content-Length")
	}
	if len(result.Trailer) != 1 || result.Trailer.Get("X-Checksum") != "verified" {
		t.Fatalf("invalid trailers leaked: %v", result.Trailer)
	}
}

func TestRelayClosesBodyOnAbort(t *testing.T) {
	for _, mode := range []string{"upstream", "file", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			body := &closeTrackingReader{Reader: iotest.ErrReader(io.ErrUnexpectedEOF)}
			proxy := testProxy()
			proxy.HTTPClient = &http.Client{Transport: pypiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: r}, nil
			})}
			defer func() {
				if got := recover(); got != http.ErrAbortHandler {
					t.Errorf("panic = %v, want http.ErrAbortHandler", got)
				}
				if !body.closed {
					t.Error("upstream body was not closed on abort")
				}
			}()
			relayTestRoutes(proxy, "http://upstream.test").ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/"+mode, nil))
		})
	}
}
