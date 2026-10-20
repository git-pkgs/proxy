package config

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultGitCacheDir    = "./cache/git"
	defaultGitMaxRepoSize = "5GB"
	defaultGitRefreshWait = "5s"
	defaultGitStaleMaxAge = "24h"
)

// GitUpstream is an upstream.git entry: a Git server's base URL and,
// optionally, the repositories under it that may be served. In YAML or JSON
// it is either the URL alone or an object with url and repos.
type GitUpstream struct {
	URL string `json:"url" yaml:"url"`

	// Repos lists path.Match patterns for repository paths under URL, such
	// as "mediawiki/*". A pattern ending in "/**" also matches every
	// repository nested below its prefix: "mediawiki/**" matches
	// mediawiki/skins/Vector. Empty allows every repository. Required when
	// upstream.auth has credentials for the URL, since everyone who can
	// reach the proxy can read what it serves.
	Repos []string `json:"repos" yaml:"repos"`
}

// UnmarshalYAML accepts a bare URL as well as the object form.
func (g *GitUpstream) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&g.URL)
	}
	type plain GitUpstream
	return node.Decode((*plain)(g))
}

// UnmarshalJSON accepts a bare URL as well as the object form.
func (g *GitUpstream) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &g.URL); err == nil {
		return nil
	}
	type plain GitUpstream
	return json.Unmarshal(data, (*plain)(g))
}

// AllowsRepo reports whether repo, a path under URL, may be served.
func (g GitUpstream) AllowsRepo(repo string) bool {
	if len(g.Repos) == 0 {
		return true
	}
	for _, pattern := range g.Repos {
		if matchRepoPattern(pattern, repo) {
			return true
		}
	}
	return false
}

func matchRepoPattern(pattern, repo string) bool {
	prefix, nested := strings.CutSuffix(pattern, "/**")
	if !nested {
		ok, _ := path.Match(pattern, repo)
		return ok
	}
	segments := strings.Count(prefix, "/") + 1
	parts := strings.SplitN(repo, "/", segments+1)
	if len(parts) <= segments {
		return false
	}
	ok, _ := path.Match(prefix, strings.Join(parts[:segments], "/"))
	return ok
}

// GitConfig configures the Git repository cache behind /git/{name}/.
type GitConfig struct {
	// CacheDir holds the cached bare repositories. It must be local disk
	// used by this process only, which locks it. Default: ./cache/git.
	CacheDir string `json:"cache_dir" yaml:"cache_dir"`

	// MaxSize bounds the cache, counting fetches in progress; the least
	// recently used repositories not in use are evicted to stay within it.
	// Empty or "0" disables the limit.
	MaxSize string `json:"max_size" yaml:"max_size"`

	// MaxRepoSize is the most one repository may take; a clone or fetch that
	// would pass it fails. "0" disables the limit. Default: 5GB. MaxSize
	// caps it when set.
	MaxRepoSize string `json:"max_repo_size" yaml:"max_repo_size"`

	// RefreshWait is how long a request waits for the upstream's branches
	// and tags to be checked before it is served the cached copy. A fetch of
	// new commits the check finds is waited for in full. Default: 5s.
	RefreshWait string `json:"refresh_wait" yaml:"refresh_wait"`

	// StaleMaxAge is how long after the last successful upstream check a
	// cached copy may still be served when the upstream fails or is slow.
	// Default: 24h.
	StaleMaxAge string `json:"stale_max_age" yaml:"stale_max_age"`

	maxSize     int64
	maxRepoSize int64
	refreshWait time.Duration
	staleMaxAge time.Duration
}

// MaxSizeBytes returns the parsed MaxSize, 0 for no limit. Validate must run first.
func (g *GitConfig) MaxSizeBytes() int64 { return g.maxSize }

// MaxRepoSizeBytes returns the parsed MaxRepoSize, 0 for no limit. Validate must run first.
func (g *GitConfig) MaxRepoSizeBytes() int64 { return g.maxRepoSize }

