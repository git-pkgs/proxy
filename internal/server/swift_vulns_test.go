package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/proxy/internal/enrichment"
	"github.com/go-chi/chi/v5"
)

func TestSwiftRegistryVulnerabilityErrors(t *testing.T) {
	h := NewAPIHandler(enrichment.New(slog.Default()), nil)
	r := chi.NewRouter()
	r.Get("/api/vulns/{ecosystem}/*", h.HandleVulnsPath)
	r.Get("/api/package/{ecosystem}/*", h.HandlePackagePath)
	transport := &swiftVulnsTransport{}
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original })

	for _, path := range []string{
		"/api/vulns/swift/apple.swift-nio",
		"/api/vulns/swift/apple.swift-nio/2.0.0",
		"/api/vulns/Swift/Apple.Swift-NIO/2.0.0",
		"/api/package/swift/apple.swift-nio/2.0.0",
		"/api/package/Swift/Apple.Swift-NIO/2.0.0",
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body)
			}
			var response ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Code != ErrCodeBadRequest || response.Message != "package identity is not supported for vulnerability lookups" {
				t.Fatalf("unexpected response: %+v", response)
			}
		})
	}
	if transport.requests != 0 {
		t.Errorf("made %d upstream requests for unsupported identities", transport.requests)
	}
}

type swiftVulnsTransport struct{ requests int }

func (s *swiftVulnsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.requests++
	return nil, fmt.Errorf("unexpected upstream request: %s", r.URL)
}
