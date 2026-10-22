package server

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/git-pkgs/integrity"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/go-chi/chi/v5"
)

// HostedAPIHandler serves /api/hosted, where operators push archives that the
// proxy serves in place of, or in addition to, what the upstream has.
type HostedAPIHandler struct {
	proxy          *handler.Proxy
	maxUploadBytes int64
	logger         *slog.Logger
}

// NewHostedAPIHandler creates the /api/hosted handler.
func NewHostedAPIHandler(proxy *handler.Proxy, maxUploadBytes int64, logger *slog.Logger) *HostedAPIHandler {
	return &HostedAPIHandler{proxy: proxy, maxUploadBytes: maxUploadBytes, logger: logger}
}

// HostedPushResponse describes a pushed archive.
type HostedPushResponse struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	handler.HostedArtifact
}

// HostedListResponse lists the archives pushed for a package.
type HostedListResponse struct {
	Ecosystem string                   `json:"ecosystem"`
	Name      string                   `json:"name"`
	Artifacts []handler.HostedArtifact `json:"artifacts"`
}

// registerHostedRoutes mounts /api/hosted. Listing is open like the rest of
// the API; pushing and deleting require the bearer token.
func registerHostedRoutes(r chi.Router, h *HostedAPIHandler, auth *hostedAuth) {
	r.Get("/api/hosted/{ecosystem}/*", h.HandleList)
	r.Group(func(w chi.Router) {
		w.Use(requireHostedToken(auth))
		w.Put("/api/hosted/{ecosystem}/*", h.HandlePush)
		w.Delete("/api/hosted/{ecosystem}/*", h.HandleDelete)
	})
}

// HandlePush handles PUT /api/hosted/{ecosystem}/{name}/{version}, and
// PUT /api/hosted/generic/{name}/{version}/{file} for generic files.
// @Summary Push a package archive
// @Description Stores the request body as the archive for name@version. It is served in place of the upstream archive with the same version and is never evicted. Generic files are pushed to /api/hosted/generic/{name}/{version}/{file} and served at /generic/hosted/{name}/{version}/{file}. A file that was already pushed is refused with 409; delete it first to push it again.
// @Tags hosted
// @Accept octet-stream
// @Produce json
// @Security BearerAuth
// @Param ecosystem path string true "Ecosystem (npm, composer or generic)"
// @Param name path string true "Package name, including an npm scope or the Composer vendor"
// @Param version path string true "Version"
// @Param sha256 query string false "Expected sha256 of the body, hex-encoded; a mismatch is refused"
// @Success 201 {object} HostedPushResponse
// @Failure 400 {object} ErrorResponse
// @Failure 401 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Failure 413 {object} ErrorResponse
// @Router /api/hosted/{ecosystem}/{name}/{version} [put]
func (h *HostedAPIHandler) HandlePush(w http.ResponseWriter, r *http.Request) {
	path, ok := hostedPath(w, r, true)
	if !ok {
		return
	}
	target, err := handler.HostedTargetFor(path.ecosystem, path.name, path.version, path.filename)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if !mayPush(r, path.ecosystem, path.name) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, mayNotPushMessage(path.ecosystem, path.name))
		return
	}
	wantSHA256 := r.URL.Query().Get("sha256")
	if wantSHA256 != "" && !validSHA256(wantSHA256) {
		badRequest(w, "sha256 must be 64 hex characters")
		return
	}
	ecosystem := path.ecosystem

	// Refuse before reading the body, so a client sending
	// "Expect: 100-continue" isn't made to upload a file that is refused.
	if err := h.proxy.CheckPush(ecosystem, target); err != nil {
		h.pushError(w, err)
		return
	}

	body := bufio.NewReader(http.MaxBytesReader(w, r.Body, h.maxUploadBytes))
	if _, err := body.Peek(1); errors.Is(err, io.EOF) {
		badRequest(w, "request body is empty")
		return
	}

	pushed, err := h.proxy.PushLocalArtifact(r.Context(), ecosystem, target, body, r.Header.Get("Content-Type"), wantSHA256)
	if err != nil {
		h.pushError(w, err)
		return
	}
	h.logger.Info("pushed package archive",
		"ecosystem", ecosystem, "name", target.Name, "version", target.Version,
		"filename", target.Filename, "sha256", pushed.SHA256)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, HostedPushResponse{Ecosystem: ecosystem, Name: target.Name, HostedArtifact: pushed})
}

func (h *HostedAPIHandler) pushError(w http.ResponseWriter, err error) {
	status, code, message := h.pushErrorResponse(err)
	writeError(w, status, code, message)
}

// pushErrorResponse maps a push error to the status, code and message
// clients get, logging the ones that aren't the client's fault.
func (h *HostedAPIHandler) pushErrorResponse(err error) (int, string, string) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return http.StatusRequestEntityTooLarge, ErrCodeBadRequest, "archive exceeds hosted.max_upload_size"
	case errors.Is(err, handler.ErrHostedInvalidArchive), errors.Is(err, handler.ErrHostedManifestMismatch),
		errors.Is(err, handler.ErrHostedChecksumMismatch):
		return http.StatusBadRequest, ErrCodeBadRequest, err.Error()
	case errors.Is(err, handler.ErrHostedExists):
		return http.StatusConflict, ErrCodeConflict, err.Error()
	case errors.Is(err, handler.ErrVersionDenied), errors.Is(err, handler.ErrArtifactBlocked):
		return http.StatusForbidden, ErrCodeBadRequest, err.Error()
	default:
		h.logger.Error("failed to push package archive", "error", err)
		return http.StatusInternalServerError, ErrCodeInternal, "failed to store archive"
	}
}

