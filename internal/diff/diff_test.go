package diff

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/archive"
)

func createTestArchiveWithFiles(files map[string]string) []byte {
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

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
	return buf.Bytes()
}

func TestCompare(t *testing.T) {
	// Create two test archives
	oldFiles := map[string]string{
		"README.md":    "# Old Version\n",
		"src/main.go":  "package main\n\nfunc main() {\n\tprintln(\"old\")\n}\n",
		"deleted.txt":  "this will be deleted",
	}

	newFiles := map[string]string{
		"README.md":    "# New Version\n\nWith more content\n",
		"src/main.go":  "package main\n\nfunc main() {\n\tprintln(\"new\")\n}\n",
		"added.txt":    "this is new",
	}

	oldArchive, err := archive.Open("old.tar.gz", bytes.NewReader(createTestArchiveWithFiles(oldFiles)))
	if err != nil {
		t.Fatalf("failed to open old archive: %v", err)
	}
	defer oldArchive.Close()

	newArchive, err := archive.Open("new.tar.gz", bytes.NewReader(createTestArchiveWithFiles(newFiles)))
	if err != nil {
		t.Fatalf("failed to open new archive: %v", err)
	}
	defer newArchive.Close()

	// Compare
	result, err := Compare(oldArchive, newArchive)
	if err != nil {
		t.Fatalf("Compare failed: %v", err)
	}

	// Check counts
	if result.FilesChanged != 2 {
		t.Errorf("FilesChanged = %d, want 2", result.FilesChanged)
	}

	if result.FilesAdded != 1 {
		t.Errorf("FilesAdded = %d, want 1", result.FilesAdded)
	}

	if result.FilesDeleted != 1 {
		t.Errorf("FilesDeleted = %d, want 1", result.FilesDeleted)
	}

	// Check individual files
	fileMap := make(map[string]FileDiff)
	for _, f := range result.Files {
		fileMap[f.Path] = f
	}

	// Check deleted file
	if f, ok := fileMap["deleted.txt"]; !ok || f.Type != "deleted" {
		t.Error("deleted.txt should be marked as deleted")
	}

	// Check added file
	if f, ok := fileMap["added.txt"]; !ok || f.Type != "added" {
		t.Error("added.txt should be marked as added")
	}

	// Check modified files
	if f, ok := fileMap["README.md"]; !ok || f.Type != "modified" {
		t.Error("README.md should be marked as modified")
	}

	if f, ok := fileMap["src/main.go"]; !ok || f.Type != "modified" {
		t.Error("src/main.go should be marked as modified")
	}
}

func TestIsBinary(t *testing.T) {
	tests := []struct {
		name     string
		content  []byte
		expected bool
	}{
		{"empty", []byte{}, false},
		{"text", []byte("hello world"), false},
		{"binary with null", []byte{0x00, 0x01, 0x02}, true},
		{"text with newlines", []byte("line1\nline2\nline3"), false},
		{"json", []byte(`{"key": "value"}`), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBinary(tt.content)
			if got != tt.expected {
				t.Errorf("isBinary() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestGenerateUnifiedDiff(t *testing.T) {
	oldContent := []byte("line 1\nline 2\nline 3\n")
	newContent := []byte("line 1\nline 2 modified\nline 3\n")

	diff, added, deleted := generateUnifiedDiff("test.txt", oldContent, newContent)

	// Log the diff for debugging
	t.Logf("Generated diff:\n%s", diff)
	t.Logf("Added: %d, Deleted: %d", added, deleted)

	// The diff library might generate optimized diffs
	// Check that we have some diff output
	if diff == "" {
		t.Error("diff should not be empty")
	}

	if !strings.Contains(diff, "--- a/test.txt") {
		t.Error("diff should contain old file marker")
	}

	if !strings.Contains(diff, "+++ b/test.txt") {
		t.Error("diff should contain new file marker")
	}

	// Check that the diff contains the changed content
	if !strings.Contains(diff, "line 2") {
		t.Error("diff should reference the changed line")
	}
}

func TestGenerateAddedDiff(t *testing.T) {
	content := []byte("new file\nwith content\n")

	diff := generateAddedDiff("new.txt", content)

	if !strings.Contains(diff, "--- /dev/null") {
		t.Error("diff should indicate new file")
	}

	if !strings.Contains(diff, "+++ b/new.txt") {
		t.Error("diff should contain new file path")
	}

	if !strings.Contains(diff, "+new file") {
		t.Error("diff should contain added lines")
	}
}

func TestCountLines(t *testing.T) {
	tests := []struct {
		name     string
		content  []byte
		expected int
	}{
		{"empty", []byte{}, 0},
		{"one line", []byte("hello"), 1},
		{"three lines", []byte("line1\nline2\nline3"), 3},
		{"trailing newline", []byte("line1\nline2\n"), 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countLines(tt.content)
			if got != tt.expected {
				t.Errorf("countLines() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestCompareIdentical(t *testing.T) {
	files := map[string]string{
		"README.md": "# Test\n",
		"main.go":   "package main\n",
	}

	archive1, _ := archive.Open("test1.tar.gz", bytes.NewReader(createTestArchiveWithFiles(files)))
	defer archive1.Close()

	archive2, _ := archive.Open("test2.tar.gz", bytes.NewReader(createTestArchiveWithFiles(files)))
	defer archive2.Close()

	result, err := Compare(archive1, archive2)
	if err != nil {
		t.Fatalf("Compare failed: %v", err)
	}

	if len(result.Files) != 0 {
		t.Errorf("expected no changes, got %d files", len(result.Files))
	}

	if result.FilesChanged != 0 || result.FilesAdded != 0 || result.FilesDeleted != 0 {
		t.Error("expected all counts to be zero for identical archives")
	}
}

func TestCompareBinaryFiles(t *testing.T) {
	oldFiles := map[string]string{
		"image.png": string([]byte{0x89, 0x50, 0x4E, 0x47, 0x00}), // Binary content
	}

	newFiles := map[string]string{
		"image.png": string([]byte{0x89, 0x50, 0x4E, 0x47, 0x01}), // Different binary
	}

	oldArchive, _ := archive.Open("old.tar.gz", bytes.NewReader(createTestArchiveWithFiles(oldFiles)))
	defer oldArchive.Close()

	newArchive, _ := archive.Open("new.tar.gz", bytes.NewReader(createTestArchiveWithFiles(newFiles)))
	defer newArchive.Close()

	result, err := Compare(oldArchive, newArchive)
	if err != nil {
		t.Fatalf("Compare failed: %v", err)
	}

	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(result.Files))
	}

	if !result.Files[0].IsBinary {
		t.Error("file should be marked as binary")
	}

	if result.Files[0].Diff != "" {
		t.Error("binary files should not have diff content")
	}
}
