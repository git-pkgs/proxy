package server

import "net/http"

// BuildInfo identifies the running proxy binary.
type BuildInfo struct {
	Version string
	Commit  string
}

// Layout carries shared fields consumed by the base template. It is embedded
// in every page data struct so templates can access canonical URL and build
// information alongside the page's own fields.
type Layout struct {
	BuildInfo     BuildInfo
	UIBaseURL     string
	CanonicalPath string
}

func (s *Server) layoutFor(r *http.Request) Layout {
	return Layout{
		BuildInfo:     s.buildInfo,
		UIBaseURL:     s.cfg.UIBaseURL,
		CanonicalPath: r.URL.Path,
	}
}
