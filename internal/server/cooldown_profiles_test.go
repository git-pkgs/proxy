package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
)

func TestCooldownProfilesThroughServerStart(t *testing.T) {
	if os.Getenv("PROXY_TEST_PROFILES") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCooldownProfilesThroughServerStart$")
		cmd.Env = append(os.Environ(), "PROXY_TEST_PROFILES=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("profile server: %v\n%s", err, output)
		}
		return
	}
	upstream, counts := profileTestUpstream(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	cfg := config.Default()
	cfg.BaseURL = "http://" + listener.Addr().String()
	cfg.Database.Path = filepath.Join(t.TempDir(), "cache.db")
	cfg.Storage.URL = "file://" + t.TempDir()
	cfg.CacheMetadata = true
	cfg.MetadataTTL = "1h"
	cfg.Upstream.AllowLoopback = true
	cfg.Upstream.NPM = upstream.URL + "/npm"
	cfg.Upstream.Cargo = upstream.URL + "/cargo"
	cfg.Upstream.CargoDownload = upstream.URL + "/crates"
	cfg.Upstream.Gem = upstream.URL + "/gem"
	cfg.Upstream.Go = upstream.URL + "/go"
	cfg.Cooldown = config.CooldownConfig{Profiles: map[string]config.CooldownConfig{
		"3d": {Default: "3d"}, "7d": {Default: "7d"},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Start(listener) }()
	client := &http.Client{Timeout: 5 * time.Second}
	defer func() {
		client.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()
	get := func(path string, headers map[string]string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, cfg.BaseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, string(body)
	}
	assertProfileVersionLists(t, get)
	assertProfileRouting(t, cfg.BaseURL, get, counts)
	assertConcurrentProfileRequests(t, client, cfg.BaseURL)
}

type profileGetter func(string, map[string]string) (*http.Response, string)

func assertProfileVersionLists(t *testing.T, get profileGetter) {
	t.Helper()
	for _, registry := range []struct{ path, old, middle, newest string }{
		{"npm/demo", `"1.0.0"`, `"2.0.0"`, `"3.0.0"`},
		{"cargo/de/mo/demo", `"vers":"1.0.0"`, `"vers":"2.0.0"`, `"vers":"3.0.0"`},
		{"gem/info/demo", "1.0.0 |", "2.0.0 |", "3.0.0 |"},
		{"go/example.com/demo/@v/list", "v1.0.0", "v2.0.0", "v3.0.0"},
	} {
		t.Run(registry.path, func(t *testing.T) {
			for _, profile := range []struct {
				prefix         string
				middle, newest bool
			}{
				{"/cooldown/3d/", true, false}, {"/cooldown/7d/", false, false}, {"/", true, true},
			} {
				resp, body := get(profile.prefix+registry.path, nil)
				if resp.StatusCode != http.StatusOK || !strings.Contains(body, registry.old) ||
					strings.Contains(body, registry.middle) != profile.middle || strings.Contains(body, registry.newest) != profile.newest {
					t.Fatalf("%s%s: status %d, body %s", profile.prefix, registry.path, resp.StatusCode, body)
				}
			}
		})
	}
}

func assertProfileRouting(t *testing.T, baseURL string, get profileGetter, counts func(string) int) {
	t.Helper()
	for _, profile := range []string{"3d", "7d"} {
		_, body := get("/cooldown/"+profile+"/cargo/config.json", nil)
		if !strings.Contains(body, baseURL+"/cooldown/"+profile+"/cargo/crates/") {
			t.Fatalf("Cargo download URL lost profile: %s", body)
		}
		_, body = get("/cooldown/"+profile+"/npm/demo", nil)
		if !strings.Contains(body, baseURL+"/cooldown/"+profile+"/npm/demo/-/") {
			t.Fatalf("npm download URL lost profile: %s", body)
		}
	}
	loose, _ := get("/cooldown/3d/npm/demo", nil)
	strict, _ := get("/cooldown/7d/npm/demo", map[string]string{"If-None-Match": loose.Header.Get("ETag")})
	if strict.StatusCode != http.StatusOK || strict.Header.Get("ETag") == loose.Header.Get("ETag") {
		t.Fatalf("cross-profile validator returned status %d", strict.StatusCode)
	}
	resp, _ := get("/cooldown/7d/npm/demo", map[string]string{"If-None-Match": strict.Header.Get("ETag")})
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("same-profile revalidation returned %d", resp.StatusCode)
	}
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/cooldown/3d/npm/demo/-/demo-2.0.0.tgz", http.StatusOK},
		{"/cooldown/7d/npm/demo/-/demo-2.0.0.tgz", http.StatusNotFound},
		{"/cooldown/3d/npm/demo/-/demo-2.0.0.tgz", http.StatusOK},
		{"/cooldown/7d/go/example.com/demo/@v/v2.0.0.mod", http.StatusForbidden},
		{"/cooldown/unknown/npm/demo", http.StatusNotFound},
		{"/cooldown/3d/ui/", http.StatusNotFound},
		{"/cooldown/3d/api/packages", http.StatusNotFound},
		{"/cooldown/3d/metrics", http.StatusNotFound},
	} {
		resp, _ := get(tc.path, nil)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d", tc.path, resp.StatusCode, tc.status)
		}
	}
	if got := counts("/npm/demo/-/demo-2.0.0.tgz"); got != 1 {
		t.Errorf("artifact upstream fetches = %d, want 1", got)
	}
}

