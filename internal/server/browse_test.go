package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
)

const testArchiveName = "test.tar.gz"

func TestHandleBrowseList(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// Create a test tar.gz archive
	archiveData := createTestArchive(t)
	artifactsDir := filepath.Join(ts.tempDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0755); err != nil {
		t.Fatalf("failed to create artifacts dir: %v", err)
	}
	storagePath := filepath.Join(artifactsDir, testArchiveName)
	if err := os.WriteFile(storagePath, archiveData, 0644); err != nil {
		t.Fatalf("failed to write test archive: %v", err)
	}
	// Storage path relative to artifacts directory
	relPath := testArchiveName

	// Setup test package and artifact
	pkg := &database.Package{
		PURL:      "pkg:npm/test-browse",
		Ecosystem: "npm",
		Name:      "test-browse",
	}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	ver := &database.Version{
		PURL:        "pkg:npm/test-browse@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := ts.db.UpsertVersion(ver); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	artifact := &database.Artifact{
		VersionPURL: ver.PURL,
		Filename:    "test-browse-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/test-browse/-/test-browse-1.0.0.tgz",
		StoragePath: sql.NullString{String: relPath, Valid: true},
	}
	if err := ts.db.UpsertArtifact(artifact); err != nil {
		t.Fatalf("failed to upsert artifact: %v", err)
	}

	// Test listing root directory
	req := httptest.NewRequest("GET", "/api/browse/npm/test-browse/1.0.0", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var response BrowseListResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(response.Files) == 0 {
		t.Error("expected files in response")
	}

	// Test listing subdirectory
	req = httptest.NewRequest("GET", "/api/browse/npm/test-browse/1.0.0?path=lib", nil)
	w = httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
}

func TestHandleBrowseFile(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// Create a test tar.gz archive
	archiveData := createTestArchive(t)
	artifactsDir := filepath.Join(ts.tempDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0755); err != nil {
		t.Fatalf("failed to create artifacts dir: %v", err)
	}
	storagePath := filepath.Join(artifactsDir, testArchiveName)
	if err := os.WriteFile(storagePath, archiveData, 0644); err != nil {
		t.Fatalf("failed to write test archive: %v", err)
	}
	// Storage path relative to artifacts directory
	relPath := testArchiveName

	// Setup test package and artifact
	pkg := &database.Package{
		PURL:      "pkg:npm/test-browse",
		Ecosystem: "npm",
		Name:      "test-browse",
	}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	ver := &database.Version{
		PURL:        "pkg:npm/test-browse@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := ts.db.UpsertVersion(ver); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	artifact := &database.Artifact{
		VersionPURL: ver.PURL,
		Filename:    "test-browse-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/test-browse/-/test-browse-1.0.0.tgz",
		StoragePath: sql.NullString{String: relPath, Valid: true},
	}
	if err := ts.db.UpsertArtifact(artifact); err != nil {
		t.Fatalf("failed to upsert artifact: %v", err)
	}

	// Test fetching a file
	req := httptest.NewRequest("GET", "/api/browse/npm/test-browse/1.0.0/file/README.md", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if body != "# Test Package\n" {
		t.Errorf("unexpected file content: %q", body)
	}

	// Check content type
	contentType := w.Header().Get("Content-Type")
	if contentType != contentTypePlainText {
		t.Errorf("expected text/plain content type, got %q", contentType)
	}

	// Test fetching non-existent file
	req = httptest.NewRequest("GET", "/api/browse/npm/test-browse/1.0.0/file/nonexistent.txt", nil)
	w = httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404 for non-existent file, got %d", w.Code)
	}
}

func TestDetectContentType(t *testing.T) {
	tests := []struct {
		filename   string
		expectedCT string
	}{
		{"file.txt", contentTypePlainText},
		{"file.md", contentTypePlainText},
		{"file.json", "application/json; charset=utf-8"},
		{"file.js", "application/javascript; charset=utf-8"},
		{"file.go", "text/x-go; charset=utf-8"},
		{"file.py", "text/x-python; charset=utf-8"},
		{"file.rs", "text/x-rust; charset=utf-8"},
		{"file.png", "image/png"},
		{"file.jpg", "image/jpeg"},
		{"README", contentTypePlainText},
		{"LICENSE", contentTypePlainText},
		{"Makefile", contentTypePlainText},
		{".gitignore", contentTypePlainText},
		{"file.bin", "application/octet-stream"},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := detectContentType(tt.filename)
			if got != tt.expectedCT {
				t.Errorf("detectContentType(%q) = %q, want %q", tt.filename, got, tt.expectedCT)
			}
		})
	}
}

