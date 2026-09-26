package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/go-chi/chi/v5/middleware"
)

// Small bodies normally acquire Content-Length when the handler returns. Late,
// undeclared trailers need a flush through the production responseWriter first.
func TestRelayLateTrailersThroughMiddleware(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "small body")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		w.Header().Set(http.TrailerPrefix+"X-Late", "verified")
	}))
	defer upstream.Close()
	for _, protocol := range []string{"http1", "http2"} {
		t.Run(protocol, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			s := &Server{logger: logger}
			proxy := &handler.Proxy{Logger: logger, HTTPClient: upstream.Client()}
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxy.ProxyUpstream(w, r, upstream.URL, nil)
			})
			downstream := httptest.NewUnstartedServer(s.LoggerMiddleware(middleware.Recoverer(h)))
			wantProto := 1
			if protocol == "http2" {
				downstream.EnableHTTP2 = true
				downstream.StartTLS()
				wantProto = 2
			} else {
				downstream.Start()
			}
			defer downstream.Close()
			resp, err := downstream.Client().Get(downstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != "small body" || resp.ProtoMajor != wantProto {
				t.Fatalf("unexpected response: protocol=%s body=%q error=%v", resp.Proto, body, err)
			}
			if resp.Trailer.Get("X-Late") != "verified" || resp.Header.Get("X-Late") != "" {
				t.Fatalf("late trailer lost or sent as a header: headers=%v trailers=%v", resp.Header, resp.Trailer)
			}
		})
	}
}

func TestRelayAbortThroughMiddleware(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 64*1024))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		// End the upstream chunked body without its terminating chunk.
		panic(http.ErrAbortHandler)
	}))
	defer upstream.Close()
	for _, protocol := range []string{"http1", "http2"} {
		t.Run(protocol, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			s := &Server{logger: logger}
			proxy := &handler.Proxy{Logger: logger, HTTPClient: upstream.Client()}
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxy.ProxyUpstream(w, r, upstream.URL, nil)
			})
			downstream := httptest.NewUnstartedServer(s.LoggerMiddleware(middleware.Recoverer(h)))
			wantProto := 1
			if protocol == "http2" {
				downstream.EnableHTTP2 = true
				downstream.StartTLS()
				wantProto = 2
			} else {
				downstream.Start()
			}
			defer downstream.Close()
			resp, err := downstream.Client().Get(downstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, err = io.ReadAll(resp.Body)
			if err == nil {
				t.Error("truncated upstream was delivered as a complete response")
			}
			if resp.StatusCode != http.StatusOK || resp.ProtoMajor != wantProto {
				t.Fatalf("unexpected response: status=%d protocol=%s", resp.StatusCode, resp.Proto)
			}
		})
	}
}
