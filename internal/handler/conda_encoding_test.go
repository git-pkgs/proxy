package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

func TestCondaRepodataGzip(t *testing.T) {
	// Model a large index without allocating hundreds of megabytes: only its
	// compressed representation fits the configured metadata limit.
	plain := []byte(`{"packages":{},"padding":"` + strings.Repeat("x", 8192) + `"}`)
	compressed := gzipPayload(t, plain)
	for _, tt := range []struct {
		filename string
		mode     string
	}{
		{"repodata.json", "stream"},
		{"repodata.json", "cached"},
		{"repodata.json", "stale"},
		{"current_repodata.json", "stream"},
		{"current_repodata.json", "cached"},
		{"current_repodata.json", "stale"},
	} {
		t.Run(tt.filename+"/"+tt.mode, func(t *testing.T) {
			upstream := newGzipWhenAskedUpstream(plain, compressed)
			defer upstream.Close()
			proxy, db, _, _ := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.CacheMetadata = tt.mode != "stream"
			proxy.MetadataMaxSize = int64(len(compressed))
			if tt.mode == "cached" {
				proxy.MetadataTTL = time.Hour
			}
			h := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL)
			handler := h.Routes()
			serve := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conda-forge/linux-64/"+tt.filename, nil))
				return w
			}
			assertGzipResponse(t, "first", serve(), compressed)
			if got := upstream.sawAcceptEncoding(); got != "gzip" {
				t.Fatalf("upstream Accept-Encoding = %q, want gzip", got)
			}
			if tt.mode == "stream" {
				return
			}
			entry, err := db.GetMetadataCache("conda", h.metadataCacheKey("/conda-forge/linux-64/"+tt.filename))
			if err != nil {
				t.Fatal(err)
			}
			if entry == nil || entry.ContentEncoding.String != "gzip" || entry.Size.Int64 != int64(len(compressed)) {
				t.Fatalf("cache entry does not describe compressed bytes: %+v", entry)
			}
			upstream.available.Store(false)
			assertGzipResponse(t, "offline replay", serve(), compressed)
			wantRequests := int32(1)
			if tt.mode == "stale" {
				wantRequests = 2
			}
			if got := upstream.requests.Load(); got != wantRequests {
				t.Errorf("upstream requests = %d, want %d", got, wantRequests)
			}
		})
	}
}

func TestCondaRepodataBzip2StaysIdentity(t *testing.T) {
	plain := []byte("BZh already compressed repodata")
	for _, cache := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "cached"}[cache], func(t *testing.T) {
			upstream := newGzipWhenAskedUpstream(plain, gzipPayload(t, plain))
			defer upstream.Close()
			proxy, _, _, _ := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.CacheMetadata = cache
			handler := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conda-forge/linux-64/repodata.json.bz2", nil))
			if got := upstream.sawAcceptEncoding(); got != "identity" {
				t.Errorf("upstream Accept-Encoding = %q, want identity", got)
			}
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), plain) || w.Header().Get(headerContentEncoding) != "" {
				t.Fatalf("unexpected response: %d, %v, %q", w.Code, w.Header(), w.Body.String())
			}
		})
	}
}

func TestCondaRepodataCacheSizeLimit(t *testing.T) {
	plain := []byte(`{"packages":{},"padding":"` + strings.Repeat("x", 8192) + `"}`)
	compressed := gzipPayload(t, plain)
	for _, tt := range []struct {
		name     string
		body     []byte
		encoding string
		limit    int64
		status   int
	}{
		{"identity fallback", plain, "", int64(len(plain)), http.StatusOK},
		{"identity too large", plain, "", int64(len(plain) - 1), http.StatusBadGateway},
		{"gzip too large", compressed, "gzip", int64(len(compressed) - 1), http.StatusBadGateway},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get(headerAcceptEncoding); got != "gzip" {
					t.Errorf("upstream Accept-Encoding = %q, want gzip", got)
				}
				w.Header().Set(headerContentEncoding, tt.encoding)
				_, _ = w.Write(tt.body)
			}))
			defer upstream.Close()
			proxy, db, _, _ := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.CacheMetadata = true
			proxy.MetadataMaxSize = tt.limit
			h := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL)
			handler := h.Routes()
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conda-forge/linux-64/repodata.json", nil))
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body.String())
			}
			if tt.status == http.StatusOK {
				if !bytes.Equal(w.Body.Bytes(), plain) || w.Header().Get(headerContentEncoding) != "" {
					t.Fatal("identity response was changed or mislabeled as gzip")
				}
			} else if entry, _ := db.GetMetadataCache("conda", h.metadataCacheKey("/conda-forge/linux-64/repodata.json")); entry != nil {
				t.Fatal("oversized response was cached")
			}
		})
	}
}

