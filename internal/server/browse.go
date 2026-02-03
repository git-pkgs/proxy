package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/git-pkgs/proxy/internal/archive"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/diff"
	"github.com/go-chi/chi/v5"
)

// getStripPrefix returns the path prefix to strip for a given ecosystem.
// npm packages wrap content in a "package/" directory.
func getStripPrefix(ecosystem string) string {
	switch ecosystem {
	case "npm":
		return "package/"
	default:
		return ""
	}
}

// BrowseListResponse contains the file listing for a directory in an archive.
type BrowseListResponse struct {
	Path  string           `json:"path"`
	Files []BrowseFileInfo `json:"files"`
}

// BrowseFileInfo contains metadata about a file in an archive.
type BrowseFileInfo struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
	ModTime string `json:"mod_time,omitempty"`
}

// handleBrowseList returns a list of files in a directory within an archived package version.
// GET /api/browse/{ecosystem}/{name}/{version}?path=/some/dir
func (s *Server) handleBrowseList(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	name := chi.URLParam(r, "name")
	version := chi.URLParam(r, "version")
	dirPath := r.URL.Query().Get("path")

	// Get the artifact for this version
	purl := fmt.Sprintf("pkg:%s/%s@%s", ecosystem, name, version)
	artifacts, err := s.db.GetArtifactsByVersionPURL(purl)
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
	defer artifactReader.Close()

	// Open archive with appropriate prefix stripping
	stripPrefix := getStripPrefix(ecosystem)
	archiveReader, err := archive.OpenWithPrefix(cachedArtifact.Filename, artifactReader, stripPrefix)
	if err != nil {
		s.logger.Error("failed to open archive", "error", err, "filename", cachedArtifact.Filename)
		http.Error(w, "failed to open archive", http.StatusInternalServerError)
		return
	}
	defer archiveReader.Close()

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
	json.NewEncoder(w).Encode(response)
}

// handleBrowseFile returns the contents of a specific file within an archived package version.
// GET /api/browse/{ecosystem}/{name}/{version}/file/{filepath...}
func (s *Server) handleBrowseFile(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	name := chi.URLParam(r, "name")
	version := chi.URLParam(r, "version")

	// Get the wildcard path
	filePath := chi.URLParam(r, "*")
	if filePath == "" {
		http.Error(w, "file path required", http.StatusBadRequest)
		return
	}

	// Get the artifact for this version
	purl := fmt.Sprintf("pkg:%s/%s@%s", ecosystem, name, version)
	artifacts, err := s.db.GetArtifactsByVersionPURL(purl)
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
	defer artifactReader.Close()

	// Open archive with appropriate prefix stripping
	stripPrefix := getStripPrefix(ecosystem)
	archiveReader, err := archive.OpenWithPrefix(cachedArtifact.Filename, artifactReader, stripPrefix)
	if err != nil {
		s.logger.Error("failed to open archive", "error", err, "filename", cachedArtifact.Filename)
		http.Error(w, "failed to open archive", http.StatusInternalServerError)
		return
	}
	defer archiveReader.Close()

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
	defer fileReader.Close()

	// Set content type based on file extension
	contentType := detectContentType(filePath)
	w.Header().Set("Content-Type", contentType)

	// Set filename for download
	_, filename := path.Split(filePath)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))

	// Stream the file
	io.Copy(w, fileReader)
}

// detectContentType returns an appropriate content type based on file extension.
func detectContentType(filename string) string {
	ext := strings.ToLower(path.Ext(filename))

	switch ext {
	// Text formats
	case ".txt", ".md", ".markdown":
		return "text/plain; charset=utf-8"
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
		return "text/plain; charset=utf-8"
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
			return "text/plain; charset=utf-8"
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

// handleBrowseSource renders the source code browser UI.
// GET /package/{ecosystem}/{name}/{version}/browse
func (s *Server) handleBrowseSource(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	name := chi.URLParam(r, "name")
	version := chi.URLParam(r, "version")

	data := BrowseSourceData{
		Ecosystem:   ecosystem,
		PackageName: name,
		Version:     version,
	}

	if err := s.templates.Render(w, "browse_source", data); err != nil {
		s.logger.Error("failed to render browse source page", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// handleCompareDiff compares two versions and returns a diff.
// GET /api/compare/{ecosystem}/{name}/{fromVersion}/{toVersion}
func (s *Server) handleCompareDiff(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	name := chi.URLParam(r, "name")
	fromVersion := chi.URLParam(r, "fromVersion")
	toVersion := chi.URLParam(r, "toVersion")

	// Get artifacts for both versions
	fromPURL := fmt.Sprintf("pkg:%s/%s@%s", ecosystem, name, fromVersion)
	toPURL := fmt.Sprintf("pkg:%s/%s@%s", ecosystem, name, toVersion)

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
	defer fromReader.Close()

	toReader, err := s.storage.Open(r.Context(), toArtifact.StoragePath.String)
	if err != nil {
		s.logger.Error("failed to open to artifact", "error", err)
		http.Error(w, "failed to read to version", http.StatusInternalServerError)
		return
	}
	defer toReader.Close()

	stripPrefix := getStripPrefix(ecosystem)

	fromArchive, err := archive.OpenWithPrefix(fromArtifact.Filename, fromReader, stripPrefix)
	if err != nil {
		s.logger.Error("failed to open from archive", "error", err)
		http.Error(w, "failed to open from archive", http.StatusInternalServerError)
		return
	}
	defer fromArchive.Close()

	toArchive, err := archive.OpenWithPrefix(toArtifact.Filename, toReader, stripPrefix)
	if err != nil {
		s.logger.Error("failed to open to archive", "error", err)
		http.Error(w, "failed to open to archive", http.StatusInternalServerError)
		return
	}
	defer toArchive.Close()

	// Generate diff
	result, err := diff.Compare(fromArchive, toArchive)
	if err != nil {
		s.logger.Error("failed to generate diff", "error", err)
		http.Error(w, "failed to generate diff", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// ComparePageData contains data for the version comparison page.
type ComparePageData struct {
	Ecosystem   string
	PackageName string
	FromVersion string
	ToVersion   string
}

// handleComparePage renders the version comparison UI.
// GET /package/{ecosystem}/{name}/compare/{versions}
// where {versions} is in format "fromVersion...toVersion"
func (s *Server) handleComparePage(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	name := chi.URLParam(r, "name")
	versions := chi.URLParam(r, "versions")

	// Parse versions (format: "1.0.0...2.0.0")
	parts := strings.Split(versions, "...")
	if len(parts) != 2 {
		http.Error(w, "invalid version format, use: version1...version2", http.StatusBadRequest)
		return
	}

	fromVersion := parts[0]
	toVersion := parts[1]

	data := ComparePageData{
		Ecosystem:   ecosystem,
		PackageName: name,
		FromVersion: fromVersion,
		ToVersion:   toVersion,
	}

	if err := s.templates.Render(w, "compare_versions", data); err != nil {
		s.logger.Error("failed to render compare page", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
