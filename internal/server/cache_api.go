package server

import (
	"net/http"
	"strings"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/go-chi/chi/v5"
)

// CacheDeleteResponse lists the cached files removed for a version.
type CacheDeleteResponse struct {
	PURL    string   `json:"purl"`
	Deleted []string `json:"deleted"`
}

// handleCacheDelete handles DELETE /api/cache/{ecosystem}/{name}/{version}
// @Summary Delete cached artifacts
// @Description Removes the cached files of name@version, or only the one named by filename, so the next request fetches them from the upstream again. Pushed archives are not touched; delete those through /api/hosted.
// @Tags cache
// @Produce json
// @Param ecosystem path string true "Ecosystem"
// @Param name path string true "Package name as shown in the UI, e.g. com.google.guava:guava for Maven"
// @Param version path string true "Version"
// @Param filename query string false "Delete only this file"
// @Success 200 {object} CacheDeleteResponse
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Router /api/cache/{ecosystem}/{name}/{version} [delete]
func (s *Server) handleCacheDelete(w http.ResponseWriter, r *http.Request) {
	segments, err := packagePathSegments(r)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if len(segments) < minVersionPathSegments {
		badRequest(w, "path must be /api/cache/{ecosystem}/{name}/{version}")
		return
	}
	last := len(segments) - 1
	ecosystem, name, version := chi.URLParam(r, "ecosystem"), strings.Join(segments[:last], "/"), segments[last]
	filename := r.URL.Query().Get("filename")

	versionPURL := s.cachedVersionPURL(ecosystem, name, version)
	if versionPURL == "" {
		notFound(w, "package not found")
		return
	}
	artifacts, err := s.db.GetArtifactsByVersionPURL(versionPURL)
	if err != nil {
		s.logger.Error("failed to list artifacts", "purl", versionPURL, "error", err)
		internalError(w, "failed to list artifacts")
		return
	}

	deleted := []string{}
	for _, art := range artifacts {
		if filename != "" && art.Filename != filename {
			continue
		}
		if !art.StoragePath.Valid {
			continue
		}
		if art.Source == database.SourceLocal {
			if filename != "" {
				writeError(w, http.StatusConflict, ErrCodeConflict, "pushed archives are deleted through /api/hosted")
				return
			}
			continue
		}
		if err := s.db.DiscardArtifact(versionPURL, art.Filename, art.StoragePath.String); err != nil {
			s.logger.Error("failed to delete cached artifact", "purl", versionPURL, "filename", art.Filename, "error", err)
			internalError(w, "failed to delete cached artifact")
			return
		}
		deleted = append(deleted, art.Filename)
	}
	if len(deleted) == 0 {
		notFound(w, "no cached artifact matched")
		return
	}

	s.logger.Info("deleted cached artifacts", "purl", versionPURL, "files", deleted)
	writeJSON(w, CacheDeleteResponse{PURL: versionPURL, Deleted: deleted})
}
