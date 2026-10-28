package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/git-pkgs/proxy/internal/accesslog"
	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/go-chi/chi/v5/middleware"
)

var requestCounter atomic.Uint64

// RequestIDMiddleware adds a sequential request ID to the context and response headers.
// IDs are formatted as [001], [002], etc. for easy log correlation.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = requestCounter.Add(1)
		requestID := middleware.GetReqID(r.Context())

		// Store formatted ID in context
		ctx := accesslog.WithRequestID(r.Context(), requestID)

		// Add to response header for client tracking
		w.Header().Set("X-Request-ID", requestID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetRequestID retrieves the request ID from context.
func GetRequestID(ctx context.Context) string {
	return accesslog.RequestID(ctx)
}

// markRequestContext marks each client request so upstream fetches it starts
// outlive a client that hangs up, and so a HEAD answered from the cache does
// not open the stored object.
func markRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := handler.DetachUpstreamFetch(r.Context())
		switch {
		case r.Method == http.MethodHead:
			ctx = handler.ServingHead(ctx)
		case r.Method == http.MethodGet && r.Header.Get("Range") == "":
			ctx = handler.StreamCacheMiss(ctx)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// LoggerMiddleware logs HTTP requests with request ID correlation.
func (s *Server) LoggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := GetRequestID(r.Context())

		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		// Deferred because a truncated upstream relay aborts the handler with
		// http.ErrAbortHandler, which would otherwise leave the request out of
		// the log, the metrics and the access log entirely.
		defer func() {
			duration := time.Since(start)
			userAgent := r.UserAgent()
			client := clientName(userAgent)
			addr := clientAddr(r, s.trustsForwardedFor())
			ecosystem := requestEcosystem(r.URL.Path)

			s.logger.Info("request",
				"request_id", requestID,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.status,
				"duration", duration,
				"bytes", rw.bytes,
				"client", client,
				"remote", r.RemoteAddr,
				"remote_ip", addr)

			// Scrapes of /metrics would otherwise attribute themselves,
			// burying real callers under whatever polls the proxy most often.
			if r.URL.Path != "/metrics" {
				metrics.RecordRequest(ecosystem, rw.status, duration)
				metrics.RecordResponse(ecosystem, client, rw.bytes)
				s.sources.Record(addr, client, rw.bytes)
			}

			if s.accessLog != nil {
				if err := s.accessLog.Write(accesslog.Entry{
					Event:      accesslog.EventRequest,
					RequestID:  requestID,
					Method:     r.Method,
					Path:       r.URL.EscapedPath(),
					StatusCode: rw.status,
					DurationMS: duration.Milliseconds(),
					RemoteAddr: r.RemoteAddr,
					RemoteIP:   addr,
					UserAgent:  userAgent,
					Client:     client,
					Ecosystem:  ecosystem,
					Bytes:      rw.bytes,
				}); err != nil {
					s.logger.Error("failed to write access log", "error", err)
				}
			}
		}()

		next.ServeHTTP(rw, r)
	})
}

// requestEcosystem names the ecosystem a request path belongs to.
//
// Every mounted package route must appear here. An unlisted one falls to
// "other" along with the UI, health and metrics paths, pooling a real
// ecosystem's traffic with traffic that belongs to no ecosystem at all.
func requestEcosystem(path string) string {
	segment, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	switch segment {
	case "npm", "cargo", "hex", "pub", "pypi", "maven", "gradle", "nuget",
		"conan", "chef", "conda", "cran", "julia", "debian", "rpm", "arch",
		"freebsd", "helm", "homebrew", "generic", "swift", "git":
		return segment
	case "apk":
		return "alpine"
	case "gem":
		return "rubygems"
	case "go":
		return "golang"
	case "composer":
		return "packagist"
	case "v2":
		return "oci"
	default:
		return "other"
	}
}

// rejectQueryInPath refuses requests whose decoded path contains '?', '#'
// or ';'. Handlers build upstream URLs from the decoded path, where '?' or
// '#' would end the path: busybox.apk%3F/foo-9.9-r0.apk fetches busybox but
// is parsed and cached as foo, under the wrong PURL and past the denylist.
// Tomcat and Jetty drop ";params" from a path segment, so 1.0;x/evil-1.0.jar
// would be served as version 1.0 while the proxy checks 1.0;x. Composer
// archives are exempt: their upstream URL comes from the package metadata,
// and git branch versions such as dev-fix/#42 may contain '#' or ';'.
func rejectQueryInPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.ContainsAny(r.URL.Path, "?#;") && !strings.HasPrefix(r.URL.Path, "/composer/files/") {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rejectDotSegments refuses requests whose decoded path has a '.' or '..'
// segment. Routers clean literal dot segments, but encoded ones such as
// %2e%2e reach the handlers, and several of them append the path to the
// upstream URL, where the upstream resolves it outside the configured base
// path. Backslashes count as separators, and the path is decoded once more
// so double-encoded segments are refused too.
func rejectDotSegments(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if config.HasDotSegment(r.URL.Path) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sandboxUpstreamContent keeps package-route responses from running as pages
// on the proxy's origin, which also serves /ui. Their bodies, and usually
// their Content-Type, come from upstreams or from whoever pushed a hosted
// package, so an artifact served as text/html or image/svg+xml could otherwise
// run scripts with access to the UI. nosniff stops browsers from guessing a
// renderable type, and a sandbox policy runs anything that does render with
// scripts disabled and an opaque origin. Package managers ignore both
// headers. Archive files browsed from /ui get the same headers in browse.go.
func sandboxUpstreamContent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox")
		next.ServeHTTP(w, r)
	})
}

// frameAncestorsNone stops other sites from framing the web UI, where a
// framed page could trick a signed-in user into clicking Delete.
const frameAncestorsNone = "frame-ancestors 'none'"

func denyFraming(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", frameAncestorsNone)
		next.ServeHTTP(w, r)
	})
}
