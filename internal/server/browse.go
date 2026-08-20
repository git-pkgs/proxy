package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/git-pkgs/archives"
	"github.com/git-pkgs/archives/diff"
	"github.com/git-pkgs/magic"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/go-chi/chi/v5"
)

const (
	contentTypePlainText = "text/plain; charset=utf-8"
	browseSniffSize      = 512
)

// maxBrowseArchiveSize caps how much data openArchive will buffer for
// prefix detection. Artifacts larger than this are rejected to prevent
// memory exhaustion from a single request.
const maxBrowseArchiveSize = 512 << 20 // 512 MB

// firstBrowsableArtifact returns the first cached artifact that can be opened as
// an archive, or nil if the version has none.
//
// A version's artifact list is not all archives: a PEP 658 core-metadata sidecar
// resolves to the same name and version as the distribution it describes, so it
// is cached under that version too. Sidecars are plain text, and because '-'
// sorts before '.' one can even precede the real distribution in the
// filename-ordered list, so selecting blindly would hand openArchive a file it
// cannot parse.
func firstBrowsableArtifact(artifacts []database.Artifact) *database.Artifact {
	for i := range artifacts {
		if artifacts[i].StoragePath.Valid && !isMetadataSidecar(artifacts[i].Filename) {
			return &artifacts[i]
		}
	}

	return nil
}

