package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/inventory"
	"github.com/git-pkgs/proxy/internal/packageurl"
	"github.com/go-chi/chi/v5"
)

// maxSBOMBytes caps an uploaded SBOM. syft's CycloneDX output for a large
// monorepo is a few MiB.
const maxSBOMBytes = 32 << 20

// maxRefLength caps the ref and branch query parameters of an upload.
const maxRefLength = 255

// sbomPathSuffix ends the upload path, /api/projects/{project}/sbom.
const sbomPathSuffix = "/sbom"

// ProjectsAPIHandler serves /api/projects, where repositories upload the SBOM
// of their default branch so the proxy can show which of them use which
// package versions.
type ProjectsAPIHandler struct {
	db     *database.DB
	logger *slog.Logger
}

// NewProjectsAPIHandler creates the /api/projects handler.
func NewProjectsAPIHandler(db *database.DB, logger *slog.Logger) *ProjectsAPIHandler {
	return &ProjectsAPIHandler{db: db, logger: logger}
}

// ProjectUploadResponse summarizes an uploaded SBOM.
type ProjectUploadResponse struct {
	Project string `json:"project"`
	// Components is the number of package versions recorded.
	Components int `json:"components"`
	// Skipped is the number of SBOM entries not recorded: without a package
	// URL or a version, of a package type the proxy doesn't serve, conda
	// entries without a channel, and the project's own packages.
	Skipped int `json:"skipped"`
}

