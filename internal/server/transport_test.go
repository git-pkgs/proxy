package server

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/registries/safehttp"
)

// tlsUpstream starts a TLS test server that counts accepted connections.
func tlsUpstream(t *testing.T, handler http.HandlerFunc) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	accepted := 0
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			accepted++
			mu.Unlock()
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return accepted
	}
}

// trustUpstream makes transport trust srv's certificate and pins HTTP/1.1 so
// every in-flight request needs its own connection.
func trustUpstream(t *testing.T, transport *http.Transport, srv *httptest.Server) {
	t.Helper()
	transport.TLSClientConfig = &tls.Config{
		RootCAs:    srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	}
	transport.ForceAttemptHTTP2 = false
	t.Cleanup(transport.CloseIdleConnections)
}

// burst issues n concurrent GETs and drains every body. The transport hands a
// connection back to the idle pool before the body's final Read returns, so
// the pool is settled when burst returns.
func burst(t *testing.T, client *http.Client, url string, n int) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(url)
			if err != nil {
				errs <- err
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("burst request: %v", err)
	}
}

// TestUpstreamClientReusesConnectionsAcrossBursts measures how many
// connections a second burst of concurrent requests reuses. With Go's default
// of two idle connections per host most of them are re-dialled; with the
// tuned transport the second burst reuses all of them.
func TestUpstreamClientReusesConnectionsAcrossBursts(t *testing.T) {
	const burstSize = 8

	// holdBurst returns a handler that answers a request only once burstSize
	// of them are waiting at the same time. With HTTP/1.1 pinned that puts
	// every burst on burstSize distinct connections, whatever the scheduling.
	holdBurst := func() http.HandlerFunc {
		var mu sync.Mutex
		waiting := 0
		release := make(chan struct{})
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			gate := release
			waiting++
			if waiting == burstSize {
				close(gate)
				waiting = 0
				release = make(chan struct{})
			}
			mu.Unlock()
			select {
			case <-gate:
			case <-r.Context().Done():
			}
			_, _ = w.Write([]byte("ok"))
		}
	}

	tests := []struct {
		name   string
		client *http.Client
		// Bounds on how many connections the second burst has to dial.
		minNew, maxNew int
	}{
		{
			name:   "go default keeps two idle connections",
			client: safehttp.New(nil, safehttp.Options{AllowLoopback: true}),
			minNew: burstSize - 2,
			maxNew: burstSize,
		},
		{
			name:   "tuned transport reuses the whole burst",
			client: newUpstreamClient(config.UpstreamConfig{AllowLoopback: true}),
			minNew: 0,
			maxNew: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, accepted := tlsUpstream(t, holdBurst())
			transport := tc.client.Transport.(*http.Transport)
			trustUpstream(t, transport, srv)

			burst(t, tc.client, srv.URL, burstSize)
			afterFirst := accepted()
			if afterFirst < burstSize {
				t.Fatalf("first burst opened %d connections, want at least %d", afterFirst, burstSize)
			}

			burst(t, tc.client, srv.URL, burstSize)
			newInSecond := accepted() - afterFirst
			t.Logf("second burst: %d new connections, %d reused", newInSecond, burstSize-newInSecond)

			if newInSecond < tc.minNew || newInSecond > tc.maxNew {
				t.Errorf("second burst opened %d new connections, want between %d and %d", newInSecond, tc.minNew, tc.maxNew)
			}
		})
	}
}

// TestUpstreamClientBoundsStallBeforeHeaders pins the production transport
// values, then lowers the header timeout so it can show within milliseconds
// that this is what cuts off an upstream which accepts a request but never
// sends headers.
func TestUpstreamClientBoundsStallBeforeHeaders(t *testing.T) {
	client := newUpstreamClient(config.UpstreamConfig{AllowLoopback: true})
	transport := client.Transport.(*http.Transport)
	if transport.MaxIdleConnsPerHost != upstreamMaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConnsPerHost = %d, want %d", transport.MaxIdleConnsPerHost, upstreamMaxIdleConnsPerHost)
	}
	if transport.ResponseHeaderTimeout != upstreamResponseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %v, want %v", transport.ResponseHeaderTimeout, upstreamResponseHeaderTimeout)
	}

	stall := make(chan struct{})
	srv, _ := tlsUpstream(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(stall) })
	trustUpstream(t, transport, srv)

	// Far below the client's overall timeout, so the header timeout ends the
	// request; the error text tells the two timeouts apart.
	transport.ResponseHeaderTimeout = 200 * time.Millisecond

	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request to a stalled upstream succeeded, want a timeout")
	}
	if !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Fatalf("error = %v, want a response-header timeout", err)
	}
}
