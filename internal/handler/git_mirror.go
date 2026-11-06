package handler

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/git-pkgs/proxy/internal/gitcache"
)

// Mirror caches the repository at location, a host and path such as
// github.com/acme/widgets as a pkg:git PURL names it, from the upstream.git
// host serving it, and with commit, a full commit ID, that commit too.
// Clones through /git are then served from that copy.
func (h *GitHandler) Mirror(ctx context.Context, location, commit string) (gitcache.MirrorResult, error) {
	repo, err := h.mirrorRepository(location)
	if err != nil {
		return gitcache.MirrorResult{}, err
	}
	return h.cache.Mirror(ctx, repo.key, repo.url, commit, repo.upstream.fetchWants)
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
