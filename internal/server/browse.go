package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/git-pkgs/archives"
	"github.com/git-pkgs/archives/diff"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/purl"
	"github.com/go-chi/chi/v5"
)

const contentTypePlainText = "text/plain; charset=utf-8"

// archiveFilename returns a filename suitable for archive format detection.
// Some ecosystems (e.g. composer) store artifacts with bare hash filenames
// that have no extension. This adds .zip when the original has no extension
// and the content is likely a zip archive.
func archiveFilename(filename string) string {
	if path.Ext(filename) == "" {
		return filename + ".zip"
	}
	return filename
}

// detectSingleRootDir returns the single top-level directory name if all files
// in the archive live under one common directory (e.g. GitHub zipballs use
// "repo-hash/"). Returns "" if there's no single root or the archive is flat.
func detectSingleRootDir(reader archives.Reader) string {
	files, err := reader.List()
	if err != nil || len(files) == 0 {
		return ""
	}

	var root string
	for _, f := range files {
		parts := strings.SplitN(f.Path, "/", 2) //nolint:mnd // split into dir + rest
		if len(parts) == 0 {
			continue
		}
		dir := parts[0]
		if root == "" {
			root = dir
		} else if dir != root {
			return ""
		}
	}

	if root == "" {
		return ""
	}
	return root + "/"
}

// openArchive opens a cached artifact as an archive reader, auto-detecting
// and stripping a single top-level directory prefix (like GitHub zipballs).
// For npm, the hardcoded "package/" prefix takes precedence.
func openArchive(filename string, content io.Reader, ecosystem string) (archives.Reader, error) { //nolint:ireturn // wraps multiple archive implementations
	fname := archiveFilename(filename)

	// npm always uses package/ prefix
	if ecosystem == "npm" {
		return archives.OpenWithPrefix(fname, content, "package/")
	}

	// Read content into memory so we can scan then wrap with prefix
	data, err := io.ReadAll(content)
	if err != nil {
		return nil, fmt.Errorf("reading artifact: %w", err)
	}

	// Open once to detect root prefix
	probe, err := archives.Open(fname, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	prefix := detectSingleRootDir(probe)
	_ = probe.Close()

	return archives.OpenWithPrefix(fname, bytes.NewReader(data), prefix)
}

// BrowseListResponse contains the file listing for a directory in an archives.
type BrowseListResponse struct {
	Path  string           `json:"path"`
	Files []BrowseFileInfo `json:"files"`
}

// BrowseFileInfo contains metadata about a file in an archives.
type BrowseFileInfo struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
	ModTime string `json:"mod_time,omitempty"`
}

// handleBrowseList returns a list of files in a directory within an archived package version.
// GET /api/browse/{ecosystem}/{name}/{version}?path=/some/dir
// @Summary List files inside a cached artifact
// @Description Lists files from the first cached artifact for a package version.
// @Tags browse
// @Produce json
// @Param ecosystem path string true "Ecosystem"
// @Param name path string true "Package name"
// @Param version path string true "Version"
// @Param path query string false "Directory path inside the archive"
// @Success 200 {object} BrowseListResponse
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/browse/{ecosystem}/{name}/{version} [get]
// handleBrowsePath dispatches /api/browse/{ecosystem}/* to the appropriate browse handler.
// It resolves namespaced package names by consulting the database.
//
// Supported paths:
//
//	{name}/{version}              -> browse list
//	{name}/{version}/file/{path}  -> browse file
func (s *Server) handleBrowsePath(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	wildcard := chi.URLParam(r, "*")
	segments := splitWildcardPath(wildcard)

	if ecosystem == "" || len(segments) < 2 {
		http.Error(w, "ecosystem, name, and version required", http.StatusBadRequest)
		return
	}

	// Check for /file/ in the path for browse file requests.
	fileIdx := -1
	for i, seg := range segments {
		if seg == "file" && i > 0 {
			fileIdx = i
			break
		}
	}

	if fileIdx >= 0 {
		// Everything before "file" is name+version, everything after is the file path.
		nameVersionSegments := segments[:fileIdx]
		filePath := strings.Join(segments[fileIdx+1:], "/")

		name, rest := resolvePackageName(s.db, ecosystem, nameVersionSegments)
		if name == "" && len(nameVersionSegments) >= 2 {
			name = strings.Join(nameVersionSegments[:len(nameVersionSegments)-1], "/")
			rest = nameVersionSegments[len(nameVersionSegments)-1:]
		}
		if len(rest) != 1 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.browseFile(w, r, ecosystem, name, rest[0], filePath)
		return
	}

	// No /file/ segment: this is a browse list.
	name, rest := resolvePackageName(s.db, ecosystem, segments)
	if name == "" && len(segments) >= 2 {
		name = strings.Join(segments[:len(segments)-1], "/")
		rest = segments[len(segments)-1:]
	}
	if len(rest) != 1 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.browseList(w, r, ecosystem, name, rest[0])
}

