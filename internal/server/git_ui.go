package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/gitcache"
	"github.com/git-pkgs/proxy/internal/metrics"
)

// GitRepositoriesData is the git page: the repositories in the git cache.
type GitRepositoriesData struct {
	Layout
	Rows []GitRepositoryRow
	// Summary counts the rows and their size.
	Summary string
}

// GitRepositoryRow is one cached repository on the git page.
type GitRepositoryRow struct {
	Upstream, Repo, CloneURL string
	Size                     string
	Requests                 string
	LastChecked, LastUsed    string
	// LastError names the kind of error the last refresh failed with.
	LastError, LastErrorAt string
	// Branch, Commit, Subject and CommitAge describe the newest commit of
	// the branch HEAD names, and Refs counts branches and tags.
	Branch, Commit, Subject, CommitAge string
	Refs                               string
}

func (s *Server) handleGitRepositories(w http.ResponseWriter, r *http.Request) {
	repos := s.git.Repositories()
	state := func(key string) (gitcache.RepositoryState, bool) {
		st, err := s.git.RepositoryState(r.Context(), key)
		return st, err == nil
	}
	rows, size := gitRepositoryRows(s.cfg.BaseURL, s.cfg.Upstream.Git, repos, state)
	data := GitRepositoriesData{
		Layout:  s.layoutFor(r),
		Rows:    rows,
		Summary: plural(len(rows), "repository", "repositories") + " in the git cache, " + formatSize(size),
	}
	if err := s.templates.Render(w, "git_repositories", data); err != nil {
		s.logger.Error("failed to render git repositories", "error", err)
	}
}

// gitRepositoryRows names each cached repository by its upstream and path,
// and gives the URL it is cloned from through the proxy. Repositories of an
// upstream no longer configured are left out. state reads what a repository
// holds, reporting false when it can't. It also returns the rows' total size.
func gitRepositoryRows(baseURL string, upstreams map[string]config.GitUpstream, repos []gitcache.RepositoryStats,
	state func(key string) (gitcache.RepositoryState, bool)) ([]GitRepositoryRow, int64) {
	names := make(map[string]string, len(upstreams))
	for name, u := range upstreams {
		names[gitcache.UpstreamDir(name, u.URL)] = name
	}
	rows := make([]GitRepositoryRow, 0, len(repos))
	var size int64
	for _, repo := range repos {
		dir, path, _ := strings.Cut(repo.Key, "/")
		name, ok := names[dir]
		if !ok {
			continue
		}
		requests := fmt.Sprintf("%d requests", repo.Requests)
		if repo.Requests == 1 {
			requests = "1 request"
		}
		row := GitRepositoryRow{
			Upstream:    name,
			Repo:        strings.TrimSuffix(path, ".git"),
			CloneURL:    strings.TrimSuffix(baseURL, "/") + "/git/" + name + "/" + path,
			Size:        formatSize(repo.Size),
			Requests:    requests,
			LastChecked: formatTimeAgo(repo.LastChecked),
			LastUsed:    formatTimeAgo(repo.LastUsed),
			LastError:   repo.LastError,
			LastErrorAt: formatTimeAgo(repo.LastErrorAt),
		}
		if st, ok := state(repo.Key); ok {
			row.Branch, row.Subject, row.CommitAge = st.Branch, st.Subject, formatTimeAgo(st.CommitTime)
			row.Commit = st.Commit[:min(len(st.Commit), shortCommitLength)]
			row.Refs = plural(st.Branches, "branch", "branches") + ", " + plural(st.Tags, "tag", "tags")
		}
		rows = append(rows, row)
		size += repo.Size
	}
	return rows, size
}

// shortCommitLength is how much of a commit ID the git page shows.
const shortCommitLength = 7

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// gitEcosystemStats is the git cache's row of the per-ecosystem gauges: its
// repositories as artifacts, and their size.
func gitEcosystemStats(repos []gitcache.RepositoryStats) metrics.EcosystemStats {
	stats := metrics.EcosystemStats{Ecosystem: "git", Artifacts: int64(len(repos))}
	for _, r := range repos {
		stats.CacheSize += r.Size
	}
	return stats
}
