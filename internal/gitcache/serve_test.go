package gitcache

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestRequestBodies sends the bodies git sends for large requests: gzipped,
// or chunked once one outgrows http.postBuffer.
func TestRequestBodies(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")

	lsRefs := pktLine("command=ls-refs\n") + pktDelim + pktLine("ref-prefix refs/heads/\n") + pktFlush
	var gzipped bytes.Buffer
	gz := gzip.NewWriter(&gzipped)
	_, _ = io.WriteString(gz, lsRefs)
	_ = gz.Close()
	for _, tt := range []struct {
		name     string
		body     io.Reader
		encoding string
	}{
		// A reader of unknown length is sent chunked.
		{"chunked", io.MultiReader(strings.NewReader(lsRefs)), ""},
		{"gzip", &gzipped, "gzip"},
	} {
		req, _ := http.NewRequest(http.MethodPost, tc.url("repo")+"/git-upload-pack", tt.body)
		req.Header.Set("Content-Type", requestType)
		req.Header.Set("Git-Protocol", "version=2")
		if tt.encoding != "" {
			req.Header.Set("Content-Encoding", tt.encoding)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "refs/heads/main") {
			t.Errorf("%s: status %d, body %q", tt.name, resp.StatusCode, out)
		}
	}
}

// TestCloneWithChunkedRequests clones a repository with enough refs that
// git's requests, to the cache and from the cache to the upstream, outgrow
// http.postBuffer and are sent chunked.
func TestCloneWithChunkedRequests(t *testing.T) {
	const tags = 2000
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	var stream strings.Builder
	for i := 1; i <= tags; i++ {
		fmt.Fprintf(&stream, "commit refs/heads/main\nmark :%d\ncommitter test <test@example.com> %d +0000\ndata 1\n\n", i, i)
		if i == 1 {
			stream.WriteString("from refs/heads/main^0\n")
		}
		fmt.Fprintf(&stream, "reset refs/tags/t%d\nfrom :%d\n\n", i, i)
	}
	importer := exec.Command("git", "fast-import", "--quiet")
	importer.Dir, importer.Env, importer.Stdin = work, gitEnv(t.TempDir()), strings.NewReader(stream.String())
	if out, err := importer.CombinedOutput(); err != nil {
		t.Fatalf("git fast-import: %v\n%s", err, out)
	}
	runGit(t, work, "push", "--quiet", "origin", "main", "--tags")
	tc := newTestCache(t, upstream.server.URL, nil)

	// One want per tag is about 100KB, past a postBuffer just above git's
	// minimum.
	clone := tc.clone(t, "repo", "-c", "http.postBuffer=66000")
	if got := strings.Count(runGit(t, clone, "tag", "--list"), "\n") + 1; got != tags+1 {
		t.Errorf("cloned %d tags, want %d", got, tags+1)
	}
}

func TestParseV0Advertisement(t *testing.T) {
	oid := strings.Repeat("a", 40)
	peeled := strings.Repeat("b", 40)
	adv := pktLine(oid+" HEAD\x00multi_ack symref=HEAD:refs/heads/trunk agent=git/2\n") +
		pktLine(oid+" refs/heads/trunk\n") +
		pktLine(oid+" refs/tags/v1\n") +
		pktLine(peeled+" refs/tags/v1^{}\n") +
		pktLine(oid+" refs/changes/01/1/1\n") +
		pktLine(oid+" refs/pull/1/head\n") +
		pktFlush
	pkts := newPktReader(strings.NewReader(adv))
	kind, line, err := pkts.next()
	if err != nil {
		t.Fatal(err)
	}
	remote, err := parseV0Advertisement(pkts, kind, line)
	if err != nil {
		t.Fatal(err)
	}
	if remote.head != "refs/heads/trunk" {
		t.Errorf("head = %q", remote.head)
	}
	want := map[string]string{"refs/heads/trunk": oid, "refs/tags/v1": oid}
	if len(remote.refs) != len(want) || remote.refs["refs/heads/trunk"] != oid || remote.refs["refs/tags/v1"] != oid {
		t.Errorf("refs = %v, want %v", remote.refs, want)
	}
}

// TestCommitOnNoBranchIsFetchedByID does what actions/checkout does: fetch
// one commit by object ID, here a pull request head that is on no branch.
func TestCommitOnNoBranchIsFetchedByID(t *testing.T) {
	for _, version := range []string{"0", "2"} {
		t.Run("protocol v"+version, func(t *testing.T) {
			upstream := newFakeUpstream(t)
			work := upstream.newRepo("repo")
			runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
			runGit(t, work, "checkout", "--quiet", "-b", "pr")
			sha := upstream.commit(work, "pull request")
			runGit(t, work, "push", "--quiet", "origin", "pr:refs/pull/1/head")
			tc := newTestCache(t, upstream.server.URL, nil)
			tc.clone(t, "repo")
			sessions := upstream.gitSessions.Load()

			var wg sync.WaitGroup
			for range 4 {
				wg.Go(func() {
					dir := t.TempDir()
					runGit(t, dir, "init", "--quiet")
					if out, err := tryGit(t, dir, "-c", "protocol.version="+version, "fetch", "--quiet", "--depth=1", tc.url("repo"), sha); err != nil {
						t.Errorf("fetch by ID: %v\n%s", err, out)
						return
					}
					if got := runGit(t, dir, "rev-parse", "FETCH_HEAD"); got != sha {
						t.Errorf("FETCH_HEAD = %s, want %s", got, sha)
					}
				})
			}
			wg.Wait()
			if got := upstream.gitSessions.Load() - sessions; got != 1 {
				t.Errorf("upstream saw %d fetches for the commit, want 1", got)
			}
			if refs := runGit(t, "", "ls-remote", tc.url("repo")); strings.Contains(refs, wantedPrefix) {
				t.Errorf("advertised %s refs:\n%s", wantedPrefix, refs)
			}

			// A refresh that prunes branches keeps the commit.
			runGit(t, work, "push", "--quiet", "origin", "main:refs/heads/new")
			tc.clone(t, "repo")
			if refs := tc.cachedRefs(t); !strings.Contains(refs, wantedPrefix) || !strings.Contains(refs, "/"+sha) {
				t.Errorf("refresh dropped the fetched commit:\n%s", refs)
			}
		})
	}
}

func TestReadWants(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	tests := map[string]struct {
		body string
		want []string
	}{
		"v0": {
			pktLine("want "+a+" multi_ack side-band-64k\n") + pktLine("want "+b+"\n") + pktLine("deepen 1\n") + pktFlush + pktLine("have "+a+"\n") + pktFlush,
			[]string{a, b},
		},
		"v2 fetch": {
			pktLine("command=fetch\n") + pktLine("agent=git/2\n") + pktDelim + pktLine("thin-pack\n") + pktLine("want "+a+"\n") + pktLine("have "+b+"\n") + pktLine("want "+b+"\n") + pktLine("done\n") + pktFlush,
			[]string{a, b},
		},
		"v2 ls-refs": {
			pktLine("command=ls-refs\n") + pktDelim + pktLine("want "+a+"\n") + pktFlush,
			nil,
		},
		"not an object ID": {
			pktLine("want HEAD\n") + pktFlush,
			nil,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			wants, replay := readWants(strings.NewReader(tt.body))
			if !slices.Equal(wants, tt.want) {
				t.Errorf("wants = %v, want %v", wants, tt.want)
			}
			if got, _ := io.ReadAll(replay); string(got) != tt.body {
				t.Errorf("replayed %q, want the whole body", got)
			}
		})
	}
}
