package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/git-pkgs/cooldown"
)

// cutMetadataUpstream serves one npm packument and, when cut is set, closes
// the connection halfway through the document it announced.
type cutMetadataUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	body string
	cut  bool
}

func newCutMetadataUpstream(t *testing.T, body string) *cutMetadataUpstream {
	t.Helper()
	u := &cutMetadataUpstream{body: body}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		body, cut := u.body, u.cut
		u.mu.Unlock()
		if r.URL.Path != "/pkg" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if cut {
			_, _ = io.WriteString(w, body[:len(body)/2])
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *cutMetadataUpstream) set(body string, cut bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.body, u.cut = body, cut
}

// truncPackument is an npm packument with n versions published long ago.
func truncPackument(n int) string {
	var versions, times []string
	for i := range n {
		v := fmt.Sprintf("1.0.%d", i)
		versions = append(versions, fmt.Sprintf(`%q:{"name":"pkg","version":%q,"dist":{"tarball":"https://registry.npmjs.org/pkg/-/pkg-%s.tgz"}}`, v, v, v))
		times = append(times, fmt.Sprintf(`%q:"2020-01-01T00:00:00Z"`, v))
	}
	return `{"name":"pkg","dist-tags":{"latest":"1.0.0"},"versions":{` + strings.Join(versions, ",") +
		`},"time":{` + strings.Join(times, ",") + `}}`
}

// TestMetadataTruncatedUpstream checks that a document upstream cuts off
// before its Content-Length is neither served nor cached: the cached copy is
// served instead, with and without cooldown.
func TestMetadataTruncatedUpstream(t *testing.T) {
	for _, withCooldown := range []bool{false, true} {
		t.Run(map[bool]string{false: "no cooldown", true: "cooldown"}[withCooldown], func(t *testing.T) {
			u := newCutMetadataUpstream(t, truncPackument(50))
			proxy, db, _, _ := setupTestProxy(t)
			proxy.HTTPClient = u.Client()
			proxy.CacheMetadata = true
			if withCooldown {
				proxy.Cooldown = &cooldown.Config{Default: "3d"}
			}
			handler := NewNPMHandler(proxy, "http://proxy.example", u.URL).Routes()

			first := serve(handler, "/pkg", nil)
			if first.Code != http.StatusOK {
				t.Fatalf("first request: status %d", first.Code)
			}
			before, err := db.GetMetadataCache("npm", "pkg")
			if err != nil || before == nil {
				t.Fatalf("no metadata row after the first request: %v", err)
			}

			u.set(truncPackument(51), true)
			expireMetadata(t, db, "npm", "pkg")
			proxy.SetMetadataRewriteCacheSize(0)

			rec := serve(handler, "/pkg", nil)
			if rec.Code != http.StatusOK || rec.Body.String() != first.Body.String() {
				t.Errorf("status %d, want 200 with the cached document", rec.Code)
			}
			after, err := db.GetMetadataCache("npm", "pkg")
			if err != nil || after == nil || after.ContentDigest != before.ContentDigest {
				t.Error("the cut-off document replaced the cached copy")
			}
		})
	}
}

// TestMetadataTruncatedUpstreamUncached checks that a cut-off document with
// nothing cached fails the request and is not cached.
func TestMetadataTruncatedUpstreamUncached(t *testing.T) {
	u := newCutMetadataUpstream(t, truncPackument(50))
	u.set(truncPackument(50), true)
	proxy, db, _, _ := setupTestProxy(t)
	proxy.HTTPClient = u.Client()
	proxy.CacheMetadata = true
	handler := NewNPMHandler(proxy, "http://proxy.example", u.URL).Routes()

	if rec := serve(handler, "/pkg", nil); rec.Code != http.StatusBadGateway {
		t.Errorf("status %d, want 502", rec.Code)
	}
	if entry, _ := db.GetMetadataCache("npm", "pkg"); entry != nil {
		t.Error("cached the cut-off document")
	}
}