// handleComparePath dispatches /api/compare/{ecosystem}/* to the compare handler.
// Supported paths: {name}/{fromVersion}/{toVersion}
func (s *Server) handleComparePath(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	wildcard := chi.URLParam(r, "*")
	segments := splitWildcardPath(wildcard)

	if ecosystem == "" || len(segments) < 3 {
		http.Error(w, "ecosystem, name, fromVersion, and toVersion required", http.StatusBadRequest)
		return
	}

	// The last two segments are fromVersion and toVersion.
	// Everything before that is the package name.
	name := strings.Join(segments[:len(segments)-2], "/")
	fromVersion := segments[len(segments)-2]
	toVersion := segments[len(segments)-1]

	s.compareDiff(w, r, ecosystem, name, fromVersion, toVersion)
}

func (s *Server) browseList(w http.ResponseWriter, r *http.Request, ecosystem, name, version string) {
	dirPath := r.URL.Query().Get("path")

	// Get the artifact for this version
	versionPURL := purl.MakePURLString(ecosystem, name, version)
	artifacts, err := s.db.GetArtifactsByVersionPURL(versionPURL)
	if err != nil {
		http.Error(w, "version not found", http.StatusNotFound)
		return
	}

	if len(artifacts) == 0 {
		http.Error(w, "no artifacts cached", http.StatusNotFound)
		return
	}

	// Find the first cached artifact
	var cachedArtifact *database.Artifact
	for i := range artifacts {
		if artifacts[i].StoragePath.Valid {
			cachedArtifact = &artifacts[i]
			break
		}
	}

	if cachedArtifact == nil {
		http.Error(w, "artifact not cached", http.StatusNotFound)
		return
	}

	// Open the artifact from storage
	artifactReader, err := s.storage.Open(r.Context(), cachedArtifact.StoragePath.String)
	if err != nil {
		s.logger.Error("failed to read artifact from storage", "error", err)
		http.Error(w, "failed to read artifact", http.StatusInternalServerError)
		return
	}
	defer func() { _ = artifactReader.Close() }()

	// Open archive with auto-detected prefix stripping
	archiveReader, err := openArchive(cachedArtifact.Filename, artifactReader, ecosystem)
	if err != nil {
		s.logger.Error("failed to open archive", "error", err, "filename", cachedArtifact.Filename)
		http.Error(w, "failed to open archive", http.StatusInternalServerError)
		return
	}
	defer func() { _ = archiveReader.Close() }()

	// List files in the directory
	files, err := archiveReader.ListDir(dirPath)
	if err != nil {
		s.logger.Error("failed to list directory", "error", err, "path", dirPath)
		http.Error(w, "failed to list directory", http.StatusInternalServerError)
		return
	}

	// Convert to response format
	response := BrowseListResponse{
		Path:  dirPath,
		Files: make([]BrowseFileInfo, len(files)),
	}

	for i, f := range files {
		response.Files[i] = BrowseFileInfo{
			Path:    f.Path,
			Name:    f.Name,
			Size:    f.Size,
			IsDir:   f.IsDir,
			ModTime: f.ModTime.Format("2006-01-02 15:04:05"),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// handleBrowseFile returns the contents of a specific file within an archived package version.
// GET /api/browse/{ecosystem}/{name}/{version}/file/{filepath...}
// @Summary Fetch a file inside a cached artifact
// @Description Streams a single file from the cached artifact. The file path may contain slashes.
// @Tags browse
// @Produce application/octet-stream
// @Param ecosystem path string true "Ecosystem"
// @Param name path string true "Package name"
// @Param version path string true "Version"
// @Param filepath path string true "File path inside the archive"
// @Success 200 {file} file
// @Failure 400 {string} string
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/browse/{ecosystem}/{name}/{version}/file/{filepath} [get]
func (s *Server) browseFile(w http.ResponseWriter, r *http.Request, ecosystem, name, version, filePath string) {
	if filePath == "" {
		http.Error(w, "file path required", http.StatusBadRequest)
		return
	}

	// Get the artifact for this version
	versionPURL := purl.MakePURLString(ecosystem, name, version)
	artifacts, err := s.db.GetArtifactsByVersionPURL(versionPURL)
	if err != nil {
		http.Error(w, "version not found", http.StatusNotFound)
		return
	}

	if len(artifacts) == 0 {
		http.Error(w, "no artifacts cached", http.StatusNotFound)
		return
	}

	// Find the first cached artifact
	var cachedArtifact *database.Artifact
	for i := range artifacts {
		if artifacts[i].StoragePath.Valid {
			cachedArtifact = &artifacts[i]
			break
		}
	}

	if cachedArtifact == nil {
		http.Error(w, "artifact not cached", http.StatusNotFound)
		return
	}

	// Open the artifact from storage
	artifactReader, err := s.storage.Open(r.Context(), cachedArtifact.StoragePath.String)
	if err != nil {
		s.logger.Error("failed to read artifact from storage", "error", err)
		http.Error(w, "failed to read artifact", http.StatusInternalServerError)
		return
	}
	defer func() { _ = artifactReader.Close() }()

	// Open archive with auto-detected prefix stripping
	archiveReader, err := openArchive(cachedArtifact.Filename, artifactReader, ecosystem)
	if err != nil {
		s.logger.Error("failed to open archive", "error", err, "filename", cachedArtifact.Filename)
		http.Error(w, "failed to open archive", http.StatusInternalServerError)
		return
	}
	defer func() { _ = archiveReader.Close() }()

	// Extract the file
	fileReader, err := archiveReader.Extract(filePath)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		s.logger.Error("failed to extract file", "error", err, "path", filePath)
		http.Error(w, "failed to extract file", http.StatusInternalServerError)
		return
	}
	defer func() { _ = fileReader.Close() }()

	// Set content type based on file extension
	contentType := detectContentType(filePath)
	w.Header().Set("Content-Type", contentType)

	// Set filename for download
	_, filename := path.Split(filePath)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))

	// Stream the file
	_, _ = io.Copy(w, fileReader)
}

