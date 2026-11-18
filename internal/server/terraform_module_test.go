package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
)

// TestTerraformModuleIsClonedThroughGit asks the whole server for a module
// whose registry points at a Git repository of an upstream.git host, and
// clones the location it answers with, also once the upstream is down.
func TestTerraformModuleIsClonedThroughGit(t *testing.T) {
	root := t.TempDir()
	head := newGitTestRepo(t, root, "team/vpc")
	gitUpstream := httptest.NewServer(gitBackend(t, root))
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/terraform.json":
			_, _ = w.Write([]byte(`{"modules.v1":"/v1/modules/"}`))
		case "/v1/modules/team/vpc/aws/1.0.0/download":
			w.Header().Set("X-Terraform-Get", "git::"+gitUpstream.URL+"/team/vpc.git?ref="+head)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()

	cfg, _ := startGitTestServer(t, func(cfg *config.Config) {
		cfg.Upstream.Git = map[string]config.GitUpstream{"local": {URL: gitUpstream.URL}}
		cfg.Upstream.Terraform = map[string]string{"registry.example": registry.URL}
	})

	resp, err := http.Get(cfg.BaseURL + "/terraform/modules/registry.example/team/vpc/aws/1.0.0/download")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	location := resp.Header.Get("X-Terraform-Get")
	want := "git::" + cfg.BaseURL + "/git/local/team/vpc.git?ref=" + head
	if resp.StatusCode != http.StatusNoContent || location != want {
		t.Fatalf("download = %d %q, want 204 %q", resp.StatusCode, location, want)
	}

	repoURL, _, _ := strings.Cut(strings.TrimPrefix(location, "git::"), "?")
	for _, attempt := range []string{"first", "offline"} {
		clone := filepath.Join(t.TempDir(), "clone")
		gitCommand(t, "", "clone", "--quiet", repoURL, clone)
		gitCommand(t, clone, "checkout", "--quiet", head)
		if got := gitCommand(t, clone, "rev-parse", "HEAD"); got != head {
			t.Errorf("%s clone: HEAD = %s, want %s", attempt, got, head)
		}
		gitUpstream.Close()
	}
}

// The registry's version list is passed on.
func TestTerraformModuleVersionsThroughServer(t *testing.T) {
	const versions = `{"modules":[{"versions":[{"version":"1.0.0"}]}]}`
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/terraform.json":
			_, _ = w.Write([]byte(`{"modules.v1":"/v1/modules/"}`))
		case "/v1/modules/team/vpc/aws/versions":
			_, _ = w.Write([]byte(versions))
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()
	cfg, _ := startGitTestServer(t, func(cfg *config.Config) {
		cfg.Upstream.Terraform = map[string]string{"registry.example": registry.URL}
	})

	resp, err := http.Get(cfg.BaseURL + "/terraform/modules/registry.example/team/vpc/aws/versions")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK || string(got) != versions {
		t.Errorf("versions = %d %s %v, want %s", resp.StatusCode, got, err, versions)
	}
}
