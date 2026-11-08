package gitcache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	relayIDBytes           = 16
	relayReadHeaderTimeout = 30 * time.Second
)

// relay is a loopback HTTP server that every git fetch the cache runs goes
// through. git never reaches the network itself: each fetch gets a route to
// one upstream repository, and the relay forwards its requests with the
// proxy's upstream client, so address checks, upstream.auth, the access log
// and the no-redirect rule apply as they do to the proxy's own requests, and
// git never sees a credential.
type relay struct {
	client   *http.Client
	logger   *slog.Logger
	listener net.Listener
	server   *http.Server
	base     string

	mu     sync.Mutex
	routes map[string]*relayRoute
}

type relayRoute struct {
	repoURL string
	// ctx ends when the fetch does, which stops its relayed requests, so a
	// git helper left behind by a killed git does not keep downloading.
	ctx context.Context
	// limit caps the response bytes the route relays, 0 for no cap, and
	// budget, when set, holds space within MaxSize for them. exceeded and
	// full record which of them stopped the route.
	limit    int64
	budget   *reservation
	relayed  atomic.Int64
	exceeded atomic.Bool
	full     atomic.Bool
	// answered records that the upstream answered a git-upload-pack
	// request, so a failed fetch was not an outage.
	answered atomic.Bool
	// redirected records the status of a redirect the upstream answered
	// with, 0 for none.
	redirected atomic.Int32
}

func newRelay(client *http.Client, logger *slog.Logger) (*relay, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if listener, err = net.Listen("tcp", "[::1]:0"); err != nil {
			return nil, err
		}
	}
	rl := &relay{
		client:   client,
		logger:   logger,
		listener: listener,
		base:     "http://" + listener.Addr().String(),
		routes:   make(map[string]*relayRoute),
	}
	rl.server = &http.Server{Handler: rl, ReadHeaderTimeout: relayReadHeaderTimeout}
	go func() {
		if err := rl.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("git relay stopped", "error", err)
		}
	}()
	return rl, nil
}

func (rl *relay) close() error {
	return rl.server.Close()
}

// register adds a route to repoURL for a fetch running with ctx and returns
// the URL git fetches from and a function that removes the route and stops
// its requests.
func (rl *relay) register(ctx context.Context, repoURL string, limit int64, budget *reservation) (string, *relayRoute, func()) {
	var b [relayIDBytes]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	ctx, cancel := context.WithCancel(ctx)
	route := &relayRoute{repoURL: repoURL, ctx: ctx, limit: limit, budget: budget}

	rl.mu.Lock()
	rl.routes[id] = route
	rl.mu.Unlock()

	return rl.base + "/" + id, route, func() {
		rl.mu.Lock()
		delete(rl.routes, id)
		rl.mu.Unlock()
		cancel()
	}
}

func (rl *relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, suffix, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	rl.mu.Lock()
	route := rl.routes[id]
	rl.mu.Unlock()
	if route == nil {
		http.NotFound(w, r)
		return
	}

	var target string
	switch {
	case suffix == "info/refs" && r.Method == http.MethodGet && r.URL.RawQuery == "service=git-upload-pack":
		target = route.repoURL + "/info/refs?service=git-upload-pack"
	case suffix == "git-upload-pack" && r.Method == http.MethodPost:
		target = route.repoURL + "/git-upload-pack"
	default:
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(route.ctx, cancel)()
	out, err := http.NewRequestWithContext(ctx, r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.ContentLength = r.ContentLength
	for _, name := range []string{"Git-Protocol", "Content-Type", "Content-Encoding", "Accept", "User-Agent"} {
		if value := r.Header.Get(name); value != "" {
			out.Header.Set(name, value)
		}
	}

	resp, err := rl.client.Do(out)
	if err != nil {
		if ctx.Err() == nil {
			rl.logger.Warn("git relay: upstream request failed", "upstream", route.repoURL, "error", err)
		}
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// A redirect passed on would send git to the Location itself.
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		route.redirected.Store(int32(resp.StatusCode))
		http.Error(w, "upstream redirect not followed", http.StatusBadGateway)
		return
	}

	if r.Method == http.MethodPost && resp.StatusCode == http.StatusOK {
		route.answered.Store(true)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, &routeReader{ctx: ctx, r: resp.Body, route: route}); err != nil {
		if r.Context().Err() != nil {
			// git went away; there is no one to tell.
			return
		}
		if ctx.Err() == nil && !route.exceeded.Load() && !route.full.Load() {
			rl.logger.Warn("git relay: copying upstream response", "upstream", route.repoURL, "error", err)
		}
		// Cut the connection so git fails rather than taking a response
		// that ended early for one that is complete.
		panic(http.ErrAbortHandler)
	}
}

// routeReader counts relayed bytes against the route's limit and budget.
type routeReader struct {
	ctx   context.Context
	r     io.Reader
	route *relayRoute
}

func (rr *routeReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	total := rr.route.relayed.Add(int64(n))
	if rr.route.limit > 0 && total > rr.route.limit {
		rr.route.exceeded.Store(true)
		return 0, ErrTooLarge
	}
	if rr.route.budget != nil {
		if err := rr.route.budget.cover(rr.ctx, total); err != nil {
			if errors.Is(err, ErrCacheFull) {
				rr.route.full.Store(true)
			}
			return 0, err
		}
	}
	return n, err
}
