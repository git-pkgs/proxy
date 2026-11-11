package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/git-pkgs/proxy/internal/gitcache"
	"github.com/go-chi/chi/v5"
)

// GitDeleteResponse names the cached Git repository removed and the space
// it took.
type GitDeleteResponse struct {
	Repository string `json:"repository"`
	FreedBytes int64  `json:"freed_bytes"`
}

// handleGitDelete handles DELETE /api/git/{upstream}/{repo}
// @Summary Delete a cached Git repository
// @Description Removes one repository from the git cache, so the next clone fetches it from the upstream again. A repository that a clone, a fetch or a client is using answers 409. Needs cache_delete.
// @Tags cache
// @Produce json
// @Param upstream path string true "Name of the upstream.git entry"
// @Param repo path string true "Repository path under the upstream, with or without .git"
// @Success 200 {object} GitDeleteResponse
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Router /api/git/{upstream}/{repo} [delete]
func (s *Server) handleGitDelete(w http.ResponseWriter, r *http.Request) {
	name, repo, _ := strings.Cut(chi.URLParam(r, "*"), "/")
	repo = strings.TrimSuffix(repo, ".git")
	upstream, ok := s.cfg.Upstream.Git[name]
	if !ok || repo == "" {
		notFound(w, "repository not cached")
		return
	}
	freed, err := s.git.Remove(gitcache.UpstreamDir(name, upstream.URL) + "/" + repo + ".git")
	switch {
	case errors.Is(err, gitcache.ErrNotCached):
		notFound(w, "repository not cached")
		return
	case errors.Is(err, gitcache.ErrInUse):
		writeError(w, http.StatusConflict, ErrCodeConflict, "repository is being cloned, fetched or read; try again")
		return
	case err != nil:
		s.logger.Error("failed to delete cached git repository", "upstream", name, "repository", repo, "error", err)
		internalError(w, "failed to delete cached repository")
		return
	}
	s.logger.Info("deleted cached git repository", "upstream", name, "repository", repo, "freed", freed)
	writeJSON(w, GitDeleteResponse{Repository: name + "/" + repo, FreedBytes: freed})
}