// RefreshWaitDuration returns the parsed RefreshWait. Validate must run first.
func (g *GitConfig) RefreshWaitDuration() time.Duration { return g.refreshWait }

// StaleMaxAgeDuration returns the parsed StaleMaxAge. Validate must run first.
func (g *GitConfig) StaleMaxAgeDuration() time.Duration { return g.staleMaxAge }

// Validate checks the Git cache settings, applying defaults where unset.
func (g *GitConfig) Validate() error {
	if g.CacheDir == "" {
		g.CacheDir = defaultGitCacheDir
	}
	if g.MaxRepoSize == "" {
		g.MaxRepoSize = defaultGitMaxRepoSize
	}
	if g.RefreshWait == "" {
		g.RefreshWait = defaultGitRefreshWait
	}
	if g.StaleMaxAge == "" {
		g.StaleMaxAge = defaultGitStaleMaxAge
	}

	var err error
	if g.maxSize, err = parseNonNegativeSize("git.max_size", g.MaxSize); err != nil {
		return err
	}
	if g.maxRepoSize, err = parseNonNegativeSize("git.max_repo_size", g.MaxRepoSize); err != nil {
		return err
	}
	if g.refreshWait, err = parseNonNegativeDuration("git.refresh_wait", g.RefreshWait); err != nil {
		return err
	}
	if g.staleMaxAge, err = parseNonNegativeDuration("git.stale_max_age", g.StaleMaxAge); err != nil {
		return err
	}
	return nil
}

func parseNonNegativeSize(field, value string) (int64, error) {
	size, err := ParseSize(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", field, err)
	}
	if size < 0 {
		return 0, fmt.Errorf("invalid %s %q: must not be negative", field, value)
	}
	return size, nil
}

func parseNonNegativeDuration(field, value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", field, value, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid %s %q: must not be negative", field, value)
	}
	return d, nil
}

// validateGitUpstreams checks upstream.git names, URLs and repository
// patterns. An upstream that upstream.auth has credentials for must list its
// repositories, so configuring a token does not open every repository it can
// read to everyone who can reach the proxy.
func (u *UpstreamConfig) validateGitUpstreams() error {
	for name, upstream := range u.Git {
		field := "upstream.git." + name
		if name == "" || name == "." || name == ".." || name[0] == '.' || strings.ContainsAny(name, `/\\`) {
			return fmt.Errorf("invalid upstream.git name %q", name)
		}
		if err := validateAbsoluteURL(field, upstream.URL); err != nil {
			return err
		}
		if scheme, _, _ := strings.Cut(upstream.URL, "://"); !strings.EqualFold(scheme, "https") && !strings.EqualFold(scheme, "http") {
			return fmt.Errorf("invalid %s %q: only http and https are supported", field, upstream.URL)
		}
		for _, pattern := range upstream.Repos {
			if _, err := path.Match(strings.TrimSuffix(pattern, "/**"), ""); pattern == "" || err != nil {
				return fmt.Errorf("invalid %s.repos pattern %q", field, pattern)
			}
		}
		if len(upstream.Repos) == 0 && u.HasAuthWithin(upstream.URL) {
			return fmt.Errorf("%s needs repos: upstream.auth has credentials for %s, "+
				"and everyone who can reach the proxy could read every repository they allow", field, upstream.URL)
		}
	}
	return nil
}

// HasAuthWithin reports whether an upstream.auth scope covers baseURL or a
// path under it.
func (u *UpstreamConfig) HasAuthWithin(baseURL string) bool {
	base, err := parseAuthURL(baseURL)
	if err != nil {
		return false
	}
	for scope := range u.Auth {
		configured, err := parseAuthURL(scope)
		if err != nil {
			continue
		}
		if authURLMatches(configured, base) || authURLMatches(base, configured) {
			return true
		}
	}
	return false
}
