package httpclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// redirectServers is a scoped server recording the Authorization it gets,
// and an origin. Both redirect a path containing /to/ to what follows it, on
// the scoped server.
type redirectServers struct {
	scoped, origin *httptest.Server
	mu             sync.Mutex
	got            []string
}

func newRedirectServers(t *testing.T) *redirectServers {
	t.Helper()
	s := &redirectServers{}
	s.scoped = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, target, ok := strings.Cut(r.URL.Path, "/to/"); ok {
			http.Redirect(w, r, s.scoped.URL+"/"+target, http.StatusFound)
			return
		}
		s.mu.Lock()
		s.got = append(s.got, r.Header.Get("Authorization"))
		s.mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(s.scoped.Close)
	s.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, _ := strings.CutPrefix(r.URL.Path, "/to/")
		http.Redirect(w, r, s.scoped.URL+"/"+target, http.StatusFound)
	}))
	t.Cleanup(s.origin.Close)
	return s
}

func (s *redirectServers) get(t *testing.T, authForURL AuthFunc, rawURL string) string {
	t.Helper()
	s.mu.Lock()
	s.got = nil
	s.mu.Unlock()
	client := &http.Client{Transport: NewTransport(http.DefaultTransport, authForURL)}
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) != 1 {
		t.Fatalf("scoped server got %d requests, want 1", len(s.got))
	}
	return s.got[0]
}

func TestTransportKeepsCredentialsOutOfARedirectIntoTheirScope(t *testing.T) {
	s := newRedirectServers(t)
	scopes := func(scopes map[string]string) AuthFunc {
		return func(rawURL string) (string, string) {
			best := ""
			for prefix := range scopes {
				if strings.HasPrefix(rawURL, prefix) && len(prefix) > len(best) {
					best = prefix
				}
			}
			if best == "" {
				return "", ""
			}
			return "Authorization", scopes[best]
		}
	}

	for name, tc := range map[string]struct {
		auth  map[string]string
		start string
		want  string
	}{
		"from outside the scope": {
			map[string]string{s.scoped.URL + "/private": "Bearer secret"}, s.origin.URL + "/to/private/x", "",
		},
		"from another scope": {
			map[string]string{s.scoped.URL + "/private": "Bearer secret", s.origin.URL: "Bearer other"}, s.origin.URL + "/to/private/x", "",
		},
		"through the scope's host from outside it": {
			map[string]string{s.scoped.URL + "/private": "Bearer secret"}, s.origin.URL + "/to/to/private/x", "",
		},
		"through the scope from outside it": {
			map[string]string{s.scoped.URL + "/private": "Bearer secret"}, s.origin.URL + "/to/private/to/private/x", "",
		},
		"within the scope": {
			map[string]string{s.scoped.URL + "/private": "Bearer secret"}, s.scoped.URL + "/private/to/private/y", "Bearer secret",
		},
		"to a scope with the same credentials": {
			map[string]string{s.scoped.URL + "/private": "Bearer secret", s.origin.URL: "Bearer secret"}, s.origin.URL + "/to/private/x", "Bearer secret",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := s.get(t, scopes(tc.auth), tc.start); got != tc.want {
				t.Errorf("scoped server got Authorization %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTransportRedirectWithoutCredentialsStillGetsAToken(t *testing.T) {
	var registry *httptest.Server
	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = io.WriteString(w, `{"token":"anonymous"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer anonymous" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+registry.URL+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer registry.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, registry.URL+"/v2/x/y/manifests/latest", http.StatusFound)
	}))
	defer origin.Close()
	authForURL := func(rawURL string) (string, string) {
		if strings.HasPrefix(rawURL, origin.URL) {
			return "Authorization", "Bearer secret"
		}
		return "", ""
	}

	client := &http.Client{Transport: NewTransport(http.DefaultTransport, authForURL)}
	resp, err := client.Get(origin.URL + "/start")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 with an anonymous token", resp.StatusCode)
	}
}
