package mirror

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/git-pkgs/purl"
)

// listingGit is fakeGit whose upstream.git repos lists name listed.
type listingGit struct {
	fakeGit
	listed map[string]bool
}

func (g *listingGit) ListsRepository(location string) bool { return g.listed[location] }

// pkg:github, pkg:bitbucket and pkg:generic with a git vcs_url name a
// repository on a forge, which is mirrored into the git cache as pkg:git
// names it.
func TestMirrorForgePURLsIntoTheGitCache(t *testing.T) {
	vcs := url.QueryEscape
	tests := []struct {
		purl string
		want gitCall
	}{
		{"pkg:github/actions/checkout@v4", gitCall{"github.com/actions/checkout", "v4"}},
		{"pkg:github/acme/widgets@" + gitTestCommit, gitCall{"github.com/acme/widgets", gitTestCommit}},
		// A GitHub Action in a subdirectory of its repository.
		{"pkg:github/acme/widgets@v1#setup", gitCall{"github.com/acme/widgets", "v1"}},
		{"pkg:github/acme/widgets", gitCall{"github.com/acme/widgets", ""}},
		{"pkg:bitbucket/acme/widgets@" + gitTestCommit, gitCall{"bitbucket.org/acme/widgets", gitTestCommit}},
		{"pkg:generic/widgets@1.0?vcs_url=" + vcs("git+https://git.example/acme/widgets.git@"+gitTestCommit), gitCall{"git.example/acme/widgets", gitTestCommit}},
		// The version of a generic package needn't name a ref, so without
		// one in vcs_url the repository is mirrored without a commit.
		{"pkg:generic/widgets@1.0?vcs_url=" + vcs("git+https://git.example/acme/widgets"), gitCall{"git.example/acme/widgets", ""}},
		{"pkg:generic/widgets@1.0?vcs_url=" + vcs("git+ssh://git@git.example/acme/widgets.git"), gitCall{"git.example/acme/widgets", ""}},
		// An HTTP port is part of the location, as it is of an
		// upstream.git URL; an SSH port isn't.
		{"pkg:generic/widgets@1.0?vcs_url=" + vcs("git+https://git.example:8443/acme/widgets"), gitCall{"git.example:8443/acme/widgets", ""}},
		{"pkg:generic/widgets@1.0?vcs_url=" + vcs("git+ssh://git@git.example:2222/acme/widgets"), gitCall{"git.example/acme/widgets", ""}},
		{"pkg:generic/widgets@1.0?vcs_url=" + vcs("git+https://Git.Example/acme/widgets/"), gitCall{"git.example/acme/widgets", ""}},
	}
	for _, tt := range tests {
		t.Run(strings.ReplaceAll(tt.purl, "#", "_"), func(t *testing.T) {
			m := setupTestMirror(t, 1)
			git := &listingGit{listed: map[string]bool{tt.want.location: true}}
			m.Git = git
			versions := &fakeVersions{}
			progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{tt.purl}, Versions: versions})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if progress.Failed != 0 || len(git.calls) != 1 || git.calls[0] != tt.want {
				t.Errorf("progress = %+v, calls = %+v; want one call %+v", progress, git.calls, tt.want)
			}
			if len(versions.asked) != 0 {
				t.Errorf("listed versions of %v", versions.asked)
			}
		})
	}
}

// A forge PURL is mirrored only for a repository an upstream.git repos list
// names, so the GitHub Actions in an SBOM aren't all cloned. Others fail.
func TestMirrorForgePURLNeedsAReposList(t *testing.T) {
	for _, p := range []string{
		"pkg:github/actions/checkout@v4",
		"pkg:bitbucket/acme/widgets@" + gitTestCommit,
		"pkg:generic/widgets@1.0?vcs_url=" + url.QueryEscape("git+https://git.example/acme/widgets"),
	} {
		t.Run(p, func(t *testing.T) {
			m := setupTestMirror(t, 1)
			git := &listingGit{}
			m.Git = git
			progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{p}})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, "repos") || len(git.calls) != 0 {
				t.Errorf("progress = %+v, calls = %+v; want a failure naming repos and no call", progress, git.calls)
			}
		})
	}
}

// A Git mirrorer that can't tell which repositories are listed mirrors no
// forge PURL.
func TestMirrorForgePURLWithoutListing(t *testing.T) {
	m := setupTestMirror(t, 1)
	git := &fakeGit{}
	m.Git = git
	progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:github/actions/checkout@v4"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if progress.Failed != 1 || len(git.calls) != 0 {
		t.Errorf("progress = %+v, calls = %+v; want a failure and no call", progress, git.calls)
	}
}

// pkg:git keeps mirroring every repository its upstream.git serves, listed
// or not, and a generic PURL whose vcs_url isn't git isn't mirrored as git.
func TestMirrorGitPURLsNeedNoReposList(t *testing.T) {
	m := setupTestMirror(t, 1)
	git := &listingGit{}
	m.Git = git
	progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{
		"pkg:git/github.com/acme/widgets@" + gitTestCommit,
		"pkg:generic/widgets@1.0?vcs_url=" + url.QueryEscape("svn+https://svn.example/widgets"),
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(git.calls) != 1 || git.calls[0] != (gitCall{"github.com/acme/widgets", gitTestCommit}) {
		t.Errorf("progress = %+v, calls = %+v", progress, git.calls)
	}
}

// Package URLs that don't name a git repository aren't mirrored as one.
func TestForgeGitRepositoryRefuses(t *testing.T) {
	for _, s := range []string{
		"pkg:github/widgets@v1",
		"pkg:npm/widgets@1.0?vcs_url=" + url.QueryEscape("git+https://git.example/acme/widgets"),
		"pkg:generic/widgets@1.0",
		"pkg:generic/widgets@1.0?vcs_url=" + url.QueryEscape("https://git.example/acme/widgets"),
		"pkg:generic/widgets@1.0?vcs_url=" + url.QueryEscape("git+file:///srv/acme/widgets"),
		"pkg:generic/widgets@1.0?vcs_url=" + url.QueryEscape("git+ftp://git.example/acme/widgets"),
		"pkg:generic/widgets@1.0?vcs_url=" + url.QueryEscape("git+https://git.example/"),
		"pkg:generic/widgets@1.0?vcs_url=" + url.QueryEscape("git+https:///acme/widgets"),
	} {
		p, err := purl.Parse(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if location, _, ok := forgeGitRepository(p); ok {
			t.Errorf("%s names repository %q, want none", s, location)
		}
	}
}
