package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
)

// setOffline turns h's upstream.git entry "up" offline, as git.offline or
// its offline setting would.
func setOffline(h *GitHandler) {
	upstream := h.upstreams["up"]
	upstream.Offline = true
	h.upstreams["up"] = upstream
}

func TestGitOfflineServesTheCachedCopyWithoutTheUpstream(t *testing.T) {
	g := newGitTestSetup(t, 0, 0)
	head := runGitCommand(t, g.work, "rev-parse", "HEAD")
	runGitCommand(t, "", "clone", "--quiet", g.url(), filepath.Join(t.TempDir(), "clone"))
	runGitCommand(t, g.work, "commit", "--quiet", "--allow-empty", "-m", "second")
	runGitCommand(t, g.work, "push", "--quiet", "origin", "HEAD:main")

	setOffline(g.handler)
	requests := g.requests.Load()
	clone := filepath.Join(t.TempDir(), "clone")
	runGitCommand(t, "", "clone", "--quiet", g.url(), clone)
	if got := runGitCommand(t, clone, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want the cached %s", got, head)
	}
	if got := g.requests.Load() - requests; got != 0 {
		t.Errorf("the upstream got %d requests, want none", got)
	}
}

func TestGitOfflineRefusesARepositoryNotCached(t *testing.T) {
	g := newGitTestSetup(t, 0, 0)
	setOffline(g.handler)
	resp, err := http.Get(g.url() + "/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "offline") {
		t.Errorf("status %d, body %q; want 404 saying the upstream is offline", resp.StatusCode, body)
	}
	if got := g.requests.Load(); got != 0 {
		t.Errorf("the upstream got %d requests, want none", got)
	}
}

func TestGitWarmsRepository(t *testing.T) {
	h := NewGitHandler(nil, nil, config.UpstreamConfig{Git: map[string]config.GitUpstream{
		"up":      {URL: "https://git.example"},
		"offline": {URL: "https://other.example", Offline: true},
	}})
	for dir, want := range map[string]bool{
		h.upstreams["up"].dir + "/repo.git":      true,
		h.upstreams["offline"].dir + "/repo.git": false,
		"up-00000000/repo.git":                   false,
	} {
		if got := h.WarmsRepository(dir); got != want {
			t.Errorf("WarmsRepository(%q) = %v, want %v", dir, got, want)
		}
	}
}

func TestGitMirrorRefusesAnOfflineUpstream(t *testing.T) {
	g := newGitTestSetup(t, 0, 0)
	setOffline(g.handler)
	location := strings.TrimPrefix(g.upstreamURL, "http://") + "/repo"
	if _, err := g.handler.Mirror(context.Background(), location, ""); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("Mirror = %v, want an error saying the upstream is offline", err)
	}
	if got := g.requests.Load(); got != 0 {
		t.Errorf("the upstream got %d requests, want none", got)
	}
}

func TestGitLFSOfflineAnswersFromTheCache(t *testing.T) {
	u := newLFSUpstream(t)
	h := newLFSHandler(t, u)
	size := int64(len(lfsTestContent))
	objectPath := func(oid string, size int64) string {
		return fmt.Sprintf("/up/acme/widget.git/info/lfs/objects/%s/%d", oid, size)
	}
	if w := lfsRequest(h, http.MethodGet, objectPath(u.oid, size), ""); w.Code != http.StatusOK {
		t.Fatalf("GET: status %d, body %s", w.Code, w.Body)
	}

	setOffline(h)
	batches, downloads := u.batches.Load(), u.downloads.Load()
	missing := strings.Repeat("0", 64)
	const batchPath = "/up/acme/widget.git/info/lfs/objects/batch"

	// A batch none of whose objects are cached is answered too, object by
	// object.
	for _, body := range []string{
		lfsBatchBody("download", lfsObject{OID: u.oid, Size: size}, lfsObject{OID: missing, Size: 3}),
		lfsBatchBody("download", lfsObject{OID: missing, Size: 3}),
	} {
		w := lfsRequest(h, http.MethodPost, batchPath, body)
		if w.Code != http.StatusOK {
			t.Fatalf("batch: status %d, body %s", w.Code, w.Body)
		}
		var resp lfsBatchResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		for _, object := range resp.Objects {
			cached := object.OID == u.oid
			if cached != (object.Actions != nil) || cached == (object.Error != nil) {
				t.Errorf("object %+v, cached %v", object, cached)
			}
			if !cached && (object.Error.Code != http.StatusNotFound || !strings.Contains(object.Error.Message, "offline")) {
				t.Errorf("uncached object error = %+v, want 404 saying the upstream is offline", object.Error)
			}
		}
	}
	if w := lfsRequest(h, http.MethodGet, objectPath(u.oid, size), ""); w.Code != http.StatusOK || w.Body.String() != lfsTestContent {
		t.Errorf("GET of the cached object: status %d, body %q", w.Code, w.Body)
	}
	if w := lfsRequest(h, http.MethodGet, objectPath(missing, 3), ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "offline") {
		t.Errorf("GET of an uncached object: status %d, body %s; want 404 saying the upstream is offline", w.Code, w.Body)
	}
	if b, d := u.batches.Load()-batches, u.downloads.Load()-downloads; b != 0 || d != 0 {
		t.Errorf("the upstream got %d batches and %d downloads, want none", b, d)
	}
}
