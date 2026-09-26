package handler

import (
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// relayResponse forwards an unmodified upstream body. Callers still own and
// close resp.Body, including when a failed transfer aborts the handler. A custom
// header copier may select or rewrite end-to-end headers; it only sees headers
// after hop-by-hop fields have been removed.
func (p *Proxy) relayResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, copyHeaders func(http.Header, http.Header)) {
	blocked := relayHopHeaders(resp.Header)
	headers := resp.Header.Clone()
	for name := range blocked {
		headers.Del(name)
	}
	bodyAllowed := r.Method != http.MethodHead && resp.StatusCode >= http.StatusOK &&
		resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusResetContent && resp.StatusCode != http.StatusNotModified
	if resp.StatusCode < http.StatusOK || resp.StatusCode == http.StatusNoContent {
		headers.Del(headerContentLength)
	} else if resp.StatusCode == http.StatusResetContent {
		headers.Set(headerContentLength, "0")
	}
	if copyHeaders == nil {
		copyHeaders = copyRelayHeaders
	}
	copyHeaders(w.Header(), headers)

	announced := make(map[string]bool)
	if bodyAllowed {
		for name := range resp.Trailer {
			name = http.CanonicalHeaderKey(name)
			if validRelayTrailer(name, blocked) {
				announced[name] = true
				w.Header().Add("Trailer", name)
			}
		}
		if len(announced) > 0 {
			// HTTP/1 trailers require chunking, not a fixed Content-Length.
			w.Header().Del(headerContentLength)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if !bodyAllowed || resp.Body == nil {
		return
	}

	written, err := io.Copy(w, resp.Body)
	if err != nil {
		upstreamURL := ""
		if resp.Request != nil && resp.Request.URL != nil {
			upstreamURL = resp.Request.URL.Redacted()
		}
		p.Logger.Warn("upstream response relay failed", "url", upstreamURL,
			"status", resp.StatusCode, "bytes", written, "error", err)
		// Headers are already committed: an error page would turn a truncated
		// download into a seemingly successful response. Let net/http close the
		// HTTP/1 connection or reset the HTTP/2 stream instead.
		panic(http.ErrAbortHandler)
	}
	relayTrailers(w, resp.Trailer, announced, blocked)
}

func copyRelayHeaders(dst, src http.Header) {
	for name, values := range src {
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func relayHopHeaders(headers http.Header) map[string]bool {
	blocked := map[string]bool{
		"Connection": true, "Proxy-Connection": true, "Keep-Alive": true,
		"Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true,
		"Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	}
	for _, value := range headers.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				blocked[http.CanonicalHeaderKey(name)] = true
			}
		}
	}
	return blocked
}

func validRelayTrailer(name string, blocked map[string]bool) bool {
	return !blocked[name] && httpguts.ValidHeaderFieldName(name) && httpguts.ValidTrailerHeader(name)
}

func relayTrailers(w http.ResponseWriter, trailers http.Header, announced, blocked map[string]bool) {
	for name, values := range trailers {
		name = http.CanonicalHeaderKey(name)
		if !validRelayTrailer(name, blocked) {
			continue
		}
		if !announced[name] {
			// A trailer discovered only at EOF must not become a regular header
			// or trigger automatic Content-Length on a short buffered response.
			_ = http.NewResponseController(w).Flush()
			name = http.TrailerPrefix + name
		}
		w.Header()[name] = append([]string(nil), values...)
	}
}