func TestIsLikelyText(t *testing.T) {
	tests := []struct {
		filename string
		expected bool
	}{
		{"README", true},
		{"README.md", true},
		{"LICENSE", true},
		{"Makefile", true},
		{"Dockerfile", true},
		{".gitignore", true},
		{"file.bin", false},
		{"data.dat", false},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := isLikelyText(tt.filename)
			if got != tt.expected {
				t.Errorf("isLikelyText(%q) = %v, want %v", tt.filename, got, tt.expected)
			}
		})
	}
}

// createTestArchive creates a tar.gz archive in memory with test files
// in npm format (with package/ prefix)
func createTestArchive(t *testing.T) []byte {
	t.Helper()

	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	files := map[string]string{
		"package/README.md":          "# Test Package\n",
		"package/package.json":       `{"name": "test-browse"}`,
		"package/lib/index.js":       "module.exports = {};",
		"package/lib/helper.js":      "module.exports.help = () => {};",
		"package/test/index.test.js": "// tests",
	}

	for path, content := range files {
		header := &tar.Header{
			Name: path,
			Size: int64(len(content)),
			Mode: 0644,
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("failed to write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("failed to write tar content: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}

	return buf.Bytes()
}

func TestBrowseNonCachedArtifact(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// Setup test package without cached artifact
	pkg := &database.Package{
		PURL:      "pkg:npm/not-cached",
		Ecosystem: "npm",
		Name:      "not-cached",
	}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	ver := &database.Version{
		PURL:        "pkg:npm/not-cached@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := ts.db.UpsertVersion(ver); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	artifact := &database.Artifact{
		VersionPURL: ver.PURL,
		Filename:    "not-cached-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/not-cached/-/not-cached-1.0.0.tgz",
		// No StoragePath - not cached
	}
	if err := ts.db.UpsertArtifact(artifact); err != nil {
		t.Fatalf("failed to upsert artifact: %v", err)
	}

	// Try to browse
	req := httptest.NewRequest("GET", "/api/browse/npm/not-cached/1.0.0", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404 for non-cached artifact, got %d", w.Code)
	}
}

func TestHandleBrowseSourcePage(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// Create a test tar.gz archive
	archiveData := createTestArchive(t)
	artifactsDir := filepath.Join(ts.tempDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0755); err != nil {
		t.Fatalf("failed to create artifacts dir: %v", err)
	}
	storagePath := filepath.Join(artifactsDir, testArchiveName)
	if err := os.WriteFile(storagePath, archiveData, 0644); err != nil {
		t.Fatalf("failed to write test archive: %v", err)
	}
	relPath := testArchiveName

	// Setup test package and artifact
	pkg := &database.Package{
		PURL:      "pkg:npm/test-browse",
		Ecosystem: "npm",
		Name:      "test-browse",
	}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	ver := &database.Version{
		PURL:        "pkg:npm/test-browse@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := ts.db.UpsertVersion(ver); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	artifact := &database.Artifact{
		VersionPURL: ver.PURL,
		Filename:    "test-browse-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/test-browse/-/test-browse-1.0.0.tgz",
		StoragePath: sql.NullString{String: relPath, Valid: true},
	}
	if err := ts.db.UpsertArtifact(artifact); err != nil {
		t.Fatalf("failed to upsert artifact: %v", err)
	}

	// Test the browse source page loads
	req := httptest.NewRequest("GET", "/package/npm/test-browse/1.0.0/browse", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()

	// Check that the page contains expected elements
	expectedStrings := []string{
		"Browse Source",
		"test-browse",
		"1.0.0",
		"file-tree",
		"file-content",
		"loadFileTree",
		"loadFile",
	}

	for _, expected := range expectedStrings {
		if !strings.Contains(body, expected) {
			t.Errorf("browse source page missing expected content: %q", expected)
		}
	}

	// Check that the escapeHTML function is present for XSS protection
	if !strings.Contains(body, "function escapeHTML(str)") {
		t.Error("browse source page missing escapeHTML function for XSS protection")
	}

	// Check that onclick handlers use escapeHTML
	if strings.Contains(body, "onclick=\"loadFileTree('${file.path}')") {
		t.Error("browse source page has unescaped file.path in onclick handler")
	}
	if strings.Contains(body, "onclick=\"loadFile('${file.path}')") {
		t.Error("browse source page has unescaped file.path in onclick handler")
	}

	// Check that ecosystem, package name, and version are set in JavaScript
	if !strings.Contains(body, "const ecosystem = 'npm'") {
		t.Error("browse source page missing ecosystem variable")
	}
	if !strings.Contains(body, "const packageName = 'test-browse'") {
		t.Error("browse source page missing packageName variable")
	}
	if !strings.Contains(body, "const version = '1.0.0'") {
		t.Error("browse source page missing version variable")
	}

	// Verify content type
	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		t.Errorf("expected HTML content type, got %q", contentType)
	}
}

func TestHandleCompareDiff(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// Create two test archives with different content
	archive1Data := createArchiveWithContent(t, map[string]string{
		"README.md": "# Version 1\n",
		"main.go":   "package main\n",
	})
	archive2Data := createArchiveWithContent(t, map[string]string{
		"README.md": "# Version 2\n",
		"main.go":   "package main\n\nfunc main() {}\n",
		"new.txt":   "new file\n",
	})

	artifactsDir := filepath.Join(ts.tempDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0755); err != nil {
		t.Fatalf("failed to create artifacts dir: %v", err)
	}

	// Write archives
	if err := os.WriteFile(filepath.Join(artifactsDir, "v1.tar.gz"), archive1Data, 0644); err != nil {
		t.Fatalf("failed to write v1 archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, "v2.tar.gz"), archive2Data, 0644); err != nil {
		t.Fatalf("failed to write v2 archive: %v", err)
	}

	// Setup package and versions
	pkg := &database.Package{
		PURL:      "pkg:npm/test-compare",
		Ecosystem: "npm",
		Name:      "test-compare",
	}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	ver1 := &database.Version{
		PURL:        "pkg:npm/test-compare@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := ts.db.UpsertVersion(ver1); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	ver2 := &database.Version{
		PURL:        "pkg:npm/test-compare@2.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := ts.db.UpsertVersion(ver2); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	artifact1 := &database.Artifact{
		VersionPURL: ver1.PURL,
		Filename:    "test-compare-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/test-compare/-/test-compare-1.0.0.tgz",
		StoragePath: sql.NullString{String: "v1.tar.gz", Valid: true},
	}
	if err := ts.db.UpsertArtifact(artifact1); err != nil {
		t.Fatalf("failed to upsert artifact: %v", err)
	}

	artifact2 := &database.Artifact{
		VersionPURL: ver2.PURL,
		Filename:    "test-compare-2.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/test-compare/-/test-compare-2.0.0.tgz",
		StoragePath: sql.NullString{String: "v2.tar.gz", Valid: true},
	}
	if err := ts.db.UpsertArtifact(artifact2); err != nil {
		t.Fatalf("failed to upsert artifact: %v", err)
	}

	// Test the compare endpoint
	req := httptest.NewRequest("GET", "/api/compare/npm/test-compare/1.0.0/2.0.0", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Parse response
	var result map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Check that we have files
	files, ok := result["files"].([]interface{})
	if !ok {
		t.Fatal("response should have files array")
	}

	if len(files) == 0 {
		t.Error("should have detected file changes")
	}

	// Check counts exist
	if _, ok := result["files_changed"]; !ok {
		t.Error("response should have files_changed")
	}
	if _, ok := result["files_added"]; !ok {
		t.Error("response should have files_added")
	}
}

func createArchiveWithContent(t *testing.T, files map[string]string) []byte {
	t.Helper()

	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	// Add package/ prefix for npm-style archives
	for path, content := range files {
		prefixedPath := "package/" + path
		header := &tar.Header{
			Name: prefixedPath,
			Size: int64(len(content)),
			Mode: 0644,
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("failed to write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("failed to write tar content: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}

	return buf.Bytes()
}

func TestHandleComparePage(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// Test valid format with ... separator
	req := httptest.NewRequest("GET", "/package/npm/test/compare/1.0.0...2.0.0", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()

	// Check that versions are set correctly in JavaScript
	if !strings.Contains(body, "const fromVersion = '1.0.0'") {
		t.Error("page should set fromVersion")
	}
	if !strings.Contains(body, "const toVersion = '2.0.0'") {
		t.Error("page should set toVersion")
	}

	// Test invalid format (missing separator)
	req = httptest.NewRequest("GET", "/package/npm/test/compare/invalid", nil)
	w = httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for invalid format, got %d", w.Code)
	}

	// Test with only one dot (should fail)
	req = httptest.NewRequest("GET", "/package/npm/test/compare/1.0.0.2.0.0", nil)
	w = httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for invalid separator, got %d", w.Code)
	}
}

func TestArchiveFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"package.tar.gz", "package.tar.gz"},
		{"d2e2f014ccd6ec9fae8dbe6336a4164346a2a856", "d2e2f014ccd6ec9fae8dbe6336a4164346a2a856.zip"},
		{"file.zip", "file.zip"},
		{"archive.tgz", "archive.tgz"},
		{"noext", "noext.zip"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := archiveFilename(tt.input)
			if got != tt.want {
				t.Errorf("archiveFilename(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestOpenArchiveStripsSingleRootDir(t *testing.T) {
	data := createZipArchive(t, map[string]string{
		"repo-abc123/README.md":   "hello",
		"repo-abc123/src/main.go": "package main",
		"repo-abc123/go.mod":      "module test",
	})
	reader, err := openArchive("test.zip", bytes.NewReader(data), "composer")
	if err != nil {
		t.Fatalf("openArchive failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	files, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Path, "repo-abc123/") {
			t.Errorf("file %q still has root prefix after stripping", f.Path)
		}
	}
}

func TestOpenArchiveMultipleRootDirs(t *testing.T) {
	data := createZipArchive(t, map[string]string{
		"src/main.go":    "package main",
		"docs/README.md": "hello",
	})
	reader, err := openArchive("test.zip", bytes.NewReader(data), "composer")
	if err != nil {
		t.Fatalf("openArchive failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	files, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	paths := make(map[string]bool)
	for _, f := range files {
		paths[f.Path] = true
	}
	if !paths["src/main.go"] {
		t.Error("expected src/main.go to remain unchanged")
	}
	if !paths["docs/README.md"] {
		t.Error("expected docs/README.md to remain unchanged")
	}
}

func TestOpenArchiveFlatNoSubdirs(t *testing.T) {
	data := createZipArchive(t, map[string]string{
		"README.md": "hello",
		"main.go":   "package main",
	})
	reader, err := openArchive("test.zip", bytes.NewReader(data), "composer")
	if err != nil {
		t.Fatalf("openArchive failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	files, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	paths := make(map[string]bool)
	for _, f := range files {
		paths[f.Path] = true
	}
	if !paths["README.md"] {
		t.Error("expected README.md at root")
	}
}

func TestOpenArchiveNpmUsesPackagePrefix(t *testing.T) {
	data := createTarGzArchive(t, map[string]string{
		"package/README.md": "hello",
		"package/index.js":  "module.exports = {}",
	})
	reader, err := openArchive("pkg.tgz", bytes.NewReader(data), "npm")
	if err != nil {
		t.Fatalf("openArchive failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	files, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Path, "package/") {
			t.Errorf("file %q still has package/ prefix", f.Path)
		}
	}
}

func TestOpenArchiveExtensionlessFilename(t *testing.T) {
	data := createZipArchive(t, map[string]string{
		"repo-hash/README.md": "hello",
	})
	reader, err := openArchive("d2e2f014ccd6ec9fae8dbe6336a4164346a2a856", bytes.NewReader(data), "composer")
	if err != nil {
		t.Fatalf("openArchive failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	files, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected files in archive")
	}
	for _, f := range files {
		if strings.HasPrefix(f.Path, "repo-hash/") {
			t.Errorf("file %q still has root prefix", f.Path)
		}
	}
}

func createZipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("failed to create zip entry: %v", err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatalf("failed to write zip content: %v", err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatalf("failed to close zip writer: %v", err)
	}
	return buf.Bytes()
}

func createTarGzArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		header := &tar.Header{
			Name: name,
			Size: int64(len(content)),
			Mode: 0644,
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("failed to write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("failed to write tar content: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}
