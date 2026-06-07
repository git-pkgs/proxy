package server

import "net/http"

// Layout carries per-request fields consumed by the shared base template
// (canonical URL, og:url). It is embedded in every page data struct so that
// templates can reference {{.UIBaseURL}} and {{.CanonicalPath}} alongside the
// page's own fields.
type Layout struct {
	UIBaseURL     string
	CanonicalPath string
}

func (s *Server) layoutFor(r *http.Request) Layout {
	return Layout{
		UIBaseURL:     s.cfg.UIBaseURL,
		CanonicalPath: r.URL.Path,
	}
}
