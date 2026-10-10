package server

import (
	"bufio"
	"encoding/json"
	"errors"
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
	// browsePrefetchSize is how much of a browsed file is read before the
	// response starts. Read errors within it still produce an error status.
	browsePrefetchSize = 64 << 10
)

// maxBrowseArchiveSize caps the compressed artifact size the browse and diff
// endpoints will read. Artifacts larger than this are rejected to prevent
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
	if err != nil {
		return ""
	}

	var root rootDetector
	for _, f := range files {
		if !root.add(f.Path) {
			break
		}
	}
	return root.prefix()
}

// openArchive opens a cached artifact as a random-access archive reader,
// auto-detecting and stripping a single top-level directory prefix (like
// GitHub zipballs). For npm, the hardcoded "package/" prefix takes precedence.
// The whole artifact is buffered, so only the version diff uses it; listing
// and file reads stream through browseArchive instead.
func openArchive(filename string, content io.Reader, ecosystem string) (archives.Reader, error) { //nolint:ireturn // wraps multiple archive implementations
	data, err := readBrowseInput(content, maxBrowseArchiveSize)
	if err != nil {
		return nil, err
	}

	if ecosystem == "npm" {
		return archives.OpenBytesWithPrefix(filename, data, npmPackagePrefix)
	}

	probe, err := archives.OpenBytes(filename, data)
	if err != nil {
		return nil, err
	}
	prefix := detectSingleRootDir(probe)
	_ = probe.Close()

	return archives.OpenBytesWithPrefix(filename, data, prefix)
}

// browseLimitsOrDefault returns the stream limits for listing and file reads.
func (s *Server) browseLimitsOrDefault() archives.StreamOptions {
	if s.browseLimits != (archives.StreamOptions{}) {
		return s.browseLimits
	}
	return defaultBrowseLimits
}

// openBrowseArchive prepares a cached artifact for streaming. content is the
// already open storage reader; later passes reopen the artifact.
func (s *Server) openBrowseArchive(r *http.Request, artifact *database.Artifact, content io.Reader, ecosystem string) (*browseArchive, error) {
	storagePath := artifact.StoragePath.String
	reopen := func() (io.ReadCloser, error) {
		return s.storage.Open(r.Context(), storagePath)
	}
	return newBrowseArchive(artifact.Filename, ecosystem, content, reopen, s.browseLimitsOrDefault())
}

// browseArchiveError reports a failure to read an archive for browsing.
func (s *Server) browseArchiveError(w http.ResponseWriter, err error, filename string) {
	if isBrowseLimitError(err) {
		s.logger.Warn("archive exceeds browse limits", "error", err, "filename", filename)
		internalError(w, "archive exceeds browse limits")
		return
	}
	s.logger.Error("failed to open archive", "error", err, "filename", filename)
	internalError(w, "failed to open archive")
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

	archive, err := s.openBrowseArchive(r, cachedArtifact, artifactReader, ecosystem)
	if err != nil {
		s.browseArchiveError(w, err, cachedArtifact.Filename)
		return
	}

	// List files in the directory, with the root prefix stripped
	files, err := archive.ListDir(dirPath)
	if err != nil {
		s.browseArchiveError(w, err, cachedArtifact.Filename)
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

	archive, err := s.openBrowseArchive(r, cachedArtifact, artifactReader, ecosystem)
	if err != nil {
		s.browseArchiveError(w, err, cachedArtifact.Filename)
		return
	}

	// Find the file and stream it straight from the archive
	fileReader, err := archive.Extract(filePath)
	switch {
	case errors.Is(err, errBrowseNotFound):
		notFound(w, "file not found")
		return
	case errors.Is(err, errBrowseIsDir):
		s.logger.Error("failed to extract file", "error", err, "path", filePath)
		internalError(w, "failed to extract file")
		return
	case err != nil:
		s.browseArchiveError(w, err, cachedArtifact.Filename)
		return
	}
	defer func() { _ = fileReader.Close() }()

	s.writeBrowseFile(w, fileReader, filePath, cachedArtifact.Filename)
}

// writeBrowseFile sends a file streamed from an archive. The start of the file
// is read before any headers, so a truncated or over-limit entry that fails
// there still gets an error status. A failure after the response has started
// aborts it, so the client sees an incomplete download rather than a
// successful one.
func (s *Server) writeBrowseFile(w http.ResponseWriter, file io.Reader, filePath, artifactName string) {
	source := &trackedReader{reader: file}
	content := bufio.NewReaderSize(source, browsePrefetchSize)
	head, err := content.Peek(browsePrefetchSize)
	if err != nil && !errors.Is(err, io.EOF) {
		s.browseReadError(w, err, filePath, artifactName)
		return
	}

	contentType, knownPath := detectContentTypeFromPath(filePath)
	if !knownPath {
		contentType = detectContentTypeFromPrefix(head[:min(len(head), browseSniffSize)])
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	_, filename := path.Split(filePath)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))

	written, err := io.Copy(w, content)
	if err == nil || source.err == nil {
		// A write error without a read error means the client went away.
		return
	}
	s.logger.Error("failed to stream file from archive", "error", source.err,
		"path", filePath, "filename", artifactName, "bytes", written)
	// Headers are already committed: finishing normally would turn a truncated
	// file into a seemingly successful download. Let net/http close the HTTP/1
	// connection or reset the HTTP/2 stream instead.
	panic(http.ErrAbortHandler)
}

// browseReadError reports a failure reading a file before its response starts.
func (s *Server) browseReadError(w http.ResponseWriter, err error, filePath, artifactName string) {
	if isBrowseLimitError(err) {
		s.logger.Warn("archive exceeds browse limits", "error", err, "path", filePath, "filename", artifactName)
		internalError(w, "archive exceeds browse limits")
		return
	}
	s.logger.Error("failed to read file from archive", "error", err, "path", filePath, "filename", artifactName)
	internalError(w, "failed to read file")
}

// trackedReader records the first read error other than io.EOF, so a failed
// copy can be told apart from a failed write to the client.
type trackedReader struct {
	reader io.Reader
	err    error
}

func (r *trackedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
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
