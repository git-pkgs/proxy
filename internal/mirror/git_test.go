package mirror

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/git-pkgs/proxy/internal/gitcache"
)

type gitCall struct{ location, commit string }

type fakeGit struct {
	mu     sync.Mutex
	calls  []gitCall
	result gitcache.MirrorResult
	err    error
}

func (f *fakeGit) Mirror(_ context.Context, location, commit string) (gitcache.MirrorResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, gitCall{location, commit})
	return f.result, f.err
}

const gitTestCommit = "a72d2c07cfca03b55371089de6aa230d8c951fa0"

func TestMirrorRunCountsGitRepositories(t *testing.T) {
	tests := []struct {
		name                                   string
		result                                 gitcache.MirrorResult
		err                                    error
		wantCompleted, wantSkipped, wantFailed int64
	}{
		{name: "fetched", result: gitcache.MirrorResult{Bytes: 42}, wantCompleted: 1},
		{name: "cached", result: gitcache.MirrorResult{Cached: true}, wantSkipped: 1},
		{name: "failed", err: errors.New("boom"), wantFailed: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := setupTestMirror(t, 1)
			git := &fakeGit{result: tt.result, err: tt.err}
			m.Git = git
			progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:git/codeberg.org/forgejo/forgejo@" + gitTestCommit}})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if progress.Completed != tt.wantCompleted || progress.Skipped != tt.wantSkipped || progress.Failed != tt.wantFailed {
				t.Errorf("progress = %+v", progress)
			}
			if progress.Bytes != tt.result.Bytes {
				t.Errorf("bytes = %d, want %d", progress.Bytes, tt.result.Bytes)
			}
			want := gitCall{"codeberg.org/forgejo/forgejo", gitTestCommit}
			if len(git.calls) != 1 || git.calls[0] != want {
				t.Errorf("calls = %+v, want %+v", git.calls, want)
			}
		})
	}
}

// TestMirrorRunGitRepositoryWithoutCommit checks a pkg:git PURL without a
// version mirrors the repository instead of listing versions.
func TestMirrorRunGitRepositoryWithoutCommit(t *testing.T) {
	m := setupTestMirror(t, 1)
	git := &fakeGit{}
	m.Git = git
	versions := &fakeVersions{err: errors.New("listed versions")}
	progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:git/codeberg.org/forgejo/forgejo"}, Versions: versions})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(versions.asked) != 0 {
		t.Errorf("listed versions of %v", versions.asked)
	}
	if progress.Failed != 0 || len(git.calls) != 1 || git.calls[0] != (gitCall{"codeberg.org/forgejo/forgejo", ""}) {
		t.Errorf("progress = %+v, calls = %+v", progress, git.calls)
	}
}

func TestMirrorRunGitWithoutUpstream(t *testing.T) {
	m := setupTestMirror(t, 1)
	progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:git/codeberg.org/forgejo/forgejo@" + gitTestCommit}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if progress.Failed != 1 {
		t.Errorf("progress = %+v, want one failure", progress)
	}
}
