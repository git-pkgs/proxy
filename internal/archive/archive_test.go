package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		filename string
		expected string
	}{
		{"package.zip", "zip"},
		{"package.jar", "zip"},
		{"package.whl", "zip"},
		{"package.nupkg", "zip"},
		{"package.tar", "tar"},
		{"package.tar.gz", "tar.gz"},
		{"package.tgz", "tgz"},
		{"package.tar.bz2", "tar.bz2"},
		{"package.tar.xz", "tar.xz"},
		{"package.gem", "gem"},
		{"unknown.txt", ""},
		{"Package.ZIP", "zip"}, // Case insensitive
		{"package.TAR.GZ", "tar.gz"},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := detectFormat(tt.filename)
			if got != tt.expected {
				t.Errorf("detectFormat(%q) = %q, want %q", tt.filename, got, tt.expected)
			}
		})
	}
}

func TestNormalizeDir(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"/", ""},
		{"dir", "dir/"},
		{"dir/", "dir/"},
		{"/dir/", "dir/"},
		{"  dir  ", "dir/"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeDir(tt.input)
			if got != tt.expected {
				t.Errorf("normalizeDir(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestIsInDir(t *testing.T) {
	tests := []struct {
		filePath string
		dirPath  string
		expected bool
	}{
		{"file.txt", "", true},
		{"dir/file.txt", "", false},
		{"dir/file.txt", "dir", true},
		{"dir/subdir/file.txt", "dir", false},
		{"dir/subdir/file.txt", "dir/subdir", true},
		{"other/file.txt", "dir", false},
		{"dir/", "", true}, // dir entry is in root
	}

	for _, tt := range tests {
		t.Run(tt.filePath+"_in_"+tt.dirPath, func(t *testing.T) {
			got := isInDir(tt.filePath, tt.dirPath)
			if got != tt.expected {
				t.Errorf("isInDir(%q, %q) = %v, want %v", tt.filePath, tt.dirPath, got, tt.expected)
			}
		})
	}
}

func TestExtractName(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"file.txt", "file.txt"},
		{"dir/file.txt", "file.txt"},
		{"dir/subdir/file.txt", "file.txt"},
		{"dir/", "dir"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := extractName(tt.path)
			if got != tt.expected {
				t.Errorf("extractName(%q) = %q, want %q", tt.path, got, tt.expected)
			}
		})
	}
}

// createTestZip creates a zip archive in memory with test files
func createTestZip() []byte {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	files := []struct {
		name    string
		content string
	}{
		{"README.md", "# Test Package"},
		{"src/main.go", "package main"},
		{"src/util/helper.go", "package util"},
		{"docs/guide.md", "# Guide"},
	}

	for _, file := range files {
		f, _ := w.Create(file.name)
		f.Write([]byte(file.content))
	}

	w.Close()
	return buf.Bytes()
}

func TestZipReader(t *testing.T) {
	data := createTestZip()
	reader, err := openZip(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("openZip failed: %v", err)
	}
	defer reader.Close()

	// Test List
	files, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(files) != 4 {
		t.Errorf("List returned %d files, want 4", len(files))
	}

	// Test ListDir root
	rootFiles, err := reader.ListDir("")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(rootFiles) < 2 {
		t.Errorf("ListDir root returned %d items, want at least 2", len(rootFiles))
	}

	// Test Extract
	rc, err := reader.Extract("README.md")
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	defer rc.Close()

	content, _ := io.ReadAll(rc)
	if string(content) != "# Test Package" {
		t.Errorf("Extract content = %q, want %q", string(content), "# Test Package")
	}

	// Test Extract non-existent file
	_, err = reader.Extract("nonexistent.txt")
	if err == nil {
		t.Error("Extract non-existent file should fail")
	}
}

// createTestTarGz creates a tar.gz archive in memory with test files
func createTestTarGz() []byte {
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	files := []struct {
		name    string
		content string
	}{
		{"package.json", `{"name": "test"}`},
		{"index.js", "console.log('hello');"},
		{"lib/util.js", "module.exports = {};"},
	}

	for _, file := range files {
		header := &tar.Header{
			Name:    file.name,
			Size:    int64(len(file.content)),
			Mode:    0644,
			ModTime: time.Now(),
		}
		tw.WriteHeader(header)
		tw.Write([]byte(file.content))
	}

	tw.Close()
	gw.Close()
	return buf.Bytes()
}

func TestTarReader(t *testing.T) {
	data := createTestTarGz()
	reader, err := openTar(bytes.NewReader(data), "gzip")
	if err != nil {
		t.Fatalf("openTar failed: %v", err)
	}
	defer reader.Close()

	// Test List
	files, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(files) != 3 {
		t.Errorf("List returned %d files, want 3", len(files))
	}

	// Test ListDir
	rootFiles, err := reader.ListDir("")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(rootFiles) < 2 {
		t.Errorf("ListDir root returned %d items, want at least 2", len(rootFiles))
	}

	// Test Extract
	rc, err := reader.Extract("package.json")
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	defer rc.Close()

	content, _ := io.ReadAll(rc)
	if !strings.Contains(string(content), "test") {
		t.Errorf("Extract content doesn't contain expected data")
	}
}

