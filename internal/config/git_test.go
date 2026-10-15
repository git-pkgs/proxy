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
		g.RefreshWaitDuration() != 5*time.Second || g.StaleMaxAgeDuration() != 24*time.Hour {
		t.Errorf("defaults = %+v", g)
	}

	for _, bad := range []GitConfig{{MaxSize: "lots"}, {RefreshWait: "soon"}, {StaleMaxAge: "-1h"}} {
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
