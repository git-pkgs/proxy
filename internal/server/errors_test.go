package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteError(t *testing.T) {
	tests := []struct {
		name    string
		fn      func(w http.ResponseWriter)
		status  int
		code    string
		message string
	}{
		{
			name:    "badRequest",
			fn:      func(w http.ResponseWriter) { badRequest(w, "missing field") },
			status:  http.StatusBadRequest,
			code:    ErrCodeBadRequest,
			message: "missing field",
		},
		{
			name:    "notFound",
			fn:      func(w http.ResponseWriter) { notFound(w, "package not found") },
			status:  http.StatusNotFound,
			code:    ErrCodeNotFound,
			message: "package not found",
		},
		{
			name:    "internalError",
			fn:      func(w http.ResponseWriter) { internalError(w, "boom") },
			status:  http.StatusInternalServerError,
			code:    ErrCodeInternal,
			message: "boom",
		},
		{
			name: "upstream",
			fn: func(w http.ResponseWriter) {
				writeError(w, http.StatusBadGateway, ErrCodeUpstream, "registry unreachable")
			},
			status:  http.StatusBadGateway,
			code:    ErrCodeUpstream,
			message: "registry unreachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.fn(w)

			if w.Code != tt.status {
				t.Errorf("status = %d, want %d", w.Code, tt.status)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response body is not valid JSON: %v (body: %q)", err, w.Body.String())
			}
			if resp.Code != tt.code {
				t.Errorf("code = %q, want %q", resp.Code, tt.code)
			}
			if resp.Message != tt.message {
				t.Errorf("message = %q, want %q", resp.Message, tt.message)
			}
		})
	}
}

func TestAPIErrorResponseShape(t *testing.T) {
	w := httptest.NewRecorder()
	badRequest(w, "x")

	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := raw["code"]; !ok {
		t.Error("response missing 'code' field")
	}
	if _, ok := raw["message"]; !ok {
		t.Error("response missing 'message' field")
	}
	if len(raw) != 2 {
		t.Errorf("response has unexpected fields: %v", raw)
	}
}
