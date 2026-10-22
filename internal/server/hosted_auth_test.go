package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
)

func basicAuth(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

func TestRequireHostedToken(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
		wantStatus    int
		wantName      string
	}{
		{name: "missing header", authorization: "", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", authorization: "Bearer guess", wantStatus: http.StatusUnauthorized},
		{name: "token prefix only", authorization: "Bearer s3cre", wantStatus: http.StatusUnauthorized},
		{name: "basic without password", authorization: "Basic czNjcmV0", wantStatus: http.StatusUnauthorized},
		{name: "basic with wrong password", authorization: basicAuth("s3cret", "guess"), wantStatus: http.StatusUnauthorized},
		{name: "empty bearer", authorization: "Bearer ", wantStatus: http.StatusUnauthorized},
		{name: "other scheme", authorization: "Token s3cret", wantStatus: http.StatusUnauthorized},
		{name: "bearer", authorization: "Bearer s3cret", wantStatus: http.StatusNoContent, wantName: "hosted.token"},
		{name: "scheme is case-insensitive", authorization: "bearer s3cret", wantStatus: http.StatusNoContent, wantName: "hosted.token"},
		{name: "basic password", authorization: basicAuth("__token__", "s3cret"), wantStatus: http.StatusNoContent, wantName: "hosted.token"},
		{name: "bare token", authorization: "s3cret", wantStatus: http.StatusNoContent, wantName: "hosted.token"},
		{name: "scoped token", authorization: "Bearer ci-token", wantStatus: http.StatusNoContent, wantName: "ci"},
	}

	auth := newHostedAuth(config.HostedConfig{
		Token:  "s3cret",
		Tokens: []config.HostedToken{{Name: "ci", Token: "ci-token", Push: []string{"npm:@acme/*"}}},
	})
	var gotName string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotName = credentialFrom(r.Context()).name
		w.WriteHeader(http.StatusNoContent)
	})
	handler := requireHostedToken(auth)(next)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName = ""
			req := httptest.NewRequest(http.MethodPut, "/api/hosted/npm/request/2.88.3", nil)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if gotName != tt.wantName {
				t.Errorf("credential = %q, want %q", gotName, tt.wantName)
			}
			if tt.wantStatus != http.StatusUnauthorized {
				return
			}
			challenges := rec.Header().Values("WWW-Authenticate")
			if !slices.Contains(challenges, `Bearer realm="proxy"`) || !slices.Contains(challenges, `Basic realm="proxy"`) {
				t.Errorf("WWW-Authenticate = %q, want Bearer and Basic challenges", challenges)
			}
			var body ErrorResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decoding error body: %v", err)
			}
			if body.Code != ErrCodeUnauthorized {
				t.Errorf("error code = %q, want %q", body.Code, ErrCodeUnauthorized)
			}
		})
	}
}

func TestHostedCredentialMayPush(t *testing.T) {
	auth := newHostedAuth(config.HostedConfig{
		Token: "admin",
		Tokens: []config.HostedToken{{
			Name:  "ci",
			Token: "ci-token",
			Push:  []string{"npm:@acme/*", "npm:acme-*", "composer:Acme/*", "generic:tools"},
		}},
	})
	admin, ci := &auth.credentials[0], &auth.credentials[1]

	tests := []struct {
		ecosystem, name string
		want            bool
	}{
		{"npm", "@acme/widget", true},
		{"npm", "acme-cli", true},
		{"npm", "@other/widget", false},
		{"npm", "@acme/widget/extra", false},
		{"npm", "request", false},
		{"composer", "acme/widget", true},
		{"composer", "ACME/Widget", true},
		{"composer", "other/widget", false},
		{"generic", "tools", true},
		{"generic", "tools2", false},
		{"pypi", "acme-cli", false},
	}
	for _, tt := range tests {
		if got := ci.mayPush(tt.ecosystem, tt.name); got != tt.want {
			t.Errorf("ci.mayPush(%s, %s) = %v, want %v", tt.ecosystem, tt.name, got, tt.want)
		}
		if !admin.mayPush(tt.ecosystem, tt.name) {
			t.Errorf("hosted.token may not push %s %s", tt.ecosystem, tt.name)
		}
	}
}