func TestCondaRepodataCooldownEncoding(t *testing.T) {
	plain, err := json.Marshal(map[string]any{
		"packages": map[string]any{
			"old.tar.bz2": map[string]any{"name": "demo", "timestamp": time.Now().Add(-7 * 24 * time.Hour).UnixMilli()},
			"new.tar.bz2": map[string]any{"name": "demo", "timestamp": time.Now().UnixMilli()},
		},
		"packages.conda": map[string]any{
			"old.conda": map[string]any{"name": "demo", "timestamp": time.Now().Add(-7 * 24 * time.Hour).UnixMilli()},
			"new.conda": map[string]any{"name": "demo", "timestamp": time.Now().UnixMilli()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, encoding := range []string{"identity", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			body := plain
			if encoding == "gzip" {
				body = gzipPayload(t, plain)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(headerContentEncoding, encoding)
				_, _ = w.Write(body)
			}))
			defer upstream.Close()
			proxy := testProxy()
			proxy.HTTPClient = upstream.Client()
			proxy.Cooldown = &cooldown.Config{Default: "3d"}
			handler := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()
			for _, filename := range []string{"repodata.json", "current_repodata.json"} {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conda-forge/linux-64/"+filename, nil))
				if w.Code != http.StatusOK || w.Header().Get(headerContentEncoding) != "" {
					t.Fatalf("unexpected response: %d, %v", w.Code, w.Header())
				}
				var result map[string]map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				for key, old := range map[string]string{"packages": "old.tar.bz2", "packages.conda": "old.conda"} {
					if len(result[key]) != 1 || result[key][old] == nil {
						t.Errorf("%s: cooldown did not retain only the old package: %s", key, w.Body.String())
					}
				}
			}
		})
	}
}

func TestCondaRepodataCooldownRejectsInvalidGzip(t *testing.T) {
	compressed := gzipPayload(t, []byte(`{"padding":"`+strings.Repeat("x", 8192)+`"}`))
	for _, tt := range []struct {
		name string
		body []byte
	}{
		{"invalid header", []byte("not gzip")},
		{"truncated body", compressed[:len(compressed)-4]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(headerContentEncoding, "gzip")
				_, _ = w.Write(tt.body)
			}))
			defer upstream.Close()
			proxy := testProxy()
			proxy.HTTPClient = upstream.Client()
			proxy.Cooldown = &cooldown.Config{Default: "3d"}
			handler := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conda-forge/linux-64/repodata.json", nil))
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", w.Code)
			}
		})
	}
}

// largeRepodata builds repodata with n old and n new entries in each of
// packages and packages.conda.
func largeRepodata(n int) []byte {
	old := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	recent := time.Now().UnixMilli()
	var b strings.Builder
	b.WriteString(`{"info":{"subdir":"linux-64"},"repodata_version":1`)
	for _, key := range []string{"packages", "packages.conda"} {
		fmt.Fprintf(&b, `,%q:{`, key)
		for i := range 2 * n {
			if i > 0 {
				b.WriteByte(',')
			}
			ts, kind := old, "old"
			if i%2 == 1 {
				ts, kind = recent, "new"
			}
			fmt.Fprintf(&b, `"pkg-%d-%s.conda":{"name":"pkg%d","version":"1.0","timestamp":%d,"depends":["python >=3.10,<4","libgcc >=13"],"md5":"%032d"}`,
				i, kind, i, ts, i)
		}
		b.WriteByte('}')
	}
	b.WriteString(`,"removed":[]}`)
	return []byte(b.String())
}

func TestCondaRepodataCooldownStreamsPastMetadataLimit(t *testing.T) {
	plain := largeRepodata(2000)
	compressed := gzipPayload(t, plain)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentEncoding, "gzip")
		_, _ = w.Write(compressed)
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.HTTPClient = upstream.Client()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	proxy.MetadataMaxSize = int64(len(plain) / 10)
	handler := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conda-forge/linux-64/repodata.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for repodata %d times the metadata limit", w.Code, len(plain)/int(proxy.MetadataMaxSize))
	}
	var result struct {
		Info     map[string]any             `json:"info"`
		Packages map[string]json.RawMessage `json:"packages"`
		Conda    map[string]json.RawMessage `json:"packages.conda"`
		Removed  []string                   `json:"removed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(result.Packages) != 2000 || len(result.Conda) != 2000 || result.Info["subdir"] != "linux-64" || result.Removed == nil {
		t.Fatalf("got %d packages, %d conda packages, info %v", len(result.Packages), len(result.Conda), result.Info)
	}
	for name := range result.Packages {
		if strings.HasSuffix(name, "-new.conda") {
			t.Fatalf("%s is inside the cooldown but was kept", name)
		}
	}
	want := `"pkg-0-old.conda":{"name":"pkg0","version":"1.0","timestamp":`
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("kept entries should keep their upstream bytes")
	}
}

func TestCondaRepodataCooldownAbortsTruncatedStream(t *testing.T) {
	compressed := gzipPayload(t, largeRepodata(2000))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentEncoding, "gzip")
		_, _ = w.Write(compressed[:len(compressed)*3/4])
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.HTTPClient = upstream.Client()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	server := httptest.NewServer(NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes())
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/conda-forge/linux-64/repodata.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want the stream to start before the upstream body breaks", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("read a truncated repodata response without an error")
	}
}
