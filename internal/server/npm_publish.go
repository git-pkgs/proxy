package server

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // npm's legacy dist.shasum is sha1
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/git-pkgs/proxy/internal/handler"
)

// npmPublishOverhead allows for the JSON around the base64 tarball in a
// publish document.
const npmPublishOverhead = 1 << 20

// npmPublishDocument is the body npm publish sends: the package document
// with the new version and its tarball as a base64 attachment.
type npmPublishDocument struct {
	Name        string                          `json:"name"`
	DistTags    map[string]string               `json:"dist-tags"`
	Versions    map[string]npmPublishVersion    `json:"versions"`
	Attachments map[string]npmPublishAttachment `json:"_attachments"`
}

type npmPublishVersion struct {
	Dist struct {
		Integrity string `json:"integrity"`
		Shasum    string `json:"shasum"`
	} `json:"dist"`
}

type npmPublishAttachment struct {
	Data string `json:"data"`
}

// withNPMPublish sends PUT requests for a package to publish and everything
// else to the npm handler.
func withNPMPublish(npm, publish http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			publish.ServeHTTP(w, r)
			return
		}
		npm.ServeHTTP(w, r)
	})
}

// HandleNPMPublish handles PUT /npm/{name}, what npm publish sends. The
// tarball goes through the same push path as PUT /api/hosted/npm/..., so it
// is checked, stored and listed the same way. One new version per request,
// tagged latest or not at all; npm unpublish, deprecate and dist-tag are not
// supported.
func (h *HostedAPIHandler) HandleNPMPublish(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" || strings.Contains(name, "/-/") || strings.HasPrefix(name, "-/") {
		npmError(w, http.StatusMethodNotAllowed, ErrCodeBadRequest, "only npm publish is supported")
		return
	}
	if !mayPush(r, "npm", name) {
		npmError(w, http.StatusForbidden, ErrCodeForbidden, mayNotPushMessage("npm", name))
		return
	}

	var doc npmPublishDocument
	body := http.MaxBytesReader(w, r.Body, h.maxUploadBytes/3*4+npmPublishOverhead)
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.npmPushError(w, err)
			return
		}
		npmError(w, http.StatusBadRequest, ErrCodeBadRequest, "request body is not an npm publish document")
		return
	}
	version, tarball, ok := h.publishedVersion(w, name, doc)
	if !ok {
		return
	}

	target, err := handler.HostedTargetFor("npm", name, version, "")
	if err != nil {
		npmError(w, http.StatusBadRequest, ErrCodeBadRequest, err.Error())
		return
	}
	if err := h.proxy.CheckPush("npm", target); err != nil {
		h.npmPushError(w, err)
		return
	}
	pushed, err := h.proxy.PushLocalArtifact(r.Context(), "npm", target, bytes.NewReader(tarball), "application/octet-stream", "")
	if err != nil {
		h.npmPushError(w, err)
		return
	}
	h.logger.Info("published npm package", "name", name, "version", version,
		"token", credentialFrom(r.Context()).name, "sha256", pushed.SHA256)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"ok": true, "id": name + "@" + version})
}

// publishedVersion checks a publish document and returns the version it adds
// with its decoded tarball. It answers the request itself when it returns
// false.
func (h *HostedAPIHandler) publishedVersion(w http.ResponseWriter, name string, doc npmPublishDocument) (string, []byte, bool) {
	if doc.Name != name {
		npmError(w, http.StatusBadRequest, ErrCodeBadRequest, "package name in the document doesn't match the URL")
		return "", nil, false
	}
	if len(doc.Attachments) == 0 {
		npmError(w, http.StatusBadRequest, ErrCodeBadRequest, "only publishing a new version is supported, not changing an existing one")
		return "", nil, false
	}
	if len(doc.Versions) != 1 || len(doc.Attachments) != 1 {
		npmError(w, http.StatusBadRequest, ErrCodeBadRequest, "publish one version with one tarball at a time")
		return "", nil, false
	}
	var version string
	var entry npmPublishVersion
	for version, entry = range doc.Versions {
	}
	for tag, tagged := range doc.DistTags {
		if tag != "latest" || tagged != version {
			npmError(w, http.StatusBadRequest, ErrCodeBadRequest, "only the latest dist-tag is supported; publish without --tag")
			return "", nil, false
		}
	}
	var attachment npmPublishAttachment
	for _, attachment = range doc.Attachments {
	}
	tarball, err := base64.StdEncoding.DecodeString(attachment.Data)
	if err != nil || len(tarball) == 0 {
		npmError(w, http.StatusBadRequest, ErrCodeBadRequest, "the tarball attachment is not valid base64")
		return "", nil, false
	}
	if int64(len(tarball)) > h.maxUploadBytes {
		npmError(w, http.StatusRequestEntityTooLarge, ErrCodeBadRequest, "archive exceeds hosted.max_upload_size")
		return "", nil, false
	}
	if !npmDistMatches(entry.Dist.Integrity, entry.Dist.Shasum, tarball) {
		npmError(w, http.StatusBadRequest, ErrCodeBadRequest, "the tarball doesn't match dist.integrity or dist.shasum")
		return "", nil, false
	}
	return version, tarball, true
}

// npmDistMatches checks the tarball against the sha512 entries of an
// integrity string, or against the sha1 shasum when there are none. A
// document with neither passes; the stored archive gets its own digests.
func npmDistMatches(integrity, shasum string, tarball []byte) bool {
	sha512Sum := sha512.Sum512(tarball)
	want := base64.StdEncoding.EncodeToString(sha512Sum[:])
	checked := false
	for _, entry := range strings.Fields(integrity) {
		digest, ok := strings.CutPrefix(entry, "sha512-")
		if !ok {
			continue
		}
		if digest == want {
			return true
		}
		checked = true
	}
	if checked {
		return false
	}
	if shasum != "" {
		sha1Sum := sha1.Sum(tarball) //nolint:gosec // npm's legacy dist.shasum is sha1
		return strings.EqualFold(shasum, hex.EncodeToString(sha1Sum[:]))
	}
	return true
}

// npmError writes an error response npm can show: the npm CLI prints the
// "error" field, the other hosted endpoints' clients read "code" and
// "message".
func npmError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message, "error": message})
}

func (h *HostedAPIHandler) npmPushError(w http.ResponseWriter, err error) {
	status, code, message := h.pushErrorResponse(err)
	npmError(w, status, code, message)
}
