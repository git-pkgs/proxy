package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHomebrewBottleImage(t *testing.T) {
	formulas := map[string]string{
		"/formula/hello.json": `{"versions":{"stable":"2.12.3"},"revision":0,
			"bottle":{"stable":{"rebuild":1,"files":{"arm64_linux":{}}}}}`,
		"/formula/openssl@3.json": `{"versions":{"stable":"3.5.0"},"revision":2,
			"bottle":{"stable":{"rebuild":0,"files":{"arm64_sequoia":{}}}}}`,
		"/formula/libsigc++.json": `{"versions":{"stable":"3.6.0"},"revision":0,
			"bottle":{"stable":{"rebuild":0,"files":{"x86_64_linux":{}}}}}`,
		"/formula/nobottle.json": `{"versions":{"stable":"1.0"},"bottle":{}}`,
	}
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, ok := formulas[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer api.Close()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = api.Client()
	h := NewHomebrewHandler(proxy, api.URL)

	tests := []struct {
		formula, version string
		image, tag, err  string
	}{
		{formula: "hello", version: "2.12.3", image: "homebrew/core/hello", tag: "2.12.3-1"},
		{formula: "openssl@3", version: "3.5.0", image: "homebrew/core/openssl/3", tag: "3.5.0_2"},
		{formula: "libsigc++", version: "3.6.0", image: "homebrew/core/libsigcxx", tag: "3.6.0"},
		// Only the current version is described; another is the tag as given.
		{formula: "hello", version: "2.12.2", image: "homebrew/core/hello", tag: "2.12.2"},
		{formula: "hello", version: "2.12.3-1", image: "homebrew/core/hello", tag: "2.12.3-1"},
		{formula: "nobottle", version: "1.0", err: "has no bottles"},
		{formula: "missing", version: "1.0", err: `no formula "missing"`},
	}
	for _, tt := range tests {
		ref, err := h.BottleImage(context.Background(), tt.formula, tt.version)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s@%s: err = %v, want %q", tt.formula, tt.version, err, tt.err)
			}
			continue
		}
		if err != nil || ref.Name != tt.image || len(ref.References) != 1 || ref.References[0] != tt.tag {
			t.Errorf("%s@%s = %+v, %v; want %s:%s", tt.formula, tt.version, ref, err, tt.image, tt.tag)
		}
	}

	before := requests.Load()
	for _, bad := range [][2]string{{"../hello", "1.0"}, {"Hello", "1.0"}, {"hello", "1.0/../x"}, {"hello", ""}} {
		if _, err := h.BottleImage(context.Background(), bad[0], bad[1]); err == nil {
			t.Errorf("%q@%q accepted", bad[0], bad[1])
		}
	}
	if requests.Load() != before {
		t.Error("an invalid name or version reached the API")
	}
}
