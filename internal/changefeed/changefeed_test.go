package changefeed

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakePackagist serves a change feed the way packagist.org does: without
// since, 400 with the current timestamp; with an old since, a resync; with a
// known one, the actions recorded after it.
type fakePackagist struct {
	mu      sync.Mutex
	now     int64
	actions map[int64]string // since -> JSON actions
	asked   []string
}

func (p *fakePackagist) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	since := r.URL.Query().Get("since")
	p.asked = append(p.asked, since)
	w.Header().Set("Content-Type", "application/json")
	if since == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":"Invalid or missing since query parameter","timestamp":%d}`, p.now)
		return
	}
	n, _ := strconv.ParseInt(since, 10, 64)
	actions, ok := p.actions[n]
	if !ok {
		actions = `[{"type":"resync","package":"*","time":1}]`
	}
	_, _ = fmt.Fprintf(w, `{"actions":%s,"timestamp":%d}`, actions, p.now)
}

func TestComposerFeed(t *testing.T) {
	p := &fakePackagist{now: 100, actions: map[int64]string{
		100: `[{"type":"update","package":"acme/lib","time":1},{"type":"update","package":"acme/lib~dev","time":1},{"type":"delete","package":"acme/gone","time":1}]`,
		200: `[]`,
	}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	feed := NewComposerFeed(srv.Client(), ComposerFeedURL(srv.URL+"/"))
	ctx := context.Background()

	if c, err := feed.Changes(ctx); err != nil || c.Names != nil || c.All {
		t.Fatalf("first read = %+v, %v; want the start position only", c, err)
	}
	p.mu.Lock()
	p.now = 200
	p.mu.Unlock()
	c, err := feed.Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme/lib", "acme/lib~dev", "acme/gone", "acme/gone~dev"}; !slices.Equal(c.Names, want) || c.All {
		t.Errorf("changes = %+v, want %v", c, want)
	}
	if c, err := feed.Changes(ctx); err != nil || len(c.Names) != 0 || c.All {
		t.Errorf("changes without news = %+v, %v", c, err)
	}

	// A position the feed no longer has history for gets a resync.
	feed.since = 50
	if c, err := feed.Changes(ctx); err != nil || !c.All {
		t.Errorf("changes after a resync = %+v, %v, want All", c, err)
	}
	if want := []string{"", "100", "200", "50"}; !slices.Equal(p.asked, want) {
		t.Errorf("asked since %q, want %q", p.asked, want)
	}
}

func TestComposerFeedErrorKeepsPosition(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Query().Get("since") != "7" {
			t.Errorf("since = %q, want 7", r.URL.Query().Get("since"))
		}
		_, _ = w.Write([]byte(`{"actions":[{"type":"update","package":"acme/lib","time":1}],"timestamp":8}`))
	}))
	defer srv.Close()
	feed := &ComposerFeed{client: srv.Client(), url: srv.URL, since: 7}
	if _, err := feed.Changes(context.Background()); err == nil {
		t.Fatal("want an error from a failing feed")
	}
	if feed.since != 7 {
		t.Fatalf("since = %d after an error, want 7", feed.since)
	}
	fail.Store(false)
	if c, err := feed.Changes(context.Background()); err != nil || !slices.Equal(c.Names, []string{"acme/lib"}) {
		t.Errorf("changes after recovery = %+v, %v", c, err)
	}
}

// fakeReplicate serves a CouchDB-style changes feed over seqs 1..len(ids).
type fakeReplicate struct {
	mu    sync.Mutex
	ids   []string
	limit int
	asked []string
}

func (f *fakeReplicate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/registry/" {
		_, _ = fmt.Fprintf(w, `{"db_name":"registry","update_seq":%d}`, len(f.ids))
		return
	}
	if r.URL.Path != "/registry/_changes" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	f.asked = append(f.asked, r.URL.Query().Get("since"))
	if f.limit > 0 {
		limit = min(limit, f.limit)
	}
	var results []string
	last := since
	for seq := since + 1; seq <= len(f.ids) && len(results) < limit; seq++ {
		results = append(results, fmt.Sprintf(`{"seq":%d,"id":%q,"changes":[{"rev":"1-a"}]}`, seq, f.ids[seq-1]))
		last = seq
	}
	_, _ = fmt.Fprintf(w, `{"results":[%s],"last_seq":%d}`, strings.Join(results, ","), last)
}

func TestNPMFeed(t *testing.T) {
	f := &fakeReplicate{ids: []string{"old"}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	feed := NewNPMFeed(srv.Client(), srv.URL+"/registry/_changes")
	ctx := context.Background()

	if c, err := feed.Changes(ctx); err != nil || c.Names != nil {
		t.Fatalf("first read = %+v, %v; want the start position only", c, err)
	}
	f.mu.Lock()
	f.ids = append(f.ids, "lodash", "@scope/pkg", "_design/app")
	f.mu.Unlock()
	c, err := feed.Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lodash", "@scope/pkg"}; !slices.Equal(c.Names, want) {
		t.Errorf("changes = %v, want %v", c.Names, want)
	}
	if c, err := feed.Changes(ctx); err != nil || len(c.Names) != 0 {
		t.Errorf("changes without news = %+v, %v", c, err)
	}
	if want := []string{"1", "4"}; !slices.Equal(f.asked, want) {
		t.Errorf("asked since %q, want %q", f.asked, want)
	}
}

// TestNPMFeedPages checks that a backlog longer than a page is read page by
// page, and that one read stops after npmMaxPages.
func TestNPMFeedPages(t *testing.T) {
	f := &fakeReplicate{limit: npmPageLimit}
	srv := httptest.NewServer(f)
	defer srv.Close()
	feed := &NPMFeed{client: srv.Client(), url: srv.URL + "/registry/_changes", since: []byte("0")}
	f.ids = make([]string, npmPageLimit*(npmMaxPages+1)+5)
	for i := range f.ids {
		f.ids[i] = "pkg-" + strconv.Itoa(i)
	}

	c, err := feed.Changes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Names) != npmPageLimit*npmMaxPages {
		t.Errorf("first read returned %d names, want %d", len(c.Names), npmPageLimit*npmMaxPages)
	}
	c, err = feed.Changes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Names) != npmPageLimit+5 {
		t.Errorf("second read returned %d names, want %d", len(c.Names), npmPageLimit+5)
	}
}
