// Package diff provides utilities for comparing package versions.
package diff

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/git-pkgs/archives"
)

// FileDiff represents the diff for a single file.
type FileDiff struct {
	Path      string `json:"path"`
	Type      string `json:"type"` // "modified", "added", "deleted", "renamed"
	OldPath   string `json:"old_path,omitempty"`
	Diff      string `json:"diff,omitempty"`
	IsBinary  bool   `json:"is_binary,omitempty"`
	LinesAdded int   `json:"lines_added"`
	LinesDeleted int `json:"lines_deleted"`
}

// CompareResult contains the complete comparison between two versions.
type CompareResult struct {
	Files        []FileDiff `json:"files"`
	TotalAdded   int        `json:"total_added"`
	TotalDeleted int        `json:"total_deleted"`
	FilesChanged int        `json:"files_changed"`
	FilesAdded   int        `json:"files_added"`
	FilesDeleted int        `json:"files_deleted"`
}

// Compare generates a diff between two archive readers.
func Compare(oldReader, newReader archives.Reader) (*CompareResult, error) {
	// Get file listings
	oldFiles, err := oldReader.List()
	if err != nil {
		return nil, fmt.Errorf("listing old archive: %w", err)
	}

	newFiles, err := newReader.List()
	if err != nil {
		return nil, fmt.Errorf("listing new archive: %w", err)
	}

	// Create maps for quick lookup
	oldMap := make(map[string]archives.FileInfo)
	newMap := make(map[string]archives.FileInfo)

	for _, f := range oldFiles {
		if !f.IsDir {
			oldMap[f.Path] = f
		}
	}

	for _, f := range newFiles {
		if !f.IsDir {
			newMap[f.Path] = f
		}
	}

	result := &CompareResult{
		Files: []FileDiff{},
	}

	// Find all unique paths
	allPaths := make(map[string]bool)
	for path := range oldMap {
		allPaths[path] = true
	}
	for path := range newMap {
		allPaths[path] = true
	}

	// Convert to sorted slice
	paths := make([]string, 0, len(allPaths))
	for path := range allPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	// Compare each file
	for _, path := range paths {
		oldExists := oldMap[path]
		newExists := newMap[path]

		var fileDiff FileDiff

		if oldExists.Path != "" && newExists.Path == "" {
			// File was deleted
			fileDiff = FileDiff{
				Path: path,
				Type: "deleted",
			}
			result.FilesDeleted++
		} else if oldExists.Path == "" && newExists.Path != "" {
			// File was added
			fileDiff = FileDiff{
				Path: path,
				Type: "added",
			}
			result.FilesAdded++

			// Try to get content for added files
			if content, err := readFileContent(newReader, path); err == nil {
				if isBinary(content) {
					fileDiff.IsBinary = true
				} else {
					fileDiff.Diff = generateAddedDiff(path, content)
					fileDiff.LinesAdded = countLines(content)
				}
			}
		} else {
			// File exists in both - check if modified
			oldContent, err1 := readFileContent(oldReader, path)
			newContent, err2 := readFileContent(newReader, path)

			if err1 != nil || err2 != nil {
				continue // Skip files we can't read
			}

			if bytes.Equal(oldContent, newContent) {
				continue // No change
			}

			fileDiff = FileDiff{
				Path: path,
				Type: "modified",
			}
			result.FilesChanged++

			if isBinary(oldContent) || isBinary(newContent) {
				fileDiff.IsBinary = true
			} else {
				diffText, added, deleted := generateUnifiedDiff(path, oldContent, newContent)
				fileDiff.Diff = diffText
				fileDiff.LinesAdded = added
				fileDiff.LinesDeleted = deleted
				result.TotalAdded += added
				result.TotalDeleted += deleted
			}
		}

		result.Files = append(result.Files, fileDiff)
	}

	return result, nil
}

