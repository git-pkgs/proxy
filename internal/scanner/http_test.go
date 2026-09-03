package scanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPScanner_Scan(t *testing.T) {
	tests := []struct {
		name        string
		respStatus  int
		respBody    string
		wantAllowed bool
		wantReason  string
		wantErr     bool
	}{
		{
			name:        "allowed",
			respStatus:  http.StatusOK,
			respBody:    `{"allowed": true}`,
			wantAllowed: true,
		},
		{
			name:        "blocked with reason and findings",
			respStatus:  http.StatusOK,
			respBody:    `{"allowed": false, "reason": "malware detected", "findings": [{"severity": "high", "title": "EICAR", "description": "test signature"}]}`,
			wantAllowed: false,
			wantReason:  "malware detected",
		},
		{
			name:       "non-200 status is an error",
			respStatus: http.StatusInternalServerError,
			respBody:   `{}`,
			wantErr:    true,
		},
		{
			name:       "malformed JSON is an error",
			respStatus: http.StatusOK,
			respBody:   `not json`,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got Request
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				if got.Ecosystem != "npm" || got.Name != "left-pad" || got.FetchURL == "" {
					t.Errorf("unexpected request body: %+v", got)
				}
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Errorf("missing/incorrect Authorization header: %q", r.Header.Get("Authorization"))
				}

				w.WriteHeader(tt.respStatus)
				_, _ = w.Write([]byte(tt.respBody))
			}))
			defer srv.Close()

			s := NewHTTPScanner("test", srv.URL, map[string]string{"Authorization": "Bearer secret"}, nil)

			result, err := s.Scan(context.Background(), Request{
				Ecosystem: "npm",
				Name:      "left-pad",
				Version:   "1.0.0",
				FetchURL:  srv.URL + "/fetch",
			})

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Scan() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Scan() unexpected error: %v", err)
			}
			if result.Allowed != tt.wantAllowed {
				t.Errorf("Allowed = %v, want %v", result.Allowed, tt.wantAllowed)
			}
			if result.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", result.Reason, tt.wantReason)
			}
		})
	}
}

func TestHTTPScanner_Scan_ContextTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"allowed": true}`))
	}))
	defer srv.Close()

	s := NewHTTPScanner("slow", srv.URL, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_, err := s.Scan(ctx, Request{Ecosystem: "npm", Name: "left-pad"})
	if err == nil {
		t.Fatal("Scan() error = nil, want timeout error")
	}
}

func TestHTTPScanner_Name(t *testing.T) {
	s := NewHTTPScanner("clamav", "http://example.invalid", nil, nil)
	if got := s.Name(); got != "clamav" {
		t.Errorf("Name() = %q, want %q", got, "clamav")
	}
}
