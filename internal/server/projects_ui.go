package server

import (
	"cmp"
	"net/http"
	"net/url"
	"slices"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/inventory"
	"github.com/go-chi/chi/v5"
)

// ProjectsListData contains data for rendering the projects list page.
type ProjectsListData struct {
	Layout
	Projects []ProjectRow
}

// ProjectRow is a project in the UI.
type ProjectRow struct {
	Name       string
	Ref        string
	Branch     string
	UploadedAt string
	Components int64
}

// ProjectShowData contains data for rendering a project's page.
type ProjectShowData struct {
	Layout
	Project    ProjectRow
	Components []ProjectComponentRow
	Vulnerable int
	Outdated   int
}

// ProjectComponentRow is a package version a project uses, with what the
// proxy knows about it.
type ProjectComponentRow struct {
	Ecosystem string
	Name      string
	Version   string
	// Cached is set when the proxy has a package record, so the package
	// page exists; CachedEcosystem and CachedName address it.
	Cached          bool
	CachedEcosystem string
	CachedName      string
	LatestVersion   string
	Outdated        bool
	VulnCount       int
	Severity        string
}

// VersionUsage lists the projects using one version of a package.
type VersionUsage struct {
	Version        string
	EscapedVersion string
	// Cached is set when the proxy has a record of the version.
	Cached   bool
	Projects []string
}

// VulnerableVersion is a package version with known vulnerabilities that
// projects use.
type VulnerableVersion struct {
	Ecosystem string
	Name      string
	Version   string
	Severity  string
	VulnCount int
	Projects  []string
}

// mountProjectUI registers the project inventory pages when
// projects.enabled is set.
func (s *Server) mountProjectUI(ui chi.Router) {
	if !s.cfg.Projects.Enabled {
		return
	}
	ui.Get("/projects", s.handleProjectsList)
	ui.Get("/projects/*", s.handleProjectShow)
}

func (s *Server) handleProjectsList(w http.ResponseWriter, r *http.Request) {
	projects, err := s.db.ListProjects()
	if err != nil {
		s.logger.Error("failed to list projects", "error", err)
		http.Error(w, "failed to list projects", http.StatusInternalServerError)
		return
	}
	data := ProjectsListData{Layout: s.layoutFor(r), Projects: make([]ProjectRow, len(projects))}
	for i, p := range projects {
		data.Projects[i] = projectRow(p)
	}
	if err := s.templates.Render(w, "projects_list", data); err != nil {
		s.logger.Error("failed to render projects list", "error", err)
	}
}

