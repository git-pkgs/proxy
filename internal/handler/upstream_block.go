package handler

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/git-pkgs/registries/fetch"
)

// serveUpstreamBlock forwards an upstream policy denial with its own status and body.
func (p *Proxy) serveUpstreamBlock(w http.ResponseWriter, err error) bool {
	var statusErr *fetch.StatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	if statusErr.Code != http.StatusForbidden && statusErr.Code != http.StatusConflict {
		return false
	}
	p.Logger.Warn("upstream refused artifact on policy grounds", "status", statusErr.Code, "detail", statusErr.Body)
	w.Header().Set(headerContentType, upstreamBlockContentType(statusErr))
	w.WriteHeader(statusErr.Code)
	_, _ = w.Write([]byte(statusErr.Body))
	return true
}

// upstreamBlockContentType forwards the type the upstream labelled the body
// with, and falls back to JSON only while that body is still valid JSON.
func upstreamBlockContentType(statusErr *fetch.StatusError) string {
	validJSON := json.Valid([]byte(statusErr.Body))
	if contentType := statusErr.ContentType; contentType != "" && (validJSON || !jsonMediaType(contentType)) {
		return contentType
	}
	if validJSON {
		return contentTypeJSON
	}
	return "text/plain; charset=utf-8"
}

// jsonMediaType reports whether a Content-Type names a JSON document.
func jsonMediaType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == contentTypeJSON || strings.HasSuffix(mediaType, "+json")
}
