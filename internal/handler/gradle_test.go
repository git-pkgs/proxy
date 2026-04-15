package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGradleBuildCacheHandler_PutGetHead(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewGradleBuildCacheHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	key := "a1b2c3d4e5f6"
	payload := "cache entry content"

	putReq, err := http.NewRequest(http.MethodPut, srv.URL+"/cache/"+key, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("failed to create PUT request: %v", err)
	}
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT request failed: %v", err)
	}
	_ = putResp.Body.Close()

	if putResp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d, want %d", putResp.StatusCode, http.StatusCreated)
	}

	getResp, err := http.Get(srv.URL + "/cache/" + key)
	if err != nil {
		t.Fatalf("GET request failed: %v", err)
	}
	defer func() { _ = getResp.Body.Close() }()

	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", getResp.StatusCode, http.StatusOK)
	}
	if getResp.Header.Get("Content-Type") != gradleBuildCacheContentType {
		t.Fatalf("GET Content-Type = %q, want %q", getResp.Header.Get("Content-Type"), gradleBuildCacheContentType)
	}

	body, _ := io.ReadAll(getResp.Body)
	if string(body) != payload {
		t.Fatalf("GET body = %q, want %q", body, payload)
	}

	headReq, err := http.NewRequest(http.MethodHead, srv.URL+"/cache/"+key, nil)
	if err != nil {
		t.Fatalf("failed to create HEAD request: %v", err)
	}
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatalf("HEAD request failed: %v", err)
	}
	defer func() { _ = headResp.Body.Close() }()

	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want %d", headResp.StatusCode, http.StatusOK)
	}
	body, _ = io.ReadAll(headResp.Body)
	if len(body) != 0 {
		t.Fatalf("HEAD body length = %d, want 0", len(body))
	}
}

func TestGradleBuildCacheHandler_RootKeyPath(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewGradleBuildCacheHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	key := "rootpathkey"
	putReq, err := http.NewRequest(http.MethodPut, srv.URL+"/"+key, strings.NewReader("root"))
	if err != nil {
		t.Fatalf("failed to create PUT request: %v", err)
	}
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT request failed: %v", err)
	}
	_ = putResp.Body.Close()

	if putResp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d, want %d", putResp.StatusCode, http.StatusCreated)
	}

	getResp, err := http.Get(srv.URL + "/cache/" + key)
	if err != nil {
		t.Fatalf("GET request failed: %v", err)
	}
	defer func() { _ = getResp.Body.Close() }()

	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", getResp.StatusCode, http.StatusOK)
	}
}

func TestGradleBuildCacheHandler_GetMiss(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewGradleBuildCacheHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/cache/missing-key")
	if err != nil {
		t.Fatalf("GET request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestGradleBuildCacheHandler_MethodNotAllowed(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewGradleBuildCacheHandler(proxy, "http://localhost")

	req := httptest.NewRequest(http.MethodPost, "/cache/key", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestGradleBuildCacheHandler_PathTraversalRejected(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewGradleBuildCacheHandler(proxy, "http://localhost")

	req := httptest.NewRequest(http.MethodGet, "/cache/../secret", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestGradleBuildCacheHandler_PutOverwriteReturnsOK(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewGradleBuildCacheHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	key := "overwrite-key"

	for i, payload := range []string{"first", "second"} {
		req, err := http.NewRequest(http.MethodPut, srv.URL+"/cache/"+key, strings.NewReader(payload))
		if err != nil {
			t.Fatalf("failed to create PUT request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT request failed: %v", err)
		}
		_ = resp.Body.Close()

		want := http.StatusCreated
		if i == 1 {
			want = http.StatusOK
		}
		if resp.StatusCode != want {
			t.Fatalf("PUT #%d status = %d, want %d", i+1, resp.StatusCode, want)
		}
	}
}