// isMetadataSidecar reports whether filename is a core-metadata sidecar rather
// than a distribution archive.
func isMetadataSidecar(filename string) bool {
	return strings.HasSuffix(filename, handler.PyPIMetadataSuffix)
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
	limited := io.LimitReader(content, maxBrowseArchiveSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("reading artifact: %w", err)
	}
	if int64(len(data)) > maxBrowseArchiveSize {
		return nil, fmt.Errorf("artifact too large for browsing (%d bytes)", len(data))
	}

	if ecosystem == "npm" {
		return archives.OpenBytesWithPrefix(filename, data, "package/")
	}

	probe, err := archives.OpenBytes(filename, data)
	if err != nil {
		return nil, err
	}
	prefix := detectSingleRootDir(probe)
	_ = probe.Close()

	return archives.OpenBytesWithPrefix(filename, data, prefix)
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
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /ui/api/browse/{ecosystem}/{name}/{version} [get]
// handleBrowsePath dispatches /api/browse/{ecosystem}/* to the appropriate browse handler.
// It resolves namespaced package names by consulting the database.
//
// Supported paths:
//
//	{name}/{version}              -> browse list
//	{name}/{version}/file/{path}  -> browse file
func (s *Server) handleBrowsePath(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	segments, err := packagePathSegments(r)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	if ecosystem == "" || len(segments) < 2 {
		badRequest(w, "ecosystem, name, and version required")
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
			notFound(w, "not found")
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
		notFound(w, "not found")
		return
	}
	s.browseList(w, r, ecosystem, name, rest[0])
}

// handleComparePath dispatches /api/compare/{ecosystem}/* to the compare handler.
// Supported paths: {name}/{fromVersion}/{toVersion}
func (s *Server) handleComparePath(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	segments, err := packagePathSegments(r)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	if ecosystem == "" || len(segments) < 3 {
		badRequest(w, "ecosystem, name, fromVersion, and toVersion required")
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
	versionPURL := s.cachedVersionPURL(ecosystem, name, version)
	artifacts, err := s.db.GetArtifactsByVersionPURL(versionPURL)
	if err != nil {
		notFound(w, "version not found")
		return
	}

	if len(artifacts) == 0 {
		notFound(w, "no artifacts cached")
		return
	}

	cachedArtifact := firstBrowsableArtifact(artifacts)

	if cachedArtifact == nil {
		notFound(w, "artifact not cached")
		return
	}

	// Open the artifact from storage
	artifactReader, err := s.storage.Open(r.Context(), cachedArtifact.StoragePath.String)
	if err != nil {
		s.logger.Error("failed to read artifact from storage", "error", err)
		internalError(w, "failed to read artifact")
		return
	}
	defer func() { _ = artifactReader.Close() }()

	// Open archive with auto-detected prefix stripping
	archiveReader, err := openArchive(cachedArtifact.Filename, artifactReader, ecosystem)
	if err != nil {
		s.logger.Error("failed to open archive", "error", err, "filename", cachedArtifact.Filename)
		internalError(w, "failed to open archive")
		return
	}
	defer func() { _ = archiveReader.Close() }()

	// List files in the directory
	files, err := archiveReader.ListDir(dirPath)
	if err != nil {
		s.logger.Error("failed to list directory", "error", err, "path", dirPath)
		internalError(w, "failed to list directory")
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
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /ui/api/browse/{ecosystem}/{name}/{version}/file/{filepath} [get]
func (s *Server) browseFile(w http.ResponseWriter, r *http.Request, ecosystem, name, version, filePath string) {
	if filePath == "" {
		badRequest(w, "file path required")
		return
	}

	// Get the artifact for this version
	versionPURL := s.cachedVersionPURL(ecosystem, name, version)
	artifacts, err := s.db.GetArtifactsByVersionPURL(versionPURL)
	if err != nil {
		notFound(w, "version not found")
		return
	}

	if len(artifacts) == 0 {
		notFound(w, "no artifacts cached")
		return
	}

	cachedArtifact := firstBrowsableArtifact(artifacts)

	if cachedArtifact == nil {
		notFound(w, "artifact not cached")
		return
	}

	// Open the artifact from storage
	artifactReader, err := s.storage.Open(r.Context(), cachedArtifact.StoragePath.String)
	if err != nil {
		s.logger.Error("failed to read artifact from storage", "error", err)
		internalError(w, "failed to read artifact")
		return
	}
	defer func() { _ = artifactReader.Close() }()

	// Open archive with auto-detected prefix stripping
	archiveReader, err := openArchive(cachedArtifact.Filename, artifactReader, ecosystem)
	if err != nil {
		s.logger.Error("failed to open archive", "error", err, "filename", cachedArtifact.Filename)
		internalError(w, "failed to open archive")
		return
	}
	defer func() { _ = archiveReader.Close() }()

	// Extract the file
	fileReader, err := archiveReader.Extract(filePath)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			notFound(w, "file not found")
			return
		}
		s.logger.Error("failed to extract file", "error", err, "path", filePath)
		internalError(w, "failed to extract file")
		return
	}
	defer func() { _ = fileReader.Close() }()

	contentType, knownPath := detectContentTypeFromPath(filePath)
	var content io.Reader = fileReader
	if !knownPath {
		bufferedFile := bufio.NewReaderSize(fileReader, browseSniffSize)
		prefix, _ := bufferedFile.Peek(browseSniffSize)
		contentType = detectContentTypeFromPrefix(prefix)
		content = bufferedFile
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	_, filename := path.Split(filePath)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))

	// Stream the file
	_, _ = io.Copy(w, content)
}

func detectContentTypeFromPath(filename string) (string, bool) {
	ext := strings.ToLower(path.Ext(filename))

	switch ext {
	// Text formats
	case ".txt", ".md", ".markdown":
		return contentTypePlainText, true
	case ".html", ".htm", ".xhtml":
		return contentTypePlainText, true
	case ".css":
		return "text/css; charset=utf-8", true
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8", true
	case ".json":
		return "application/json; charset=utf-8", true
	case ".xml":
		return "application/xml; charset=utf-8", true
	case ".yaml", ".yml":
		return "text/yaml; charset=utf-8", true
	case ".toml":
		return "text/toml; charset=utf-8", true

	// Programming languages
	case ".go":
		return "text/x-go; charset=utf-8", true
	case ".rs":
		return "text/x-rust; charset=utf-8", true
	case ".py":
		return "text/x-python; charset=utf-8", true
	case ".rb":
		return "text/x-ruby; charset=utf-8", true
	case ".java":
		return "text/x-java; charset=utf-8", true
	case ".c", ".h":
		return "text/x-c; charset=utf-8", true
	case ".cpp", ".cc", ".cxx", ".hpp":
		return "text/x-c++; charset=utf-8", true
	case ".ts":
		return "text/typescript; charset=utf-8", true
	case ".tsx":
		return "text/tsx; charset=utf-8", true
	case ".jsx":
		return "text/jsx; charset=utf-8", true
	case ".php":
		return "text/x-php; charset=utf-8", true

	// Config files
	case ".conf", ".config", ".ini":
		return contentTypePlainText, true
	case ".sh", ".bash":
		return "text/x-shellscript; charset=utf-8", true
	case ".dockerfile":
		return "text/x-dockerfile; charset=utf-8", true

	// Images
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".gif":
		return "image/gif", true
	case ".svg":
		return contentTypePlainText, true
	case ".ico":
		return "image/x-icon", true

	// Archives
	case ".zip", ".tar", ".gz", ".bz2", ".xz":
		return "application/octet-stream", true

	default:
		if isLikelyText(filename) {
			return contentTypePlainText, true
		}
		return "", false
	}
}

func detectContentTypeFromPrefix(prefix []byte) string {
	result := magic.DetectPrefix(prefix)
	if result.Kind == magic.KindText {
		return contentTypePlainText
	}

	switch result.Format {
	case "png":
		return "image/png"
	case "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "pdf":
		return "application/pdf"
	default:
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
//
// Version is the decoded version, for display. EscapedVersion is the same value
// escaped as a single URL path segment and is what the links and the browse API
// calls must use; see database.Version.EscapedVersion.
type BrowseSourceData struct {
	Layout
	Ecosystem      string
	PackageName    string
	Version        string
	EscapedVersion string
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
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /ui/api/compare/{ecosystem}/{name}/{fromVersion}/{toVersion} [get]
func (s *Server) compareDiff(w http.ResponseWriter, r *http.Request, ecosystem, name, fromVersion, toVersion string) {
	// Get artifacts for both versions
	fromPURL := s.cachedVersionPURL(ecosystem, name, fromVersion)
	toPURL := s.cachedVersionPURL(ecosystem, name, toVersion)

	fromArtifacts, err := s.db.GetArtifactsByVersionPURL(fromPURL)
	if err != nil || len(fromArtifacts) == 0 {
		notFound(w, "from version not found or not cached")
		return
	}

	toArtifacts, err := s.db.GetArtifactsByVersionPURL(toPURL)
	if err != nil || len(toArtifacts) == 0 {
		notFound(w, "to version not found or not cached")
		return
	}

	// Find cached artifacts
	fromArtifact := firstBrowsableArtifact(fromArtifacts)
	toArtifact := firstBrowsableArtifact(toArtifacts)

	if fromArtifact == nil || toArtifact == nil {
		notFound(w, "one or both versions not cached")
		return
	}

	// Open both archives
	fromReader, err := s.storage.Open(r.Context(), fromArtifact.StoragePath.String)
	if err != nil {
		s.logger.Error("failed to open from artifact", "error", err)
		internalError(w, "failed to read from version")
		return
	}
	defer func() { _ = fromReader.Close() }()

	toReader, err := s.storage.Open(r.Context(), toArtifact.StoragePath.String)
	if err != nil {
		s.logger.Error("failed to open to artifact", "error", err)
		internalError(w, "failed to read to version")
		return
	}
	defer func() { _ = toReader.Close() }()

	fromArchive, err := openArchive(fromArtifact.Filename, fromReader, ecosystem)
	if err != nil {
		s.logger.Error("failed to open from archive", "error", err)
		internalError(w, "failed to open from archive")
		return
	}
	defer func() { _ = fromArchive.Close() }()

	toArchive, err := openArchive(toArtifact.Filename, toReader, ecosystem)
	if err != nil {
		s.logger.Error("failed to open to archive", "error", err)
		internalError(w, "failed to open to archive")
		return
	}
	defer func() { _ = toArchive.Close() }()

	// Generate diff
	result, err := diff.Compare(fromArchive, toArchive)
	if err != nil {
		s.logger.Error("failed to generate diff", "error", err)
		internalError(w, "failed to generate diff")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// ComparePageData contains data for the version comparison page.
//
// FromVersion and ToVersion are decoded, for display; the Escaped variants are
// the path-segment form used to build the compare API URL.
type ComparePageData struct {
	Layout
	Ecosystem          string
	PackageName        string
	FromVersion        string
	ToVersion          string
	EscapedFromVersion string
	EscapedToVersion   string
}

// handleComparePage is now showComparePage in server.go, dispatched via handlePackagePath.
