package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/git-pkgs/purl"

	"github.com/git-pkgs/proxy/internal/gitcache"
)

// GitMirrorer caches Git repositories for pkg:git items. location is the
// repository's host and path, such as codeberg.org/forgejo/forgejo, and
// version a full commit ID or a tag, or empty to cache the repository's
// branches and tags only.
type GitMirrorer interface {
	Mirror(ctx context.Context, location, version string) (gitcache.MirrorResult, error)
}

// UnavailableGit fails every pkg:git item with Reason.
type UnavailableGit struct {
	Reason string
}

func (u UnavailableGit) Mirror(context.Context, string, string) (gitcache.MirrorResult, error) {
	return gitcache.MirrorResult{}, errors.New(u.Reason)
}

// GitListing is implemented by a GitMirrorer that can tell which
// repositories an upstream.git repos list names.
type GitListing interface {
	// ListsRepository reports whether location is served by an upstream.git
	// host whose repos list names it.
	ListsRepository(location string) bool
}

var errNoGitUpstream = errors.New("git repositories are mirrored from upstream.git, and none is configured")

// forgeHosts are the hosts of the PURL types that name a repository on a
// forge.
var forgeHosts = map[string]string{"github": "github.com", "bitbucket": "bitbucket.org"}

// forgeGitRepository returns the repository, as pkg:git names it, and the
// version a pkg:github, pkg:bitbucket or pkg:generic PURL with a git
// vcs_url names. A generic package's version needn't be a ref, so only one
// given in its vcs_url is used.
func forgeGitRepository(p *purl.PURL) (location, version string, ok bool) {
	if host, ok := forgeHosts[p.Type]; ok && p.Namespace != "" {
		return host + "/" + p.Namespace + "/" + p.Name, p.Version, true
	}
	if p.Type != "generic" {
		return "", "", false
	}
	return vcsGitRepository(p.Qualifier("vcs_url"))
}

// vcsGitRepository reads a git vcs_url, such as
// git+https://github.com/acme/widgets.git@v1.0, as a repository and the ref
// after its last unescaped "@", if any.
func vcsGitRepository(vcsURL string) (location, ref string, ok bool) {
	rest, ok := strings.CutPrefix(vcsURL, "git+")
	if !ok {
		return "", "", false
	}
	u, err := url.Parse(rest)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	// The host and port as an upstream.git URL gives them; an SSH or git
	// port is not the HTTP one.
	host := u.Host
	switch u.Scheme {
	case "https", "http":
	case "ssh", "git":
		host = u.Hostname()
		if strings.Contains(host, ":") {
			host = "[" + host + "]" // IPv6, as an upstream.git URL writes it
		}
	default:
		return "", "", false
	}
	// The ref follows a literal "@"; an escaped one is part of the path.
	repoPath := u.EscapedPath()
	if i := strings.LastIndex(repoPath, "@"); i >= 0 {
		repoPath, ref = repoPath[:i], repoPath[i+1:]
	}
	// url.Parse checked the escapes, so neither fails.
	repoPath, _ = url.PathUnescape(repoPath)
	ref, _ = url.PathUnescape(ref)
	// A ".git" suffix is kept, as pkg:git keeps it, for an upstream that
	// serves the repository only under that name.
	repoPath = strings.Trim(path.Clean("/"+repoPath), "/")
	if repoPath == "" {
		return "", "", false
	}
	return strings.ToLower(host) + "/" + repoPath, ref, true
}

// mirrorGit caches a pkg:git repository, and the commit its version names
// with the submodules it pins, in the cache /git serves clones from.
func (m *Mirror) mirrorGit(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	if m.Git == nil {
		m.recordFailure(tracker, pv, errNoGitUpstream)
		return
	}
	if pv.listedOnly {
		if listing, ok := m.Git.(GitListing); !ok || !listing.ListsRepository(pv.Name) {
			m.recordFailure(tracker, pv, fmt.Errorf("%s: pkg:github, pkg:bitbucket and vcs_url PURLs are mirrored only for repositories an upstream.git repos list names", pv.Name))
			return
		}
	}
	result, err := m.Git.Mirror(ctx, pv.Name, pv.Version)
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	if result.Cached {
		tracker.skipped.Add(1)
		m.logger.Debug("already cached", "ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version)
		return
	}
	tracker.completed.Add(1)
	tracker.bytes.Add(result.Bytes)
	m.logger.Info("mirrored", "ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version, "size", result.Bytes)
}
