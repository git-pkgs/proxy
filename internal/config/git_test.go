package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestGitUpstreamAcceptsURLOrObject(t *testing.T) {
	want := map[string]GitUpstream{
		"github":    {URL: "https://github.com"},
		"wikimedia": {URL: "https://gitlab.wikimedia.org", Repos: []string{"mediawiki/*"}},
	}

	var fromYAML struct {
		Git map[string]GitUpstream `yaml:"git"`
	}
	err := yaml.Unmarshal([]byte(`
git:
  github: "https://github.com"
  wikimedia:
    url: "https://gitlab.wikimedia.org"
    repos: ["mediawiki/*"]
`), &fromYAML)
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}

	var fromJSON struct {
		Git map[string]GitUpstream `json:"git"`
	}
	err = json.Unmarshal([]byte(`{"git": {
		"github": "https://github.com",
		"wikimedia": {"url": "https://gitlab.wikimedia.org", "repos": ["mediawiki/*"]}
	}}`), &fromJSON)
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}

	for format, got := range map[string]map[string]GitUpstream{"YAML": fromYAML.Git, "JSON": fromJSON.Git} {
		for name, w := range want {
			if g := got[name]; g.URL != w.URL || !slices.Equal(g.Repos, w.Repos) {
				t.Errorf("%s %s = %+v, want %+v", format, name, g, w)
			}
		}
	}
}

func TestGitUpstreamAllowsRepo(t *testing.T) {
	open := GitUpstream{URL: "https://github.com"}
	if !open.AllowsRepo("any/repo") {
		t.Error("an upstream without repos refused a repository")
	}
	listed := GitUpstream{URL: "https://gitlab.example", Repos: []string{"mediawiki/*", "tools/one", "group/**"}}
	for repo, want := range map[string]bool{
		"mediawiki/core":            true,
		"tools/one":                 true,
		"tools/two":                 false,
		"mediawiki/extensions/Echo": false,
		"group/sub/project":         true,
		"group/project":             true,
		"group":                     false,
		"groups/project":            false,
	} {
		if got := listed.AllowsRepo(repo); got != want {
			t.Errorf("AllowsRepo(%q) = %v, want %v", repo, got, want)
		}
	}
}

// GitHub and Bitbucket ignore the case of repository paths, and a
// pkg:github or pkg:bitbucket PURL lowercases them, so repos patterns match
// there whatever the case. Other hosts may tell the cases apart.
func TestGitUpstreamAllowsRepoIgnoresCaseOnForges(t *testing.T) {
	for _, tt := range []struct {
		url, repo string
		want      bool
	}{
		{"https://github.com", "azure/login", true},
		{"https://github.com", "Azure/Login", true},
		{"https://GitHub.com/", "AZURE/login", true},
		{"https://github.com:443", "azure/login", true},
		{"https://bitbucket.org", "azure/login", true},
		{"https://github.com", "datadog/login", false},
		{"https://gitlab.example", "azure/login", false},
		{"https://gitlab.example", "Azure/login", true},
	} {
		g := GitUpstream{URL: tt.url, Repos: []string{"Azure/*"}}
		if got := g.AllowsRepo(tt.repo); got != tt.want {
			t.Errorf("%s: AllowsRepo(%q) = %v, want %v", tt.url, tt.repo, got, tt.want)
		}
	}
}

// The cache names a repository of GitHub or Bitbucket in lowercase, so a
// clone of Azure/login is served what the mirror cached for azure/login.
func TestGitUpstreamCacheRepo(t *testing.T) {
	for _, tt := range []struct{ url, repo, want string }{
		{"https://github.com", "Azure/Login", "azure/login"},
		{"https://bitbucket.org/", "Acme/Widgets", "acme/widgets"},
		{"https://gitlab.example", "Acme/Widgets", "Acme/Widgets"},
		{"https://github.example", "Acme/Widgets", "Acme/Widgets"},
	} {
		if got := (GitUpstream{URL: tt.url}).CacheRepo(tt.repo); got != tt.want {
			t.Errorf("%s: CacheRepo(%q) = %q, want %q", tt.url, tt.repo, got, tt.want)
		}
	}
}

