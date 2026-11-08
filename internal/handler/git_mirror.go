package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"

	"github.com/git-pkgs/proxy/internal/gitcache"
)

// maxSubmoduleDepth bounds how deeply Mirror follows submodules of
// submodules.
const maxSubmoduleDepth = 8

// Mirror caches the repository at location, a host and path such as
// github.com/acme/widgets as a pkg:git PURL names it, from the upstream.git
// host serving it. With version, a full commit ID or a tag, it caches that
// commit too, and the commits its submodules pin, from the upstream.git
// hosts serving them. A submodule no upstream.git host serves is skipped.
// Clones through /git are then served from those copies.
func (h *GitHandler) Mirror(ctx context.Context, location, version string) (gitcache.MirrorResult, error) {
	return h.mirror(ctx, location, version, map[string]bool{}, 0)
}

func (h *GitHandler) mirror(ctx context.Context, location, version string, seen map[string]bool, depth int) (gitcache.MirrorResult, error) {
	repo, err := h.mirrorRepository(location)
	if err != nil {
		return gitcache.MirrorResult{}, err
	}
	result, err := h.cache.Mirror(ctx, repo.key, repo.url, version, repo.upstream.fetchWants)
	if err != nil {
		return result, err
	}
	for _, sub := range result.Submodules {
		subLocation, err := gitSubmoduleLocation(repo.url, sub.URL)
		if err == nil {
			_, err = h.mirrorRepository(subLocation)
		}
		if err != nil {
			h.logger().Warn("git mirror: skipping submodule", "repository", location, "submodule", sub.Path, "url", sub.URL, "error", err)
			continue
		}
		if seen[subLocation+"@"+sub.Commit] {
			continue
		}
		seen[subLocation+"@"+sub.Commit] = true
		if depth >= maxSubmoduleDepth {
			return result, fmt.Errorf("submodule %s: nested more than %d deep", sub.Path, maxSubmoduleDepth)
		}
		subResult, err := h.mirror(ctx, subLocation, sub.Commit, seen, depth+1)
		if err != nil {
			return result, fmt.Errorf("submodule %s: %w", sub.Path, err)
		}
		result.Cached = result.Cached && subResult.Cached
		result.Bytes += subResult.Bytes
	}
	return result, nil
}

func (h *GitHandler) logger() *slog.Logger {
	if h.proxy != nil && h.proxy.Logger != nil {
		return h.proxy.Logger
	}
	return slog.Default()
}

// gitSubmoduleLocation turns the URL .gitmodules gives a submodule into a
// location as Mirror takes it. A relative URL is resolved against the
// superproject's repoURL, as git does; ssh, git and scp-like URLs name the
// host and path the repository has there.
func gitSubmoduleLocation(repoURL, submoduleURL string) (string, error) {
	var host, repoPath string
	switch {
	case strings.HasPrefix(submoduleURL, "./"), strings.HasPrefix(submoduleURL, "../"):
		u, err := url.Parse(repoURL)
		if err != nil {
			return "", err
		}
		host, repoPath = u.Host, path.Join(u.Path, submoduleURL)
	case strings.Contains(submoduleURL, "://"):
		u, err := url.Parse(submoduleURL)
		if err != nil {
			return "", err
		}
		switch u.Scheme {
		case "http", "https":
			host = u.Host
		case "ssh", "git", "git+ssh", "ssh+git":
			host = u.Hostname()
		default:
			return "", fmt.Errorf("unsupported submodule URL %q", submoduleURL)
		}
		repoPath = u.Path
	default:
		// scp-like: [user@]host:path
		hostPart, p, ok := strings.Cut(submoduleURL, ":")
		if !ok || strings.Contains(hostPart, "/") {
			return "", fmt.Errorf("unsupported submodule URL %q", submoduleURL)
		}
		_, host, _ = strings.Cut(hostPart, "@")
		if host == "" {
			host = hostPart
		}
		repoPath = p
	}
	repoPath = strings.Trim(path.Clean("/"+repoPath), "/")
	if host == "" || repoPath == "" {
		return "", fmt.Errorf("unsupported submodule URL %q", submoduleURL)
	}
	return strings.ToLower(host) + "/" + repoPath, nil
}

// mirrorRepository resolves location to a repository of the upstream.git
// host whose URL it starts with, the longest such URL if several do.
func (h *GitHandler) mirrorRepository(location string) (gitRepository, error) {
	var best, bestPrefix, rest string
	for name, upstream := range h.upstreams {
		prefix := gitLocationPrefix(upstream.URL)
		path, ok := strings.CutPrefix(location, prefix)
		if prefix == "" || !ok || path == "" {
			continue
		}
		if len(prefix) > len(bestPrefix) || (len(prefix) == len(bestPrefix) && name < best) {
			best, bestPrefix, rest = name, prefix, path
		}
	}
	if best == "" {
		return gitRepository{}, fmt.Errorf("no upstream.git host serves %s", location)
	}
	repo, status, msg := h.repository(best + "/" + rest)
	if status != 0 {
		return gitRepository{}, fmt.Errorf("%s: %s", location, msg)
	}
	return repo, nil
}

// gitLocationPrefix is how a pkg:git PURL names the repositories under an
// upstream URL: its host and path, ending in a slash. Empty for a URL
// without a host.
func gitLocationPrefix(upstreamURL string) string {
	u, err := url.Parse(upstreamURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host) + strings.TrimSuffix(u.Path, "/") + "/"
}
