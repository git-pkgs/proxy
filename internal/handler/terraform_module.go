package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/git-pkgs/purl"
)

// headerTerraformGet is where the Module Registry Protocol gives the
// location of a module version's source.
const headerTerraformGet = "X-Terraform-Get"

// terraformModule is a module on one registry.
type terraformModule struct {
	terraformRegistry
	namespace, name, system string
}

// moduleFromRequest returns the module a request names. Unlike a provider
// address, its case is kept, as Terraform keeps it: a registry may match
// module addresses case-sensitively.
func (h *TerraformHandler) moduleFromRequest(w http.ResponseWriter, r *http.Request) (terraformModule, bool) {
	registry, ok := h.registryFromRequest(w, r)
	if !ok {
		return terraformModule{}, false
	}
	m := terraformModule{
		terraformRegistry: registry,
		namespace:         r.PathValue("namespace"),
		name:              r.PathValue("name"),
		system:            r.PathValue("system"),
	}
	for _, part := range []string{m.namespace, m.name, m.system} {
		if !terraformName.MatchString(part) {
			http.Error(w, "invalid module address", http.StatusBadRequest)
			return terraformModule{}, false
		}
	}
	return m, true
}

// handleModuleVersions serves the module's version list as the registry
// gives it.
func (h *TerraformHandler) handleModuleVersions(w http.ResponseWriter, r *http.Request) {
	m, ok := h.moduleFromRequest(w, r)
	if !ok {
		return
	}
	modulesURL, err := h.serviceURL(r.Context(), m.terraformRegistry, "modules.v1")
	if err != nil {
		h.writeModuleError(w, r, m, err)
		return
	}
	res := h.proxy.fetchMetadataDocument(r.Context(), terraformEcosystem, m.cacheKey(m.documentKey("versions")),
		modulesURL+m.path()+"/versions", contentTypeJSON, validTerraformJSON)
	if res.err != nil {
		h.writeModuleError(w, r, m, res.err)
		return
	}
	markStale(w, res.stale)
	w.Header().Set(headerContentType, contentTypeJSON)
	_, _ = w.Write(res.body)
}

// handleModuleDownload answers with the location of a module version's
// source, as the registry gives it, with a Git repository the Git handler
// serves replaced by its clone URL on the proxy, so the module is fetched
// through the git cache.
func (h *TerraformHandler) handleModuleDownload(w http.ResponseWriter, r *http.Request) {
	m, ok := h.moduleFromRequest(w, r)
	if !ok {
		return
	}
	version := r.PathValue("version")
	if !terraformVersion.MatchString(version) {
		http.Error(w, "invalid module version", http.StatusBadRequest)
		return
	}
	if versionPURL := m.purl(version); h.proxy.Denylist.Denied(versionPURL) {
		h.proxy.serveArtifactError(w, fmt.Errorf("%w: %s", ErrVersionDenied, versionPURL), "failed to fetch module")
		return
	}
	modulesURL, err := h.serviceURL(r.Context(), m.terraformRegistry, "modules.v1")
	if err != nil {
		h.writeModuleError(w, r, m, err)
		return
	}
	downloadURL := modulesURL + m.path() + "/" + url.PathEscape(version) + "/download"
	res := h.proxy.fetchMetadataDocument(r.Context(), terraformEcosystem, m.cacheKey(m.documentKey(version+"/download")),
		downloadURL, contentTypeJSON, validTerraformModuleDownload)
	if res.err != nil {
		h.writeModuleError(w, r, m, res.err)
		return
	}
	var download terraformModuleDownload
	_ = json.Unmarshal(res.body, &download)
	markStale(w, res.stale)
	w.Header().Set(headerTerraformGet, h.moduleLocation(download.Location, downloadURL))
	w.WriteHeader(http.StatusNoContent)
}

// terraformModuleDownload is a module version's download location: the
// JSON body registry.opentofu.org answers with, and what the X-Terraform-Get
// header registry.terraform.io answers with is stored as.
type terraformModuleDownload struct {
	Location string `json:"location"`
}

// terraformModuleDownloadDocument is the download document for location.
func terraformModuleDownloadDocument(location string) []byte {
	body, _ := json.Marshal(terraformModuleDownload{Location: location})
	return body
}

var errTerraformModuleLocation = errors.New("module download without a location")

func validTerraformModuleDownload(body []byte) error {
	var download terraformModuleDownload
	if err := json.Unmarshal(body, &download); err != nil {
		return fmt.Errorf("%w: %w", errTerraformDocument, err)
	}
	if download.Location == "" {
		return errTerraformModuleLocation
	}
	return nil
}

func validTerraformJSON(body []byte) error {
	if !json.Valid(body) {
		return fmt.Errorf("%w: invalid JSON", errTerraformDocument)
	}
	return nil
}

// moduleLocation returns where a client fetches a module version from,
// given the location the registry answered downloadURL with. A relative
// location is resolved against downloadURL, as Terraform does, and a git::
// location of a repository the Git handler serves is pointed at the proxy,
// keeping any subdirectory and query.
func (h *TerraformHandler) moduleLocation(location, downloadURL string) string {
	if strings.HasPrefix(location, "/") || strings.HasPrefix(location, "./") || strings.HasPrefix(location, "../") {
		base, err := url.Parse(downloadURL)
		ref, refErr := url.Parse(location)
		if err != nil || refErr != nil {
			return location
		}
		return base.ResolveReference(ref).String()
	}
	source, ok := strings.CutPrefix(location, "git::")
	if !ok {
		return location
	}
	// go-getter's syntax: the query follows the subdirectory, which
	// follows the repository after a "//".
	source, query, hasQuery := strings.Cut(source, "?")
	scheme, rest, ok := strings.Cut(source, "://")
	if !ok {
		return location
	}
	repo, subdir, hasSubdir := strings.Cut(rest, "//")
	cloneURL, ok := h.git.CloneURL(scheme + "://" + repo)
	if !ok {
		return location
	}
	location = "git::" + cloneURL
	if hasSubdir {
		location += "//" + subdir
	}
	if hasQuery {
		location += "?" + query
	}
	return location
}

func (h *TerraformHandler) writeModuleError(w http.ResponseWriter, r *http.Request, m terraformModule, err error) {
	h.writeRegistryError(w, r, m.hostname, "module", m.hostname+"/"+m.path(), err)
}

// path is the module's address below its registry: namespace/name/system.
func (m terraformModule) path() string {
	return m.namespace + "/" + m.name + "/" + m.system
}

func (m terraformModule) documentKey(document string) string {
	return "modules/" + m.path() + "/" + document
}

// purl returns the version PURL the denylist checks a module version
// against: pkg:terraform/{namespace}/{name}@{version}, as purl-spec#889
// drafts it, without the target system or the registry. It is lowercase, so
// a rule covers every spelling of the address.
func (m terraformModule) purl(version string) string {
	return purl.New(terraformEcosystem, strings.ToLower(m.namespace), strings.ToLower(m.name), version, nil).String()
}