func TestValidateGitUpstreams(t *testing.T) {
	tests := []struct {
		name     string
		upstream UpstreamConfig
		wantErr  bool
	}{
		{name: "public", upstream: UpstreamConfig{Git: map[string]GitUpstream{"github": {URL: "https://github.com"}}}},
		{name: "relative URL", upstream: UpstreamConfig{Git: map[string]GitUpstream{"github": {URL: "github.com"}}}, wantErr: true},
		{name: "name with slash", upstream: UpstreamConfig{Git: map[string]GitUpstream{"a/b": {URL: "https://github.com"}}}, wantErr: true},
		{name: "name with leading dot", upstream: UpstreamConfig{Git: map[string]GitUpstream{".tmp": {URL: "https://github.com"}}}, wantErr: true},
		{name: "bad pattern", upstream: UpstreamConfig{Git: map[string]GitUpstream{"github": {URL: "https://github.com", Repos: []string{"["}}}}, wantErr: true},
		{name: "nested pattern", upstream: UpstreamConfig{Git: map[string]GitUpstream{"gerrit": {URL: "https://gerrit.example/r", Repos: []string{"mediawiki/**"}}}}},
		{name: "ssh URL", upstream: UpstreamConfig{Git: map[string]GitUpstream{"github": {URL: "ssh://git@github.com"}}}, wantErr: true},
		{
			name: "credentials without repos",
			upstream: UpstreamConfig{
				Git:  map[string]GitUpstream{"private": {URL: "https://git.example"}},
				Auth: map[string]AuthConfig{"https://git.example": {Type: "bearer", Token: "t"}},
			},
			wantErr: true,
		},
		{
			name: "credentials for a path under the upstream",
			upstream: UpstreamConfig{
				Git:  map[string]GitUpstream{"private": {URL: "https://git.example"}},
				Auth: map[string]AuthConfig{"https://git.example/team": {Type: "bearer", Token: "t"}},
			},
			wantErr: true,
		},
		{
			name: "credentials with repos",
			upstream: UpstreamConfig{
				Git:  map[string]GitUpstream{"private": {URL: "https://git.example", Repos: []string{"team/*"}}},
				Auth: map[string]AuthConfig{"https://git.example": {Type: "bearer", Token: "t"}},
			},
		},
		{
			name: "credentials for another host",
			upstream: UpstreamConfig{
				Git:  map[string]GitUpstream{"github": {URL: "https://github.com"}},
				Auth: map[string]AuthConfig{"https://npm.example": {Type: "bearer", Token: "t"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.upstream.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGitConfigDefaults(t *testing.T) {
	var g GitConfig
	if err := g.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if g.CacheDir != defaultGitCacheDir || g.MaxSizeBytes() != 0 || g.MaxRepoSizeBytes() != 5<<30 ||
		g.RefreshWaitDuration() != 5*time.Second || g.StaleMaxAgeDuration() != 24*time.Hour ||
		g.WarmMaxIdleDuration() != 30*24*time.Hour {
		t.Errorf("defaults = %+v", g)
	}

	for value, want := range map[string]time.Duration{"90d": 90 * 24 * time.Hour, "12h": 12 * time.Hour, "0": 0} {
		g := GitConfig{WarmMaxIdle: value}
		if err := g.Validate(); err != nil || g.WarmMaxIdleDuration() != want {
			t.Errorf("warm_max_idle %q = %v, %v; want %v", value, g.WarmMaxIdleDuration(), err, want)
		}
	}

	for _, bad := range []GitConfig{{MaxSize: "lots"}, {RefreshWait: "soon"}, {StaleMaxAge: "-1h"}, {WarmMaxIdle: "-1d"}, {WarmMaxIdle: "often"}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate(%+v) accepted it", bad)
		}
	}
}

func TestLoadGitConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
upstream:
  git:
    github: https://github.com
git:
  cache_dir: /data/git
  max_size: 50GB
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Setenv("PROXY_GIT_REFRESH_WAIT", "2s")
	cfg.LoadFromEnv()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Upstream.Git["github"].URL != "https://github.com" || cfg.Git.CacheDir != "/data/git" ||
		cfg.Git.MaxSizeBytes() != 50<<30 || cfg.Git.RefreshWaitDuration() != 2*time.Second {
		t.Errorf("loaded %+v and %+v", cfg.Upstream.Git, cfg.Git)
	}
}

func TestLoadGitOffline(t *testing.T) {
	load := func(t *testing.T, data string) *Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		cfg.LoadFromEnv()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		return cfg
	}
	upstreams := `
upstream:
  git:
    github: https://github.com
    internal:
      url: https://git.example.com
      offline: true
`
	cfg := load(t, upstreams)
	if cfg.Upstream.Git["github"].Offline || !cfg.Upstream.Git["internal"].Offline {
		t.Errorf("per-upstream offline: %+v", cfg.Upstream.Git)
	}

	cfg = load(t, upstreams+"git:\n  offline: true\n")
	if !cfg.Upstream.Git["github"].Offline || !cfg.Upstream.Git["internal"].Offline {
		t.Errorf("git.offline left an upstream online: %+v", cfg.Upstream.Git)
	}

	t.Setenv("PROXY_GIT_OFFLINE", "true")
	cfg = load(t, upstreams)
	if !cfg.Git.Offline || !cfg.Upstream.Git["github"].Offline {
		t.Errorf("PROXY_GIT_OFFLINE left an upstream online: %+v", cfg.Upstream.Git)
	}
}
