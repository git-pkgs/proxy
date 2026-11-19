package httpclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// tokenService issues "secret-token" only to requests carrying the configured
// credentials, and "anonymous-token" otherwise.
func tokenService(t *testing.T, issued *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issued.Add(1)
		token := "anonymous-token"
		if r.Header.Get("Authorization") == "Basic Y29ycDpwYXNz" {
			token = "secret-token"
		}
		_, _ = io.WriteString(w, `{"token":"`+token+`","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// challengingRegistry answers every request without a bearer token with a
// challenge naming realm, and records the Authorization headers it receives.
func challengingRegistry(t *testing.T, realm string, received *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		*received = append(*received, auth)
		if !strings.HasPrefix(auth, "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`/token",service="registry.corp",scope="repository:corp/private:pull"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "blob")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func credentialsFor(prefix string) AuthFunc {
	return func(rawURL string) (string, string) {
		if strings.HasPrefix(rawURL, prefix) {
			return "Authorization", "Basic Y29ycDpwYXNz"
		}
		return "", ""
	}
}

func get(t *testing.T, client *http.Client, rawURL string) int {
	t.Helper()
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestTransportDoesNotSendCredentialedTokenToAnotherSite is a regression test
// for a credential leak. Any host the proxy fetches from, such as one named in
// a Composer dist URL or a Helm index, could answer with a challenge naming a
// token service the proxy has credentials for. The proxy fetched a token with
// those credentials and sent it to that host.
func TestTransportDoesNotSendCredentialedTokenToAnotherSite(t *testing.T) {
	var issued atomic.Int32
	realm := tokenService(t, &issued)
	var received []string
	evil := challengingRegistry(t, realm.URL, &received)

	client := &http.Client{Transport: NewTransport(http.DefaultTransport, credentialsFor(realm.URL))}
	if status := get(t, client, evil.URL+"/v2/corp/private/blobs/sha256:abc"); status != http.StatusUnauthorized {
		t.Errorf("status = %d, want the challenge passed back as 401", status)
	}
	for _, auth := range received {
		if auth != "" {
			t.Errorf("the challenging host received Authorization %q", auth)
		}
	}
	if n := issued.Load(); n != 0 {
		t.Errorf("token service was asked %d times on the challenging host's behalf, want 0", n)
	}
}

// TestTransportDoesNotSendCachedCredentialedTokenToAnotherSite checks the
// same for a token already cached from a legitimate pull: a challenge that
// names the same realm, service and scope must not be handed it.
func TestTransportDoesNotSendCachedCredentialedTokenToAnotherSite(t *testing.T) {
	var issued atomic.Int32
	realm := tokenService(t, &issued)
	var legitimate, leaked []string
	registry := challengingRegistry(t, realm.URL, &legitimate)
	evil := challengingRegistry(t, realm.URL, &leaked)

	// The registry itself has configured credentials, so it is trusted with
	// tokens from its realm.
	auth := func(rawURL string) (string, string) {
		if strings.HasPrefix(rawURL, realm.URL) || strings.HasPrefix(rawURL, registry.URL) {
			return "Authorization", "Basic Y29ycDpwYXNz"
		}
		return "", ""
	}
	client := &http.Client{Transport: NewTransport(http.DefaultTransport, auth)}
	if status := get(t, client, registry.URL+"/v2/corp/private/blobs/sha256:abc"); status != http.StatusOK {
		t.Fatalf("legitimate pull: status = %d, want 200", status)
	}
	if got := legitimate[len(legitimate)-1]; got != "Bearer secret-token" {
		t.Fatalf("legitimate pull sent %q, want the credentialed token", got)
	}

	get(t, client, evil.URL+"/v2/corp/private/blobs/sha256:abc")
	for _, got := range leaked {
		if strings.HasPrefix(got, "Bearer ") {
			t.Errorf("the challenging host received %q", got)
		}
	}
}

// TestTransportFollowsAnonymousTokenServiceOnAnotherHost keeps public pulls
// working where the token service is on another host and no credentials are
// configured for it: the token is anonymous, so any registry may have it.
func TestTransportFollowsAnonymousTokenServiceOnAnotherHost(t *testing.T) {
	var issued atomic.Int32
	realm := tokenService(t, &issued)
	var received []string
	registry := challengingRegistry(t, realm.URL, &received)

	client := &http.Client{Transport: NewTransport(http.DefaultTransport, credentialsFor("https://elsewhere.example"))}
	if status := get(t, client, registry.URL+"/v2/library/alpine/blobs/sha256:abc"); status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if got := received[len(received)-1]; got != "Bearer anonymous-token" {
		t.Errorf("registry received %q, want the anonymous token", got)
	}
}

// TestTransportDoesNotSendCredentialedTokenToHostWithOtherCredentials checks
// that a registry with credentials of its own, but not the realm's, is not
// handed a token minted from the realm's credentials.
func TestTransportDoesNotSendCredentialedTokenToHostWithOtherCredentials(t *testing.T) {
	var issued atomic.Int32
	realm := tokenService(t, &issued)
	var received []string
	vendor := challengingRegistry(t, realm.URL, &received)

	auth := func(rawURL string) (string, string) {
		switch {
		case strings.HasPrefix(rawURL, realm.URL):
			return "Authorization", "Basic Y29ycDpwYXNz"
		case strings.HasPrefix(rawURL, vendor.URL):
			return "Authorization", "Basic dmVuZG9yOmtleQ=="
		}
		return "", ""
	}
	client := &http.Client{Transport: NewTransport(http.DefaultTransport, auth)}
	if status := get(t, client, vendor.URL+"/v2/corp/private/blobs/sha256:abc"); status != http.StatusUnauthorized {
		t.Errorf("status = %d, want the challenge passed back as 401", status)
	}
	for _, got := range received {
		if got != "Basic dmVuZG9yOmtleQ==" {
			t.Errorf("the vendor registry received Authorization %q, want only its own credentials", got)
		}
	}
	if n := issued.Load(); n != 0 {
		t.Errorf("token service was asked %d times on the vendor registry's behalf, want 0", n)
	}
}

// TestTransportSendsCredentialedTokenToRegistryWithHeaderSpelledDifferently
// checks that a registry whose credentials differ from the realm's only in
// the case of the header name is still handed the realm's token.
func TestTransportSendsCredentialedTokenToRegistryWithHeaderSpelledDifferently(t *testing.T) {
	var issued atomic.Int32
	realm := tokenService(t, &issued)
	var received []string
	registry := challengingRegistry(t, realm.URL, &received)

	auth := func(rawURL string) (string, string) {
		switch {
		case strings.HasPrefix(rawURL, realm.URL):
			return "Authorization", "Basic Y29ycDpwYXNz"
		case strings.HasPrefix(rawURL, registry.URL):
			return "authorization", "Basic Y29ycDpwYXNz"
		}
		return "", ""
	}
	client := &http.Client{Transport: NewTransport(http.DefaultTransport, auth)}
	if status := get(t, client, registry.URL+"/v2/corp/private/blobs/sha256:abc"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := received[len(received)-1]; got != "Bearer secret-token" {
		t.Errorf("registry received %q, want the credentialed token", got)
	}
}

// TestTransportRequestsPullScopeForRequestedRepository checks that the token
// is requested for pull access to the repository in the request path, not
// whatever scope the challenge names.
func TestTransportRequestsPullScopeForRequestedRepository(t *testing.T) {
	var scopes [][]string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			scopes = append(scopes, r.URL.Query()["scope"])
			_, _ = io.WriteString(w, `{"token":"t","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer t" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="registry.corp",scope="repository:corp/other:pull,push"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "blob")
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: NewTransport(http.DefaultTransport, credentialsFor(srv.URL))}
	if status := get(t, client, srv.URL+"/v2/corp/app/manifests/latest"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(scopes) != 1 || len(scopes[0]) != 1 || scopes[0][0] != "repository:corp/app:pull" {
		t.Errorf("token requested with scopes %q, want [[repository:corp/app:pull]]", scopes)
	}
}

func TestSameTokenService(t *testing.T) {
	for _, tt := range []struct {
		registry, realm string
		want            bool
	}{
		{"https://registry-1.docker.io", "https://auth.docker.io/token", true},
		{"https://index.docker.io", "https://auth.docker.io/token", true},
		{"https://registry.gitlab.com", "https://gitlab.com/jwt/auth", true},
		{"https://ghcr.io", "https://ghcr.io/token", true},
		{"https://harbor.corp.example", "https://harbor.corp.example/service/token", true},
		{"https://registry.corp.example", "https://auth.corp.example", false},
		{"https://evil.example", "https://auth.docker.io/token", false},
		{"https://docker.io.evil.example", "https://auth.docker.io/token", false},
		{"https://evil.jfrog.io", "https://myco.jfrog.io/artifactory/api/docker/token", false},
		{"https://evil.azurecr.io", "https://myco.azurecr.io/oauth2/token", false},
		{"https://gitlab.com", "https://registry.gitlab.com", false},
		{"http://registry-1.docker.io", "https://auth.docker.io/token", false},
		{"https://registry-1.docker.io", "https://auth.docker.io:8443/token", false},
		{"http://127.0.0.1:5000", "http://127.0.0.1:5000/token", true},
		{"http://127.0.0.1:5000", "http://127.0.0.1:5001/token", false},
		{"http://localhost:5000", "https://localhost:5000/token", false},
	} {
		if got := sameTokenService(mustParse(t, tt.registry), mustParse(t, tt.realm)); got != tt.want {
			t.Errorf("sameTokenService(%s, %s) = %v, want %v", tt.registry, tt.realm, got, tt.want)
		}
	}
}

func mustParse(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