// readFileContent reads a file's content from an archive reader.
func readFileContent(reader archives.Reader, path string) ([]byte, error) {
	rc, err := reader.Extract(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	return io.ReadAll(rc)
}

// isBinary checks if content appears to be binary.
func isBinary(content []byte) bool {
	if len(content) == 0 {
		return false
	}

	// Check first 8KB for null bytes
	checkLen := len(content)
	if checkLen > 8192 {
		checkLen = 8192
	}

	for i := 0; i < checkLen; i++ {
		if content[i] == 0 {
			return true
		}
	}

	return false
}

// generateUnifiedDiff generates a unified diff between two file contents.
// Uses line-based diffing for proper unified diff output.
func generateUnifiedDiff(path string, oldContent, newContent []byte) (string, int, int) {
	return generateSimpleDiff(path, oldContent, newContent)
}

// generateSimpleDiff generates a line-based unified diff.
func generateSimpleDiff(path string, oldContent, newContent []byte) (string, int, int) {
	oldLines := strings.Split(string(oldContent), "\n")
	newLines := strings.Split(string(newContent), "\n")

	// Simple line-by-line comparison (can be improved with Myers algorithm)
	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("--- a/%s\n", path))
	buf.WriteString(fmt.Sprintf("+++ b/%s\n", path))

	linesAdded := 0
	linesDeleted := 0

	// Find common prefix
	commonPrefix := 0
	maxCommon := len(oldLines)
	if len(newLines) < maxCommon {
		maxCommon = len(newLines)
	}
	for commonPrefix < maxCommon && oldLines[commonPrefix] == newLines[commonPrefix] {
		commonPrefix++
	}

	// Find common suffix
	commonSuffix := 0
	oldEnd := len(oldLines) - 1
	newEnd := len(newLines) - 1
	for commonSuffix < maxCommon-commonPrefix &&
	      oldEnd-commonSuffix >= commonPrefix &&
	      newEnd-commonSuffix >= commonPrefix &&
	      oldLines[oldEnd-commonSuffix] == newLines[newEnd-commonSuffix] {
		commonSuffix++
	}

	// Calculate range
	oldStart := commonPrefix
	oldCount := len(oldLines) - commonPrefix - commonSuffix
	newStart := commonPrefix
	newCount := len(newLines) - commonPrefix - commonSuffix

	if oldCount == 0 && newCount == 0 {
		return "", 0, 0
	}

	// Context lines
	contextBefore := 3
	contextAfter := 3

	hunkOldStart := oldStart - contextBefore
	if hunkOldStart < 0 {
		hunkOldStart = 0
	}

	hunkNewStart := newStart - contextBefore
	if hunkNewStart < 0 {
		hunkNewStart = 0
	}

	// Build hunk
	var hunk strings.Builder

	// Context before
	for i := hunkOldStart; i < oldStart && i < len(oldLines); i++ {
		hunk.WriteString(" " + oldLines[i] + "\n")
	}

	// Deleted lines
	for i := oldStart; i < oldStart+oldCount && i < len(oldLines); i++ {
		hunk.WriteString("-" + oldLines[i] + "\n")
		linesDeleted++
	}

	// Added lines
	for i := newStart; i < newStart+newCount && i < len(newLines); i++ {
		hunk.WriteString("+" + newLines[i] + "\n")
		linesAdded++
	}

	// Context after
	afterStart := oldStart + oldCount
	for i := 0; i < contextAfter && afterStart+i < len(oldLines); i++ {
		hunk.WriteString(" " + oldLines[afterStart+i] + "\n")
	}

	// Calculate hunk size
	hunkOldCount := (oldStart - hunkOldStart) + oldCount + contextAfter
	hunkNewCount := (newStart - hunkNewStart) + newCount + contextAfter

	// Write hunk header
	buf.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", hunkOldStart+1, hunkOldCount, hunkNewStart+1, hunkNewCount))
	buf.WriteString(hunk.String())

	return buf.String(), linesAdded, linesDeleted
}

// generateAddedDiff generates a diff for a newly added file.
func generateAddedDiff(path string, content []byte) string {
	var buf strings.Builder
	buf.WriteString("--- /dev/null\n")
	buf.WriteString(fmt.Sprintf("+++ b/%s\n", path))

	lines := bytes.Split(content, []byte("\n"))
	buf.WriteString(fmt.Sprintf("@@ -0,0 +1,%d @@\n", len(lines)))

	for _, line := range lines {
		buf.WriteString("+" + string(line) + "\n")
	}

	return buf.String()
}

// countLines counts the number of lines in content.
func countLines(content []byte) int {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	count := 0
	for scanner.Scan() {
		count++
	}
	return count
}