func assertConcurrentProfileRequests(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	var wg sync.WaitGroup
	for range 8 {
		for _, profile := range []string{"3d", "7d"} {
			wg.Go(func() {
				resp, err := client.Get(baseURL + "/cooldown/" + profile + "/npm/demo")
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = resp.Body.Close() }()
				var doc struct{ Versions map[string]json.RawMessage }
				if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
					t.Error(err)
					return
				}
				_, middle := doc.Versions["2.0.0"]
				if middle != (profile == "3d") {
					t.Errorf("%s served another profile's versions", profile)
				}
			})
		}
	}
	wg.Wait()
}

func profileTestUpstream(t *testing.T) (*httptest.Server, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	counts := make(map[string]int)
	ages := []time.Duration{10 * 24 * time.Hour, 5 * 24 * time.Hour, 24 * time.Hour}
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/npm/demo":
			versions := map[string]any{}
			times := map[string]string{}
			for i, age := range ages {
				version := fmt.Sprintf("%d.0.0", i+1)
				times[version] = time.Now().Add(-age).UTC().Format(time.RFC3339)
				versions[version] = map[string]any{"name": "demo", "version": version, "dist": map[string]string{
					"tarball": upstream.URL + "/npm/demo/-/demo-" + version + ".tgz",
				}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "demo", "versions": versions, "time": times, "dist-tags": map[string]string{"latest": "3.0.0"}})
		case "/npm/demo/-/demo-2.0.0.tgz":
			_, _ = io.WriteString(w, "cached package bytes")
		case "/cargo/de/mo/demo":
			for i, age := range ages {
				_, _ = fmt.Fprintf(w, "{\"name\":\"demo\",\"vers\":\"%d.0.0\",\"deps\":[],\"features\":{},\"yanked\":false,\"pubtime\":%q}\n", i+1, time.Now().Add(-age).UTC().Format(time.RFC3339))
			}
		case "/gem/info/demo":
			_, _ = io.WriteString(w, "---\n1.0.0 |checksum:a\n2.0.0 |checksum:b\n3.0.0 |checksum:c\n")
		case "/gem/api/v1/versions/demo.json":
			versions := make([]map[string]string, 0, len(ages))
			for i, age := range ages {
				versions = append(versions, map[string]string{"number": fmt.Sprintf("%d.0.0", i+1), "created_at": time.Now().Add(-age).UTC().Format(time.RFC3339)})
			}
			_ = json.NewEncoder(w).Encode(versions)
		case "/go/example.com/demo/@v/list":
			_, _ = io.WriteString(w, "v1.0.0\nv2.0.0\nv3.0.0\n")
		default:
			for i, age := range ages {
				if r.URL.Path == fmt.Sprintf("/go/example.com/demo/@v/v%d.0.0.info", i+1) {
					_ = json.NewEncoder(w).Encode(map[string]string{"Version": fmt.Sprintf("v%d.0.0", i+1), "Time": time.Now().Add(-age).UTC().Format(time.RFC3339)})
					return
				}
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream, func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[path]
	}
}