// ProjectSummary describes a project in the inventory.
type ProjectSummary struct {
	Name       string    `json:"name"`
	Ref        string    `json:"ref,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	UploadedAt time.Time `json:"uploaded_at"`
	// ComponentCount is the number of package versions recorded. It is not
	// set when listing the projects using a package.
	ComponentCount *int64 `json:"component_count,omitempty"`
	// Version is set when listing the projects using a package: the version
	// of it this project uses.
	Version string `json:"version,omitempty"`
}

// ProjectListResponse lists projects.
type ProjectListResponse struct {
	Projects []ProjectSummary `json:"projects"`
}

// ProjectComponent is a package version a project uses.
type ProjectComponent struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	PURL      string `json:"purl"`
}

// ProjectResponse is a project with its components.
type ProjectResponse struct {
	ProjectSummary
	Components []ProjectComponent `json:"components"`
}

// mountProjectAPI registers /api/projects when projects.enabled is set.
func (s *Server) mountProjectAPI(r chi.Router) {
	if s.cfg.Projects.Enabled {
		registerProjectRoutes(r, NewProjectsAPIHandler(s.db, s.logger), s.cfg.Projects.TokenExpanded())
	}
}

// registerProjectRoutes mounts /api/projects. Reading is open like the rest
// of the API; uploading and deleting require the bearer token.
//
// A project name such as "group/sub/repo" is accepted both as path segments,
// /api/projects/group/sub/repo, and URL-encoded as one segment,
// /api/projects/group%2Fsub%2Frepo, which is how GitLab's CI_PROJECT_PATH
// usually ends up when a client escapes it.
func registerProjectRoutes(r chi.Router, h *ProjectsAPIHandler, token string) {
	r.Get("/api/projects", h.HandleList)
	r.Get("/api/projects/*", h.HandleGet)
	r.Group(func(w chi.Router) {
		w.Use(requireBearerToken(token))
		w.Put("/api/projects/*", h.HandleUpload)
		w.Delete("/api/projects/*", h.HandleDelete)
	})
}

// HandleUpload handles PUT /api/projects/{project}/sbom.
// @Summary Upload a project's SBOM
// @Description Replaces the package versions recorded for the project with the ones the SBOM lists. CycloneDX (JSON or XML) and SPDX (JSON or tag-value) are accepted, e.g. the output of `syft dir:. -o cyclonedx-json`. Entries without a package URL or version, of a package type the proxy doesn't serve, and the project's own packages are skipped and counted. The project may be given as path segments or URL-encoded as one segment.
// @Tags projects
// @Accept json,application/xml,plain
// @Produce json
// @Security BearerAuth
// @Param project path string true "Project path, e.g. group/sub/repo"
// @Param ref query string false "Commit the SBOM was generated from"
// @Param branch query string false "Branch the SBOM was generated from"
// @Success 200 {object} ProjectUploadResponse
// @Failure 400 {object} ErrorResponse
// @Failure 401 {object} ErrorResponse
// @Failure 413 {object} ErrorResponse
// @Router /api/projects/{project}/sbom [put]
func (h *ProjectsAPIHandler) HandleUpload(w http.ResponseWriter, r *http.Request) {
	path, err := projectPath(r)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	name, ok := strings.CutSuffix(path, sbomPathSuffix)
	if !ok {
		badRequest(w, "path must be /api/projects/{project}/sbom")
		return
	}
	if err := inventory.ValidateProjectName(name); err != nil {
		badRequest(w, err.Error())
		return
	}
	ref, branch := r.URL.Query().Get("ref"), r.URL.Query().Get("branch")
	if len(ref) > maxRefLength || len(branch) > maxRefLength {
		badRequest(w, "ref and branch must not be longer than 255 characters")
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSBOMBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, ErrCodeBadRequest, "SBOM is larger than 32 MiB")
		return
	case err != nil:
		badRequest(w, "failed to read request body")
		return
	case len(data) == 0:
		badRequest(w, "request body is empty")
		return
	}

	components, skipped, err := inventory.Parse(data)
	if err != nil {
		badRequest(w, "request body is not a CycloneDX or SPDX SBOM: "+err.Error())
		return
	}
	records := make([]database.ProjectComponent, len(components))
	for i, c := range components {
		records[i] = database.ProjectComponent{
			PackagePURL: c.PackagePURL, Ecosystem: c.Ecosystem, Name: c.Name, Version: c.Version,
		}
	}
	if err := h.db.SaveProject(name, ref, branch, time.Now(), records); err != nil {
		h.logger.Error("failed to save project", "project", name, "error", err)
		internalError(w, "failed to save project")
		return
	}
	h.logger.Info("recorded project SBOM", "project", name, "ref", ref,
		"components", len(records), "skipped", skipped)
	writeJSON(w, ProjectUploadResponse{Project: name, Components: len(records), Skipped: skipped})
}

// HandleDelete handles DELETE /api/projects/{project}, and the same under
// /ui/api/projects for the delete button on a project's page.
// @Summary Delete a project
// @Description Removes the project and the package versions recorded for it. The path names the project itself, without the /sbom of the upload path: a path ending in /sbom that names no project is answered with 400.
// @Tags projects
// @Security BearerAuth
// @Param project path string true "Project path, e.g. group/sub/repo"
// @Success 204
// @Failure 400 {object} ErrorResponse
// @Failure 401 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/projects/{project} [delete]
func (h *ProjectsAPIHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	name, ok := validProjectPath(w, r)
	if !ok {
		return
	}
	deleted, err := h.db.DeleteProject(name)
	switch {
	case err != nil:
		h.logger.Error("failed to delete project", "project", name, "error", err)
		internalError(w, "failed to delete project")
	case !deleted && (name == "sbom" || strings.HasSuffix(name, sbomPathSuffix)):
		// Most likely the upload path; a project whose last segment is
		// "sbom" is deleted above when it exists.
		badRequest(w, "project not found; to delete a project, send DELETE /api/projects/{project} without /sbom")
	case !deleted:
		notFound(w, "project not found")
	default:
		h.logger.Info("deleted project", "project", name)
		w.WriteHeader(http.StatusNoContent)
	}
}

// HandleList handles GET /api/projects.
// @Summary List projects
// @Description Lists the projects that uploaded an SBOM. With purl, lists the projects using that package, or only that version of it when the purl has one; each entry then carries the version the project uses.
// @Tags projects
// @Produce json
// @Param purl query string false "Package URL, e.g. pkg:npm/lodash or pkg:npm/lodash@4.17.20"
// @Success 200 {object} ProjectListResponse
// @Failure 400 {object} ErrorResponse
// @Router /api/projects [get]
func (h *ProjectsAPIHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	if query := r.URL.Query().Get("purl"); query != "" {
		h.listUsages(w, query)
		return
	}
	projects, err := h.db.ListProjects()
	if err != nil {
		h.logger.Error("failed to list projects", "error", err)
		internalError(w, "failed to list projects")
		return
	}
	resp := ProjectListResponse{Projects: make([]ProjectSummary, len(projects))}
	for i, p := range projects {
		resp.Projects[i] = projectSummary(p)
	}
	writeJSON(w, resp)
}

func (h *ProjectsAPIHandler) listUsages(w http.ResponseWriter, query string) {
	c, err := inventory.FromPURL(query)
	if err != nil {
		badRequest(w, "invalid purl: "+err.Error())
		return
	}
	usages, err := h.db.GetProjectsUsingPackage(c.PackagePURL, c.Version)
	if err != nil {
		h.logger.Error("failed to look up package usage", "purl", query, "error", err)
		internalError(w, "failed to look up package usage")
		return
	}
	resp := ProjectListResponse{Projects: make([]ProjectSummary, len(usages))}
	for i, u := range usages {
		resp.Projects[i] = ProjectSummary{
			Name:       u.Name,
			Ref:        u.Ref.String,
			Branch:     u.Branch.String,
			UploadedAt: u.UploadedAt,
			Version:    u.Version,
		}
	}
	writeJSON(w, resp)
}

// HandleGet handles GET /api/projects/{project}.
// @Summary Get a project
// @Description Returns the project with the package versions recorded for it.
// @Tags projects
// @Produce json
// @Param project path string true "Project path, e.g. group/sub/repo"
// @Success 200 {object} ProjectResponse
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/projects/{project} [get]
func (h *ProjectsAPIHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	name, ok := validProjectPath(w, r)
	if !ok {
		return
	}
	project, err := h.db.GetProject(name)
	if err != nil {
		h.logger.Error("failed to get project", "project", name, "error", err)
		internalError(w, "failed to get project")
		return
	}
	if project == nil {
		notFound(w, "project not found")
		return
	}
	components, err := h.db.GetProjectComponents(project.ID)
	if err != nil {
		h.logger.Error("failed to get project components", "project", name, "error", err)
		internalError(w, "failed to get project components")
		return
	}
	resp := ProjectResponse{ProjectSummary: projectSummary(*project), Components: make([]ProjectComponent, len(components))}
	for i, c := range components {
		resp.Components[i] = ProjectComponent{
			Ecosystem: c.Ecosystem,
			Name:      c.Name,
			Version:   c.Version,
			PURL:      packageurl.WithVersionString(c.PackagePURL, c.Version),
		}
	}
	writeJSON(w, resp)
}

func projectSummary(p database.Project) ProjectSummary {
	return ProjectSummary{
		Name:           p.Name,
		Ref:            p.Ref.String,
		Branch:         p.Branch.String,
		UploadedAt:     p.UploadedAt,
		ComponentCount: &p.Components,
	}
}

// projectPath returns the decoded wildcard of a /api/projects or
// /ui/projects route. chi hands it over still encoded when the request path
// had escapes its canonical form doesn't (wildcardIsEncoded), e.g. a project
// name sent as one segment with %2F.
func projectPath(r *http.Request) (string, error) {
	wildcard := chi.URLParam(r, "*")
	if !wildcardIsEncoded(r) {
		return wildcard, nil
	}
	decoded, err := url.PathUnescape(wildcard)
	if err != nil {
		return "", errors.New("invalid escape in project path")
	}
	return decoded, nil
}

// validProjectPath returns the project named by the route, answering 400 if
// it is not a valid project name.
func validProjectPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	name, err := projectPath(r)
	if err == nil {
		err = inventory.ValidateProjectName(name)
	}
	if err != nil {
		badRequest(w, err.Error())
		return "", false
	}
	return name, true
}
