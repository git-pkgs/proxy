package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/gitcache"
	"github.com/git-pkgs/proxy/internal/metrics"
)

const (
	gitEcosystem = "git"

	gitInfoRefs    = "/info/refs"
	gitUploadPack  = "/git-upload-pack"
	gitReceivePack = "/git-receive-pack"
	gitRequestType = "application/x-git-upload-pack-request"

	// gitRequestTimeout replaces the server's read and write timeouts for Git
	// requests: a cold clone of a large repository keeps the client waiting
	// before the first byte, a large pack takes a while to send, and the rest
	// of a request body is read only after any fetch it needs.
	gitRequestTimeout = 2 * time.Hour
)

// GitHandler serves Git repositories read-only over smart HTTP at
// /git/{name}/{repo}, from a local cache of the upstream.git hosts. Clients
// clone and fetch with ordinary git commands, usually through an insteadOf
// rewrite. Pushing and the dumb protocol are refused.
type GitHandler struct {
	proxy     *Proxy
	cache     *gitcache.Cache
	upstreams map[string]gitUpstream
}

type gitUpstream struct {
	config.GitUpstream
	// dir names the upstream's directory in the cache (gitcache.UpstreamDir).
	dir string
	// fetchWants is off when upstream.auth has credentials for the host. A
	// fetch by ID there could reach commits outside the repos allowlist,
	// as GitHub serves any commit in a repository's fork network.
	fetchWants bool
}

// NewGitHandler creates a Git handler serving upstream.git from cache.
func NewGitHandler(proxy *Proxy, cache *gitcache.Cache, upstream config.UpstreamConfig) *GitHandler {
	h := &GitHandler{proxy: proxy, cache: cache, upstreams: make(map[string]gitUpstream, len(upstream.Git))}
	for name, git := range upstream.Git {
		git.URL = strings.TrimSuffix(git.URL, "/")
		h.upstreams[name] = gitUpstream{
			GitUpstream: git,
			dir:         gitcache.UpstreamDir(name, git.URL),
			fetchWants:  !upstream.HasAuthWithin(git.URL),
		}
	}
	return h
}

// Routes returns the HTTP handler for Git requests. Mount this at /git.
func (h *GitHandler) Routes() http.Handler {
	return http.HandlerFunc(h.serve)
}

// gitRequest is a parsed /git/{name}/{repo}/{endpoint} request.
type gitRequest struct {
	infoRefs   bool
	fetchWants bool
	// key names the repository in the cache: {name}-{URL hash}/{repo}.git,
	// with any .git the client gave removed from repo first, so both
	// spellings share one copy.
	key string
	// repoURL is the upstream repository, spelled as the client spelled it.
	repoURL string
}

func (h *GitHandler) serve(w http.ResponseWriter, r *http.Request) {
	req, status, msg := h.parse(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	deadline := time.Now().Add(gitRequestTimeout)
	rc := http.NewResponseController(w)
	if err := errors.Join(rc.SetReadDeadline(deadline), rc.SetWriteDeadline(deadline)); err != nil {
		h.proxy.Logger.Warn("git: cannot extend the request deadlines", "error", err)
	}
	// upload-pack writes its response while its stdin is still attached to
	// the request body. Without full duplex, an HTTP/1 server reads off and
	// closes whatever body is left once the response is flushed.
	if !req.infoRefs {
		if err := rc.EnableFullDuplex(); err != nil {
			h.proxy.Logger.Warn("git: cannot enable full duplex", "error", err)
		}
	}

	lease, err := h.cache.Open(r.Context(), req.key, req.repoURL,
		gitcache.OpenOptions{Refresh: req.infoRefs, FetchWants: req.fetchWants})
	if err != nil {
		h.serveError(w, r, err)
		return
	}
	defer lease.Release()
	if !req.infoRefs {
		lease.ServeUploadPack(w, r)
		return
	}
	if lease.Hit() {
		metrics.RecordArtifactCacheHit(gitEcosystem)
	} else {
		metrics.RecordArtifactCacheMiss(gitEcosystem)
	}
	lease.ServeInfoRefs(w, r)
}

func (h *GitHandler) parse(r *http.Request) (gitRequest, int, string) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	var req gitRequest
	switch {
	case strings.HasSuffix(path, gitInfoRefs):
		path = strings.TrimSuffix(path, gitInfoRefs)
		req.infoRefs = true
		switch r.URL.Query().Get("service") {
		case "git-upload-pack":
		case "git-receive-pack":
			return req, http.StatusForbidden, "pushing is not supported"
		default:
			return req, http.StatusForbidden, "only the smart HTTP protocol is supported"
		}
		if r.Method != http.MethodGet {
			return req, http.StatusMethodNotAllowed, "method not allowed"
		}
	case strings.HasSuffix(path, gitUploadPack):
		path = strings.TrimSuffix(path, gitUploadPack)
		if r.Method != http.MethodPost {
			return req, http.StatusMethodNotAllowed, "method not allowed"
		}
		if r.Header.Get("Content-Type") != gitRequestType {
			return req, http.StatusUnsupportedMediaType, "unsupported content type"
		}
	case strings.HasSuffix(path, gitReceivePack):
		return req, http.StatusForbidden, "pushing is not supported"
	default:
		return req, http.StatusNotFound, "not found"
	}

	name, repoPath, _ := strings.Cut(path, "/")
	upstream, ok := h.upstreams[name]
	if !ok {
		return req, http.StatusNotFound, "unknown upstream"
	}
	segments := strings.Split(repoPath, "/")
	last := len(segments) - 1
	suffix := ""
	if trimmed, ok := strings.CutSuffix(segments[last], ".git"); ok {
		segments[last], suffix = trimmed, ".git"
	}
	escaped := make([]string, len(segments))
	for i, segment := range segments {
		if !validGitSegment(segment, i == last) {
			return req, http.StatusNotFound, "invalid repository path"
		}
		escaped[i] = url.PathEscape(segment)
	}
	repo := strings.Join(segments, "/")
	if !upstream.AllowsRepo(repo) {
		// As for a missing repository, so a prober learns nothing about
		// the allowlist.
		return req, http.StatusNotFound, "repository not found"
	}
	req.key = upstream.dir + "/" + repo + ".git"
	req.repoURL = upstream.URL + "/" + strings.Join(escaped, "/") + suffix
	req.fetchWants = upstream.fetchWants
	return req, 0, ""
}

// validGitSegment checks one segment of a repository path, which becomes both
// part of a directory under the cache and part of the upstream URL. Only the
// last segment may end in .git, so one repository's directory never sits
// inside another's.
func validGitSegment(segment string, last bool) bool {
	if segment == "" || segment == "." || segment == ".." || (!last && strings.HasSuffix(segment, ".git")) {
		return false
	}
	for _, r := range segment {
		if r < ' ' || r == 0x7f || r == '\\' {
			return false
		}
	}
	return true
}

// serveError answers a request whose repository could not be opened. The
// cache has already logged why.
func (h *GitHandler) serveError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	switch {
	case errors.Is(err, gitcache.ErrNotFound):
		http.Error(w, "repository not found", http.StatusNotFound)
	case errors.Is(err, gitcache.ErrRefused):
		http.Error(w, "upstream refused the request", http.StatusBadGateway)
	case errors.Is(err, gitcache.ErrTooLarge):
		http.Error(w, "repository is larger than git.max_repo_size or git.max_size", http.StatusInsufficientStorage)
	case errors.Is(err, gitcache.ErrCacheFull):
		http.Error(w, "git cache is full", http.StatusInsufficientStorage)
	default:
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
}
