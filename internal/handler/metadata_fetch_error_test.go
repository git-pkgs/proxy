package handler

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteMetadataFetchErrorLogLevel(t *testing.T) {
	tests := []struct {
		upstream  int
		wantLevel string
	}{
		{http.StatusForbidden, "level=WARN"},
		{http.StatusUnauthorized, "level=WARN"},
		{http.StatusGone, "level=WARN"},
		{http.StatusRequestTimeout, "level=ERROR"},
		{http.StatusTooManyRequests, "level=ERROR"},
		{http.StatusInternalServerError, "level=ERROR"},
		{http.StatusBadGateway, "level=ERROR"},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.upstream), func(t *testing.T) {
			var logs bytes.Buffer
			proxy := testProxy()
			proxy.Logger = slog.New(slog.NewTextHandler(&logs, nil))

			err := upstreamMetadataStatus(&http.Response{StatusCode: tt.upstream, Body: http.NoBody})
			// Wrapped the way staleMetadata returns it when nothing is cached.
			err = fmt.Errorf("upstream failed and no cached metadata: %w", err)
			w := httptest.NewRecorder()
			proxy.writeMetadataFetchError(w, err)

			if w.Code != http.StatusBadGateway {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
			}
			if !strings.Contains(logs.String(), tt.wantLevel) {
				t.Errorf("log = %q, want %s", logs.String(), tt.wantLevel)
			}
			if want := fmt.Sprintf("upstream returned %d", tt.upstream); !strings.Contains(logs.String(), want) {
				t.Errorf("log = %q, want it to name %q", logs.String(), want)
			}
		})
	}
}