// detectContentType returns an appropriate content type based on file extension.
func detectContentType(filename string) string {
	ext := strings.ToLower(path.Ext(filename))

	switch ext {
	// Text formats
	case ".txt", ".md", ".markdown":
		return contentTypePlainText
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".xml":
		return "application/xml; charset=utf-8"
	case ".yaml", ".yml":
		return "text/yaml; charset=utf-8"
	case ".toml":
		return "text/toml; charset=utf-8"

	// Programming languages
	case ".go":
		return "text/x-go; charset=utf-8"
	case ".rs":
		return "text/x-rust; charset=utf-8"
	case ".py":
		return "text/x-python; charset=utf-8"
	case ".rb":
		return "text/x-ruby; charset=utf-8"
	case ".java":
		return "text/x-java; charset=utf-8"
	case ".c", ".h":
		return "text/x-c; charset=utf-8"
	case ".cpp", ".cc", ".cxx", ".hpp":
		return "text/x-c++; charset=utf-8"
	case ".ts":
		return "text/typescript; charset=utf-8"
	case ".tsx":
		return "text/tsx; charset=utf-8"
	case ".jsx":
		return "text/jsx; charset=utf-8"
	case ".php":
		return "text/x-php; charset=utf-8"

	// Config files
	case ".conf", ".config", ".ini":
		return contentTypePlainText
	case ".sh", ".bash":
		return "text/x-shellscript; charset=utf-8"
	case ".dockerfile":
		return "text/x-dockerfile; charset=utf-8"

	// Images
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"

	// Archives
	case ".zip", ".tar", ".gz", ".bz2", ".xz":
		return "application/octet-stream"

	default:
		// Try to detect if it looks like text
		if isLikelyText(filename) {
			return contentTypePlainText
		}
		return "application/octet-stream"
	}
}