func (s *Server) handleProjectShow(w http.ResponseWriter, r *http.Request) {
	name, err := projectPath(r)
	if err == nil {
		err = inventory.ValidateProjectName(name)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	project, err := s.db.GetProject(name)
	if err != nil {
		s.logger.Error("failed to get project", "project", name, "error", err)
		http.Error(w, "failed to get project", http.StatusInternalServerError)
		return
	}
	if project == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	components, err := s.db.GetProjectComponents(project.ID)
	if err != nil {
		s.logger.Error("failed to get project components", "project", name, "error", err)
		components = nil
	}
	vulns, err := s.db.GetComponentVulnerabilities(name)
	if err != nil {
		s.logger.Error("failed to get project vulnerabilities", "project", name, "error", err)
	}
	affecting := affectingVulnerabilities(vulns)

	data := ProjectShowData{Layout: s.layoutFor(r), Project: projectRow(*project)}
	for _, c := range components {
		row := ProjectComponentRow{
			Ecosystem:       c.Ecosystem,
			Name:            c.Name,
			Version:         c.Version,
			Cached:          c.CachedName.Valid,
			CachedEcosystem: c.CachedEcosystem.String,
			CachedName:      c.CachedName.String,
			LatestVersion:   c.LatestVersion.String,
			Outdated:        inventory.Outdated(c.Ecosystem, c.Version, c.LatestVersion.String),
		}
		if v, ok := affecting[componentKey{c.PackagePURL, c.Version}]; ok {
			row.VulnCount, row.Severity = len(v.ids), v.severity
			data.Vulnerable++
		}
		if row.Outdated {
			data.Outdated++
		}
		data.Components = append(data.Components, row)
	}
	if err := s.templates.Render(w, "project_show", data); err != nil {
		s.logger.Error("failed to render project page", "error", err)
	}
}

func projectRow(p database.Project) ProjectRow {
	return ProjectRow{
		Name:       p.Name,
		Ref:        p.Ref.String,
		Branch:     p.Branch.String,
		UploadedAt: formatTimeAgo(p.UploadedAt),
		Components: p.Components,
	}
}

// packageUsedBy lists the projects using each version of pkg, highest
// version first, or nothing when the project inventory is disabled. versions
// are the versions the proxy has records of; a version only projects know of
// is listed too.
func (s *Server) packageUsedBy(pkg *database.Package, versions []database.Version) []VersionUsage {
	if !s.cfg.Projects.Enabled {
		return nil
	}
	usages, err := s.db.GetProjectsUsingPackage(pkg.PURL, "")
	if err != nil {
		s.logger.Error("failed to get projects using package", "purl", pkg.PURL, "error", err)
		return nil
	}
	known := make(map[string]bool, len(versions))
	for i := range versions {
		known[versions[i].Version()] = true
	}
	byVersion := map[string]*VersionUsage{}
	var result []*VersionUsage
	for _, u := range usages {
		vu, ok := byVersion[u.Version]
		if !ok {
			vu = &VersionUsage{Version: u.Version, EscapedVersion: url.PathEscape(u.Version), Cached: known[u.Version]}
			byVersion[u.Version] = vu
			result = append(result, vu)
		}
		vu.Projects = append(vu.Projects, u.Name)
	}
	slices.SortFunc(result, func(a, b *VersionUsage) int {
		return inventory.CompareVersions(pkg.Ecosystem, b.Version, a.Version)
	})
	out := make([]VersionUsage, len(result))
	for i, vu := range result {
		out[i] = *vu
	}
	return out
}

// vulnerableVersionsInUse returns the package versions projects use that
// have vulnerabilities, most severe and most used first, at most limit. It
// returns nothing when the project inventory is disabled.
func (s *Server) vulnerableVersionsInUse(limit int) []VulnerableVersion {
	if !s.cfg.Projects.Enabled {
		return nil
	}
	vulns, err := s.db.GetComponentVulnerabilities("")
	if err != nil {
		s.logger.Error("failed to get vulnerable versions in use", "error", err)
		return nil
	}
	affecting := affectingVulnerabilities(vulns)
	result := make([]VulnerableVersion, 0, len(affecting))
	for _, v := range affecting {
		projects := make([]string, 0, len(v.projects))
		for p := range v.projects {
			projects = append(projects, p)
		}
		slices.Sort(projects)
		result = append(result, VulnerableVersion{
			Ecosystem: v.ecosystem, Name: v.name, Version: v.version,
			Severity: v.severity, VulnCount: len(v.ids), Projects: projects,
		})
	}
	slices.SortFunc(result, func(a, b VulnerableVersion) int {
		return cmp.Or(
			cmp.Compare(severityRank(b.Severity), severityRank(a.Severity)),
			cmp.Compare(len(b.Projects), len(a.Projects)),
			cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.Version, b.Version),
		)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

type componentKey struct{ packagePURL, version string }

// affectingVersion collects the vulnerabilities affecting one package
// version, and the projects using it.
type affectingVersion struct {
	ecosystem, name, version string
	severity                 string
	ids                      map[string]bool
	projects                 map[string]bool
}

// affectingVulnerabilities keeps the vulnerabilities that affect the version
// each component uses, grouped by package version.
func affectingVulnerabilities(vulns []database.ComponentVulnerability) map[componentKey]*affectingVersion {
	out := map[componentKey]*affectingVersion{}
	for _, v := range vulns {
		if !inventory.Affected(v.Ecosystem, v.Version, v.FixedVersion.String) {
			continue
		}
		key := componentKey{v.PackagePURL, v.Version}
		a, ok := out[key]
		if !ok {
			a = &affectingVersion{
				ecosystem: v.CachedEcosystem, name: v.CachedName, version: v.Version,
				ids: map[string]bool{}, projects: map[string]bool{},
			}
			out[key] = a
		}
		a.ids[v.VulnID] = true
		a.projects[v.Project] = true
		if severityRank(v.Severity.String) > severityRank(a.severity) {
			a.severity = v.Severity.String
		}
	}
	return out
}

// severityOrder lists severities from least to most severe.
var severityOrder = []string{"low", "medium", "high", "critical"}

// severityRank orders severities; unknown ones rank below "low".
func severityRank(severity string) int {
	return slices.Index(severityOrder, severity) + 1
}
