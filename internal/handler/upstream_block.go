package handler

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// upstreamStatusPattern matches the error the fetch layer returns for an
// upstream response it does not handle itself: fmt.Errorf("unexpected status
// %d: %s", code, body). The fetch layer exposes neither the status nor the
// body any other way, and the body is the only place an upstream's reason for
// refusing an artifact survives, so both are read back out of the message.
var upstreamStatusPattern = regexp.MustCompile(`unexpected status (\d+): `)

// upstreamStatus returns the status code and body of an upstream response the
// fetch layer rejected, and reports whether err carried one. Transport errors
// and the fetch layer's own sentinels (not found, rate limited, upstream down)
// carry no status and report false.
func upstreamStatus(err error) (code int, body string, ok bool) {
	if err == nil {
		return 0, "", false
	}
	msg := err.Error()
	loc := upstreamStatusPattern.FindStringSubmatchIndex(msg)
	if loc == nil {
		return 0, "", false
	}
	code, convErr := strconv.Atoi(msg[loc[2]:loc[3]])
	if convErr != nil {
		return 0, "", false
	}
	return code, msg[loc[1]:], true
}

// serveUpstreamBlock answers a request whose upstream refused the artifact on
// policy grounds with that upstream's status and report body, and reports
// whether err was such a refusal.
//
// A firewall in front of the upstream denies with 403 for npm, PyPI, Maven and
// OCI, and with 409 for NuGet, and puts the reason in the body. Answering with
// the real status and body keeps that reason visible to the package manager
// instead of the 502 a failed fetch otherwise produces, which reads as a proxy
// fault and hides why the artifact was refused.
func (p *Proxy) serveUpstreamBlock(w http.ResponseWriter, err error) bool {
	status, body, ok := upstreamStatus(err)
	if !ok || (status != http.StatusForbidden && status != http.StatusConflict) {
		return false
	}
	p.Logger.Warn("upstream refused artifact on policy grounds", "status", status, "detail", body)
	contentType := "text/plain; charset=utf-8"
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		contentType = contentTypeJSON
	}
	w.Header().Set(headerContentType, contentType)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
	return true
}
