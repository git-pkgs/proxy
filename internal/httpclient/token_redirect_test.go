package httpclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestTransportDoesNotLendCredentialsToARedirectedTokenRequest has a
// registry name a token realm without credentials, which redirects to a token
// service that upstream.auth has credentials for. The token request must not
// carry them there: the token would be issued for the operator's account and
// handed to the registry.
func TestTransportDoesNotLendCredentialsToARedirectedTokenRequest(t *testing.T) {
	const credentials = "Basic Y29ycDpwYXNz"
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := "anonymous-token"
		if r.Header.Get("Authorization") == credentials {
			token = "credentialed-token"
		}
		_, _ = io.WriteString(w, `{"token":"`+token+`"}`)
	}))
	defer service.Close()
	var sent []string
	var registry *httptest.Server
	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			http.Redirect(w, r, service.URL+"/token?scope=repository:corp/private:pull", http.StatusFound)
			return
		}
		sent = append(sent, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+registry.URL+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer registry.Close()
	authForURL := func(rawURL string) (string, string) {
		if strings.HasPrefix(rawURL, service.URL) {
			return "Authorization", credentials
		}
		return "", ""
	}

	client := &http.Client{Transport: NewTransport(http.DefaultTransport, authForURL)}
	resp, err := client.Get(registry.URL + "/v2/x/y/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for _, got := range sent {
		if strings.Contains(got, "credentialed-token") {
			t.Fatalf("the registry was sent %q, a token issued with the configured credentials", got)
		}
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := sent[len(sent)-1]; got != "Bearer anonymous-token" {
		t.Errorf("registry received %q, want the anonymous token", got)
	}
}

// TestTokenRedirectKeepsCredentialsSpelledDifferently checks a redirect hop
// whose credentials differ from the realm's only in the case of the header
// name still gets them.
func TestTokenRedirectKeepsCredentialsSpelledDifferently(t *testing.T) {
	transport := NewTransport(http.DefaultTransport, func(rawURL string) (string, string) {
		if strings.HasPrefix(rawURL, "https://realm.example") {
			return "Authorization", "Bearer t"
		}
		return "authorization", "Bearer t"
	})
	hop := configuredTransport{parent: transport, realm: "https://realm.example/token"}
	target, _ := url.Parse("https://service.example/token")
	if !hop.sameCredentials(target) {
		t.Error("credentials differing only in header case were not kept")
	}
}