// HandleList handles GET /api/hosted/{ecosystem}/{name}
// @Summary List pushed package archives
// @Tags hosted
// @Produce json
// @Param ecosystem path string true "Ecosystem"
// @Param name path string true "Package name"
// @Success 200 {object} HostedListResponse
// @Failure 400 {object} ErrorResponse
// @Router /api/hosted/{ecosystem}/{name} [get]
func (h *HostedAPIHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	segments, err := packagePathSegments(r)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	name := strings.Join(segments, "/")
	if !handler.HostedSupported(ecosystem) {
		badRequest(w, handler.ErrHostedUnsupported.Error())
		return
	}

	listed, err := h.proxy.LocalArtifacts(ecosystem, name)
	if err != nil {
		h.logger.Error("failed to list pushed archives", "error", err)
		internalError(w, "failed to list pushed archives")
		return
	}
	writeJSON(w, HostedListResponse{Ecosystem: ecosystem, Name: name, Artifacts: listed})
}

// HandleDelete handles DELETE /api/hosted/{ecosystem}/{name}/{version}, and
// DELETE /api/hosted/generic/{name}/{version}/{file} for one generic file.
// @Summary Delete a pushed version
// @Description Removes the archives pushed for name@version. If the upstream has the version, clients get the upstream archive again. For generic files, /api/hosted/generic/{name}/{version}/{file} removes only that file.
// @Tags hosted
// @Security BearerAuth
// @Param ecosystem path string true "Ecosystem"
// @Param name path string true "Package name"
// @Param version path string true "Version"
// @Success 204
// @Failure 401 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/hosted/{ecosystem}/{name}/{version} [delete]
func (h *HostedAPIHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	path, ok := hostedPath(w, r, false)
	if !ok {
		return
	}
	if err := handler.ValidateHostedPath(path.ecosystem, path.name, path.version, path.filename); err != nil {
		badRequest(w, err.Error())
		return
	}
	if !mayPush(r, path.ecosystem, path.name) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, mayNotPushMessage(path.ecosystem, path.name))
		return
	}
	var err error
	if path.filename != "" {
		err = h.proxy.DeleteLocalFile(path.ecosystem, path.name, path.version, path.filename)
	} else {
		err = h.proxy.DeleteLocalVersion(path.ecosystem, path.name, path.version)
	}
	switch {
	case errors.Is(err, handler.ErrHostedVersionNotFound), errors.Is(err, handler.ErrHostedFileNotFound):
		notFound(w, err.Error())
	case err != nil:
		h.logger.Error("failed to delete pushed version", "error", err)
		internalError(w, "failed to delete pushed version")
	default:
		h.logger.Info("deleted pushed version", "ecosystem", path.ecosystem, "name", path.name,
			"version", path.version, "filename", path.filename)
		w.WriteHeader(http.StatusNoContent)
	}
}

// minVersionPathSegments is one name segment plus the version.
const minVersionPathSegments = 2

// hostedPushPath is a parsed /api/hosted/{ecosystem}/... path.
type hostedPushPath struct {
	ecosystem string
	name      string
	version   string
	filename  string
}

// hostedPath splits the wildcard into name, version and, for ecosystems
// whose pushes name the file, the filename. The version is the last segment
// otherwise, and the name may span several (npm scopes, Composer vendors).
// Ecosystems that name the file have single-segment names, so their path is
// {name}/{version}/{file}; requireFile is false for deletes, where
// {name}/{version} removes the whole version.
func hostedPath(w http.ResponseWriter, r *http.Request, requireFile bool) (hostedPushPath, bool) {
	segments, err := packagePathSegments(r)
	if err != nil {
		badRequest(w, err.Error())
		return hostedPushPath{}, false
	}
	path := hostedPushPath{ecosystem: chi.URLParam(r, "ecosystem")}
	if handler.HostedFileInPath(path.ecosystem) {
		switch {
		case len(segments) == minVersionPathSegments+1:
			path.name, path.version, path.filename = segments[0], segments[1], segments[2]
		case len(segments) == minVersionPathSegments && !requireFile:
			path.name, path.version = segments[0], segments[1]
		default:
			badRequest(w, "path must be /api/hosted/"+path.ecosystem+"/{name}/{version}/{file}")
			return hostedPushPath{}, false
		}
		return path, true
	}
	if len(segments) < minVersionPathSegments {
		badRequest(w, "path must be /api/hosted/{ecosystem}/{name}/{version}")
		return hostedPushPath{}, false
	}
	last := len(segments) - 1
	path.name, path.version = strings.Join(segments[:last], "/"), segments[last]
	return path, true
}

// validSHA256 reports whether s is a hex-encoded sha256 digest.
func validSHA256(s string) bool {
	_, err := integrity.ParseHex(integrity.SHA256, s)
	return err == nil
}
