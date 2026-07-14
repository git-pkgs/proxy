package httpclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTransportFollowsBearerChallengeAndCachesToken(t *testing.T) {
	var registryRequests int
	var tokenRequests int
	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequests++
			if got := r.URL.Query().Get("service"); got != "registry.test" {
				t.Errorf("service = %q, want %q", got, "registry.test")
			}
			if got := r.URL.Query().Get("scope"); got != "repository:library/test:pull" {
				t.Errorf("scope = %q, want %q", got, "repository:library/test:pull")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"registry-token","expires_in":3600}`)
		case "/v2/library/test/blobs/sha256:first", "/v2/library/test/blobs/sha256:second":
			registryRequests++
			if r.Header.Get("Authorization") != "Bearer registry-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="registry.test",scope="repository:library/test:pull"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "blob")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &http.Client{Transport: NewTransport(http.DefaultTransport, nil)}
	for _, digest := range []string{"sha256:first", "sha256:second"} {
		resp, err := client.Get(server.URL + "/v2/library/test/blobs/" + digest)
		if err != nil {
			t.Fatalf("GET %s: %v", digest, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s response: %v", digest, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want %d", digest, resp.StatusCode, http.StatusOK)
		}
		if string(body) != "blob" {
			t.Errorf("GET %s body = %q, want %q", digest, body, "blob")
		}
	}

	if tokenRequests != 1 {
		t.Errorf("token requests = %d, want 1", tokenRequests)
	}
	if registryRequests != 3 {
		t.Errorf("registry requests = %d, want 3", registryRequests)
	}
}

func TestTransportAddsConfiguredAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Registry-Token"); got != "configured-token" {
			t.Errorf("X-Registry-Token = %q, want %q", got, "configured-token")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	authForURL := func(url string) (string, string) {
		if strings.HasPrefix(url, server.URL) {
			return "X-Registry-Token", "configured-token"
		}
		return "", ""
	}
	client := &http.Client{Transport: NewTransport(http.DefaultTransport, authForURL)}

	resp, err := client.Get(server.URL + "/metadata")
	if err != nil {
		t.Fatalf("GET metadata: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestTransportPreservesExplicitAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer explicit-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer explicit-token")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	authForURL := func(string) (string, string) {
		return "Authorization", "Bearer configured-token"
	}
	client := &http.Client{Transport: NewTransport(http.DefaultTransport, authForURL)}
	req, err := http.NewRequest(http.MethodGet, server.URL+"/artifact", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer explicit-token")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET artifact: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestTransportDoesNotReplaceExplicitAuthenticationAfterBearerChallenge(t *testing.T) {
	var registryRequests int
	var tokenRequests int
	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequests++
			_, _ = io.WriteString(w, `{"token":"registry-token"}`)
		case "/v2/library/test/blobs/sha256:test":
			registryRequests++
			if got := r.Header.Get("Authorization"); got != "Bearer explicit-token" {
				t.Errorf("Authorization = %q, want %q", got, "Bearer explicit-token")
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &http.Client{Transport: NewTransport(http.DefaultTransport, nil)}
	req, err := http.NewRequest(http.MethodGet, server.URL+"/v2/library/test/blobs/sha256:test", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer explicit-token")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET blob: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if registryRequests != 1 {
		t.Errorf("registry requests = %d, want 1", registryRequests)
	}
	if tokenRequests != 0 {
		t.Errorf("token requests = %d, want 0", tokenRequests)
	}
}

func TestTransportDoesNotForwardConfiguredAuthenticationOnTokenRedirect(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Registry-Token"); got != "" {
			t.Errorf("redirected X-Registry-Token = %q, want empty", got)
		}
		_, _ = io.WriteString(w, `{"token":"registry-token"}`)
	}))
	defer destination.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Registry-Token"); got != "configured-token" {
			t.Errorf("source X-Registry-Token = %q, want %q", got, "configured-token")
		}
		http.Redirect(w, r, destination.URL+"/token", http.StatusFound)
	}))
	defer source.Close()

	authForURL := func(rawURL string) (string, string) {
		if strings.HasPrefix(rawURL, source.URL) {
			return "X-Registry-Token", "configured-token"
		}
		return "", ""
	}
	transport := NewTransport(http.DefaultTransport, authForURL)
	token, _, err := transport.fetchToken(context.Background(), bearerChallenge{realm: source.URL + "/token"})
	if err != nil {
		t.Fatalf("fetchToken: %v", err)
	}
	if token != "registry-token" {
		t.Errorf("token = %q, want %q", token, "registry-token")
	}
}

func TestTransportPrunesExpiredTokens(t *testing.T) {
	transport := NewTransport(http.DefaultTransport, nil)
	transport.tokens["expired-unused"] = cachedToken{
		value:     "expired-token",
		expiresAt: time.Now().Add(-time.Minute),
	}
	transport.cacheToken("current", cachedToken{
		value:     "current-token",
		expiresAt: time.Now().Add(time.Minute),
	})

	if got := transport.cachedToken("current"); got != "current-token" {
		t.Errorf("cachedToken(current) = %q, want %q", got, "current-token")
	}
	if _, ok := transport.tokens["expired-unused"]; ok {
		t.Error("expired unused token was not pruned")
	}
}

func TestTransportDoesNotFollowBearerChallengeOutsideOCIRegistry(t *testing.T) {
	tokenRequests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenRequests++
			_, _ = io.WriteString(w, `{"token":"unexpected"}`)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}))
	defer server.Close()

	client := &http.Client{Transport: NewTransport(http.DefaultTransport, nil)}
	resp, err := client.Get(server.URL + "/api/packages")
	if err != nil {
		t.Fatalf("GET API: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if tokenRequests != 0 {
		t.Errorf("token requests = %d, want 0", tokenRequests)
	}
}