// isLikelyText checks if a filename suggests it's a text file.
func isLikelyText(filename string) bool {
	base := path.Base(filename)

	// Common text files without extensions
	textFiles := []string{
		"readme", "license", "authors", "contributors",
		"changelog", "changes", "news", "history",
		"install", "makefile", "dockerfile",
		"gemfile", "rakefile", "procfile",
		".gitignore", ".dockerignore", ".npmignore",
	}

	baseLower := strings.ToLower(base)
	for _, tf := range textFiles {
		if baseLower == tf || strings.HasPrefix(baseLower, tf+".") {
			return true
		}
	}

	return false
}

// BrowseSourceData contains data for the browse source page.
type BrowseSourceData struct {
	Ecosystem   string
	PackageName string
	Version     string
}

// handleBrowseSource is now showBrowseSource in server.go, dispatched via handlePackagePath.

// handleCompareDiff compares two versions and returns a diff.
// GET /api/compare/{ecosystem}/{name}/{fromVersion}/{toVersion}
// @Summary Compare two cached versions
// @Description Returns a structured diff for two cached versions.
// @Tags browse
// @Produce json
// @Param ecosystem path string true "Ecosystem"
// @Param name path string true "Package name"
// @Param fromVersion path string true "From version"
// @Param toVersion path string true "To version"
// @Success 200 {object} map[string]any
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/compare/{ecosystem}/{name}/{fromVersion}/{toVersion} [get]
func (s *Server) compareDiff(w http.ResponseWriter, r *http.Request, ecosystem, name, fromVersion, toVersion string) {
	// Get artifacts for both versions
	fromPURL := purl.MakePURLString(ecosystem, name, fromVersion)
	toPURL := purl.MakePURLString(ecosystem, name, toVersion)

	fromArtifacts, err := s.db.GetArtifactsByVersionPURL(fromPURL)
	if err != nil || len(fromArtifacts) == 0 {
		http.Error(w, "from version not found or not cached", http.StatusNotFound)
		return
	}

	toArtifacts, err := s.db.GetArtifactsByVersionPURL(toPURL)
	if err != nil || len(toArtifacts) == 0 {
		http.Error(w, "to version not found or not cached", http.StatusNotFound)
		return
	}

	// Find cached artifacts
	var fromArtifact, toArtifact *database.Artifact
	for i := range fromArtifacts {
		if fromArtifacts[i].StoragePath.Valid {
			fromArtifact = &fromArtifacts[i]
			break
		}
	}
	for i := range toArtifacts {
		if toArtifacts[i].StoragePath.Valid {
			toArtifact = &toArtifacts[i]
			break
		}
	}

	if fromArtifact == nil || toArtifact == nil {
		http.Error(w, "one or both versions not cached", http.StatusNotFound)
		return
	}

	// Open both archives
	fromReader, err := s.storage.Open(r.Context(), fromArtifact.StoragePath.String)
	if err != nil {
		s.logger.Error("failed to open from artifact", "error", err)
		http.Error(w, "failed to read from version", http.StatusInternalServerError)
		return
	}
	defer func() { _ = fromReader.Close() }()

	toReader, err := s.storage.Open(r.Context(), toArtifact.StoragePath.String)
	if err != nil {
		s.logger.Error("failed to open to artifact", "error", err)
		http.Error(w, "failed to read to version", http.StatusInternalServerError)
		return
	}
	defer func() { _ = toReader.Close() }()

	fromArchive, err := openArchive(fromArtifact.Filename, fromReader, ecosystem)
	if err != nil {
		s.logger.Error("failed to open from archive", "error", err)
		http.Error(w, "failed to open from archive", http.StatusInternalServerError)
		return
	}
	defer func() { _ = fromArchive.Close() }()

	toArchive, err := openArchive(toArtifact.Filename, toReader, ecosystem)
	if err != nil {
		s.logger.Error("failed to open to archive", "error", err)
		http.Error(w, "failed to open to archive", http.StatusInternalServerError)
		return
	}
	defer func() { _ = toArchive.Close() }()

	// Generate diff
	result, err := diff.Compare(fromArchive, toArchive)
	if err != nil {
		s.logger.Error("failed to generate diff", "error", err)
		http.Error(w, "failed to generate diff", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// ComparePageData contains data for the version comparison page.
type ComparePageData struct {
	Ecosystem   string
	PackageName string
	FromVersion string
	ToVersion   string
}

// handleComparePage is now showComparePage in server.go, dispatched via handlePackagePath.
