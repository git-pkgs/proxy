package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/enrichment"
)

// lookupRequest is a POST to a lookup endpoint whose context is already
// done, so a request that gets as far as its lookups reaches no registry.
func lookupRequest(path, contentType, body string) *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func outdatedBody(n int) string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"ecosystem":"npm","name":"p%d","version":"1.0.0"}`, i)
	}
	return `{"packages":[` + strings.Join(entries, ",") + `]}`
}

func bulkBody(n int) string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = fmt.Sprintf(`"pkg:npm/p%d@1.0.0"`, i)
	}
	return `{"purls":[` + strings.Join(entries, ",") + `]}`
}

// One request to a lookup endpoint asks the registries about each entry, so
// the number of entries is capped, and other sites can't send one through a
// visitor's browser.
func TestLookupEndpointsLimitTheirRequests(t *testing.T) {
	h := NewAPIHandler(enrichment.New(slog.New(slog.NewTextHandler(os.Stdout, nil))), nil)
	endpoints := []struct {
		path   string
		handle http.HandlerFunc
		body   func(n int) string
		limit  int
	}{
		{"/api/outdated", h.HandleOutdated, outdatedBody, 100},
		{"/api/bulk", h.HandleBulkLookup, bulkBody, 1000},
	}
	for _, e := range endpoints {
		for _, tt := range []struct {
			name, contentType string
			entries, want     int
		}{
			{"at the limit", "application/json", e.limit, http.StatusOK},
			{"over the limit", "application/json", e.limit + 1, http.StatusBadRequest},
			{"with a charset", "application/json; charset=utf-8", 1, http.StatusOK},
			{"no content type", "", 1, http.StatusUnsupportedMediaType},
			{"a form's content type", "text/plain", 1, http.StatusUnsupportedMediaType},
		} {
			t.Run(e.path+" "+tt.name, func(t *testing.T) {
				w := httptest.NewRecorder()
				e.handle(w, lookupRequest(e.path, tt.contentType, e.body(tt.entries)))
				if w.Code != tt.want {
					t.Fatalf("status = %d, want %d: %s", w.Code, tt.want, w.Body)
				}
				if tt.want == http.StatusBadRequest && !strings.Contains(w.Body.String(), fmt.Sprint(e.limit)) {
					t.Errorf("body = %s, want it to name the limit of %d", w.Body, e.limit)
				}
			})
		}
	}
}

// A body over the size limit is refused even when it is valid JSON naming
// few entries.
func TestLookupEndpointsRefuseALargeBody(t *testing.T) {
	h := NewAPIHandler(enrichment.New(slog.New(slog.NewTextHandler(os.Stdout, nil))), nil)
	long := strings.Repeat("x", maxBodySize)
	for path, e := range map[string]struct {
		handle http.HandlerFunc
		body   string
	}{
		"/api/outdated": {h.HandleOutdated, `{"packages":[{"ecosystem":"npm","name":"p","version":"1.0.0"}],"padding":"` + long + `"}`},
		"/api/bulk":     {h.HandleBulkLookup, `{"purls":["pkg:npm/p@1.0.0"],"padding":"` + long + `"}`},
	} {
		w := httptest.NewRecorder()
		e.handle(w, lookupRequest(path, "application/json", e.body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want %d", path, w.Code, http.StatusBadRequest)
		}
	}
}
