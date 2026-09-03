package handler

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/scanner"
)

// newEnabledScanGroup returns a scanner.Group that reports Enabled() true,
// so tests can exercise ServeScanFetch's normal signature-checking path
// rather than tripping its "scanning not configured" guard.
func newEnabledScanGroup(t testing.TB) *scanner.Group {
	t.Helper()
	g, err := scanner.NewGroup(config.ScanningConfig{
		Enabled:    true,
		Timeout:    "15s",
		SigningKey: "test-signing-key",
		Scanners: []config.ScannerConfig{
			{Name: "test-scanner", URL: "http://localhost/scan", Mode: "block"},
		},
	}, slog.Default())
	if err != nil {
		t.Fatalf("scanner.NewGroup() error: %v", err)
	}
	return g
}

func TestServeScanFetch_ValidToken(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")
	proxy.Scanners = newEnabledScanGroup(t)
	store.files["npm/lodash/4.17.21/lodash-4.17.21.tgz"] = []byte("artifact bytes")

	target := proxy.scanFetchURL("npm/lodash/4.17.21/lodash-4.17.21.tgz", time.Minute)

	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	proxy.ServeScanFetch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "artifact bytes" {
		t.Errorf("body = %q, want %q", w.Body.String(), "artifact bytes")
	}
}

func TestServeScanFetch_Expired(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")
	proxy.Scanners = newEnabledScanGroup(t)
	store.files["npm/lodash/4.17.21/lodash-4.17.21.tgz"] = []byte("artifact bytes")

	target := proxy.scanFetchURL("npm/lodash/4.17.21/lodash-4.17.21.tgz", -time.Minute)

	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	proxy.ServeScanFetch(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestServeScanFetch_TamperedSignature(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")
	proxy.Scanners = newEnabledScanGroup(t)
	store.files["npm/lodash/4.17.21/lodash-4.17.21.tgz"] = []byte("artifact bytes")

	target := proxy.scanFetchURL("npm/lodash/4.17.21/lodash-4.17.21.tgz", time.Minute)
	tampered := strings.Replace(target, "sig=", "sig=deadbeef", 1)

	req := httptest.NewRequest(http.MethodGet, tampered, nil)
	w := httptest.NewRecorder()
	proxy.ServeScanFetch(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestServeScanFetch_PathTraversal(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")
	proxy.Scanners = newEnabledScanGroup(t)

	target := proxy.scanFetchURL("../../etc/passwd", time.Minute)

	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	proxy.ServeScanFetch(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestServeScanFetch_ScanningDisabled(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")
	// proxy.Scanners left nil: scanning disabled.
	store.files["npm/lodash/4.17.21/lodash-4.17.21.tgz"] = []byte("artifact bytes")

	target := proxy.scanFetchURL("npm/lodash/4.17.21/lodash-4.17.21.tgz", time.Minute)

	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	proxy.ServeScanFetch(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when scanning is disabled", w.Code)
	}
}

func TestServeScanFetch_NoSigningKey(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	// proxy.ScanSigningKey left empty.
	proxy.Scanners = newEnabledScanGroup(t)
	store.files["npm/lodash/4.17.21/lodash-4.17.21.tgz"] = []byte("artifact bytes")

	target := proxy.scanFetchURL("npm/lodash/4.17.21/lodash-4.17.21.tgz", time.Minute)

	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	proxy.ServeScanFetch(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when no signing key is configured", w.Code)
	}
}

func TestServeScanFetch_MissingObject(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")
	proxy.Scanners = newEnabledScanGroup(t)

	target := proxy.scanFetchURL("npm/missing/1.0.0/missing-1.0.0.tgz", time.Minute)

	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	proxy.ServeScanFetch(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
