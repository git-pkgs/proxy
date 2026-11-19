package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// headThenFailTransport answers manifest HEADs with status and fails every
// GET, as a registry that starts timing out between the two requests.
type headThenFailTransport struct {
	parent http.RoundTripper
	status int
	active bool
}

func (t *headThenFailTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.active {
		return t.parent.RoundTrip(req)
	}
	if req.Method != http.MethodHead {
		return nil, errors.New("connection reset")
	}
	header := http.Header{}
	if t.status == http.StatusUnauthorized {
		header.Set("WWW-Authenticate", `Bearer realm="https://auth.example/token"`)
	}
	header.Set("Content-Length", "17")
	return &http.Response{StatusCode: t.status, Header: header, Body: http.NoBody, Request: req}, nil
}

// TestContainerStaleTagHeadRefusalIsNotServedFromCache checks a stale tag
// the registry refuses or no longer has on the HEAD isn't served from cache
// when the GET after it fails, while a registry that can't answer HEAD still
// gets the cached copy.
func TestContainerStaleTagHeadRefusalIsNotServedFromCache(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   int
		code   string
	}{
		{http.StatusUnauthorized, http.StatusUnauthorized, "UNAUTHORIZED"},
		{http.StatusForbidden, http.StatusForbidden, "DENIED"},
		{http.StatusNotFound, http.StatusNotFound, "MANIFEST_UNKNOWN"},
		{http.StatusGone, http.StatusGone, "MANIFEST_UNKNOWN"},
		{http.StatusMethodNotAllowed, http.StatusOK, ""},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			registry := newCountingRegistry(t)
			registry.pointLatest(registry.add(manifestDoc("a")))
			h := newRevalidateTestHandler(t, registry)
			transport := &headThenFailTransport{parent: registry.server.Client().Transport, status: tc.status}
			h.proxy.HTTPClient = &http.Client{Transport: transport}

			pullManifest(t, h, http.MethodGet, "latest")
			ageLatest(t, h)
			transport.active = true

			request := httptest.NewRequest(http.MethodGet, "/library/app/manifests/latest", nil)
			request.Header.Set("Accept", dockerManifestAccept)
			response := httptest.NewRecorder()
			h.Routes().ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
			if tc.code == "" {
				if response.Body.String() != manifestDoc("a") {
					t.Errorf("body = %q, want the cached manifest", response.Body.String())
				}
				return
			}
			if !strings.Contains(response.Body.String(), tc.code) {
				t.Errorf("body = %q, want error code %s", response.Body.String(), tc.code)
			}
			if tc.status == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") == "" {
				t.Error("the registry's WWW-Authenticate was dropped")
			}
		})
	}
}