func TestOpen(t *testing.T) {
	// Test ZIP
	zipData := createTestZip()
	reader, err := Open("test.zip", bytes.NewReader(zipData))
	if err != nil {
		t.Fatalf("Open zip failed: %v", err)
	}
	reader.Close()

	// Test TAR.GZ
	tgzData := createTestTarGz()
	reader, err = Open("test.tar.gz", bytes.NewReader(tgzData))
	if err != nil {
		t.Fatalf("Open tar.gz failed: %v", err)
	}
	reader.Close()

	// Test unsupported format
	_, err = Open("test.unknown", bytes.NewReader([]byte("data")))
	if err == nil {
		t.Error("Open with unsupported format should fail")
	}
}

func TestZipListDir(t *testing.T) {
	data := createTestZip()
	reader, err := openZip(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("openZip failed: %v", err)
	}
	defer reader.Close()

	// Test listing src directory
	srcFiles, err := reader.ListDir("src")
	if err != nil {
		t.Fatalf("ListDir src failed: %v", err)
	}

	// Should have main.go and util/ subdirectory
	if len(srcFiles) != 2 {
		t.Errorf("ListDir src returned %d items, want 2", len(srcFiles))
	}

	// Check that we have both a file and a directory
	hasFile := false
	hasDir := false
	for _, f := range srcFiles {
		if f.Name == "main.go" && !f.IsDir {
			hasFile = true
		}
		if f.Name == "util" && f.IsDir {
			hasDir = true
		}
	}

	if !hasFile {
		t.Error("ListDir src should include main.go file")
	}
	if !hasDir {
		t.Error("ListDir src should include util directory")
	}
}

func TestOpenWithPrefix(t *testing.T) {
	// Create archive with package/ prefix (like npm)
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	files := []struct {
		name    string
		content string
	}{
		{"package/README.md", "# Test"},
		{"package/index.js", "console.log('test');"},
		{"package/lib/util.js", "module.exports = {};"},
	}

	for _, file := range files {
		f, _ := w.Create(file.name)
		f.Write([]byte(file.content))
	}
	w.Close()

	// Open with prefix stripping
	reader, err := OpenWithPrefix("test.zip", bytes.NewReader(buf.Bytes()), "package/")
	if err != nil {
		t.Fatalf("OpenWithPrefix failed: %v", err)
	}
	defer reader.Close()

	// List files - should not include package/ prefix
	files2, err := reader.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	// Check that package/ prefix is stripped
	for _, f := range files2 {
		if strings.HasPrefix(f.Path, "package/") {
			t.Errorf("path %q still has package/ prefix", f.Path)
		}
	}

	// Should have 3 files without the prefix
	if len(files2) != 3 {
		t.Errorf("List returned %d files, want 3", len(files2))
	}

	// Test ListDir root
	rootFiles, err := reader.ListDir("")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}

	// Should see README.md and index.js in root, plus lib/ directory
	if len(rootFiles) < 2 {
		t.Errorf("ListDir root returned %d items, want at least 2", len(rootFiles))
	}

	// Test Extract
	rc, err := reader.Extract("README.md")
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	defer rc.Close()

	content, _ := io.ReadAll(rc)
	if string(content) != "# Test" {
		t.Errorf("Extract content = %q, want %q", string(content), "# Test")
	}
}

func TestGetStripPrefixNpm(t *testing.T) {
	// Create npm-style archive
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	files := map[string]string{
		"package/package.json": `{"name": "test"}`,
		"package/index.js":     "console.log('test');",
	}

	for path, content := range files {
		header := &tar.Header{
			Name: path,
			Size: int64(len(content)),
			Mode: 0644,
		}
		tw.WriteHeader(header)
		tw.Write([]byte(content))
	}
	tw.Close()
	gw.Close()

	// Open with npm prefix stripping
	reader, err := OpenWithPrefix("test.tgz", bytes.NewReader(buf.Bytes()), "package/")
	if err != nil {
		t.Fatalf("OpenWithPrefix failed: %v", err)
	}
	defer reader.Close()

	// List root - should see package.json and index.js directly
	rootFiles, err := reader.ListDir("")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}

	if len(rootFiles) != 2 {
		t.Errorf("expected 2 files in root, got %d", len(rootFiles))
	}

	// Verify files are at root level
	for _, f := range rootFiles {
		if strings.Contains(f.Path, "/") {
			t.Errorf("file %q should be at root level after prefix stripping", f.Path)
		}
	}
}
