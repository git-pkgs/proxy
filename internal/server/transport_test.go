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

// burst issues n concurrent GETs and drains every body so the connections
// return to the idle pool.
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
	handler := func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond) // keep the burst in flight together
		_, _ = w.Write([]byte("ok"))
	}

	tests := []struct {
		name          string
		client        *http.Client
		maxNewInBurst int
	}{
		{
			name:          "go default keeps two idle connections",
			client:        safehttp.New(nil, safehttp.Options{AllowLoopback: true}),
			maxNewInBurst: burstSize, // documents the baseline; asserted below as >= burstSize-2
		},
		{
			name:          "tuned transport reuses the whole burst",
			client:        newUpstreamClient(config.UpstreamConfig{AllowLoopback: true}),
			maxNewInBurst: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, accepted := tlsUpstream(t, handler)
			transport := tc.client.Transport.(*http.Transport)
			trustUpstream(t, transport, srv)

			burst(t, tc.client, srv.URL, burstSize)
			afterFirst := accepted()
			if afterFirst < burstSize {
				t.Fatalf("first burst opened %d connections, want at least %d", afterFirst, burstSize)
			}
			// Let the read loops hand the connections back to the idle pool.
			time.Sleep(50 * time.Millisecond)

			burst(t, tc.client, srv.URL, burstSize)
			newInSecond := accepted() - afterFirst
			t.Logf("second burst: %d new connections, %d reused", newInSecond, burstSize-newInSecond)

			if tc.maxNewInBurst == 0 {
				if newInSecond != 0 {
					t.Errorf("second burst opened %d new connections, want 0", newInSecond)
				}
				return
			}
			if newInSecond < burstSize-2 {
				t.Errorf("default transport reused more than its two idle connections: %d new", newInSecond)
			}
		})
	}
}

// TestUpstreamClientBoundsStallBeforeHeaders pins the production values and
// shows that an upstream which accepts a request but never sends headers is
// cut off by ResponseHeaderTimeout rather than by the client's overall
// timeout.
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

	// Shorten the header timeout for the test; the overall client timeout
	// stays far above it so only the header timeout can end the request.
	transport.ResponseHeaderTimeout = 200 * time.Millisecond
	client.Timeout = 10 * time.Second

	start := time.Now()
	resp, err := client.Get(srv.URL)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request to a stalled upstream succeeded, want a timeout")
	}
	if !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Fatalf("error = %v, want a response-header timeout", err)
	}
	if elapsed >= client.Timeout {
		t.Fatalf("request took %v, was bounded by the client timeout rather than the header timeout", elapsed)
	}
}
