package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// hangingRegistry holds every manifest request until the request is
// cancelled or a while has passed, and reports which came first.
func hangingRegistry(t *testing.T, wait time.Duration) (*ContainerHandler, <-chan struct{}, <-chan bool) {
	t.Helper()
	entered := make(chan struct{}, 1)
	cancelled := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			cancelled <- true
		case <-time.After(wait):
			cancelled <- false
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = server.Client()
	proxy.MetadataTTL = time.Hour
	h := &ContainerHandler{proxy: proxy, registryURL: server.URL, proxyURL: "http://localhost:8080"}
	return h, entered, cancelled
}

// pullAndHangUp requests a manifest and cancels the request once it reached
// upstream.
func pullAndHangUp(t *testing.T, h *ContainerHandler, entered <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		request := httptest.NewRequest(http.MethodGet, "/library/app/manifests/latest", nil).WithContext(ctx)
		request.Header.Set("Accept", dockerManifestAccept)
		h.Routes().ServeHTTP(httptest.NewRecorder(), request)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the manifest request never reached upstream")
	}
	cancel()
	<-done
}

func TestContainerHandler_ManifestFetchOutlivesItsCaller(t *testing.T) {
	h, entered, cancelled := hangingRegistry(t, 300*time.Millisecond)
	pullAndHangUp(t, h, entered)
	if <-cancelled {
		t.Error("the upstream manifest request ended with its caller, with a detach slot free")
	}
}

// TestContainerHandler_ManifestFetchDetachCapFallsBackToClientContext checks
// that manifest fetches share the cap on detached metadata fetches: with
// every slot taken, a caller that hangs up ends its fetch.
func TestContainerHandler_ManifestFetchDetachCapFallsBackToClientContext(t *testing.T) {
	h, entered, cancelled := hangingRegistry(t, 5*time.Second)
	for range maxDetachedMetadataFetches {
		if !h.proxy.acquireMetadataDetachSlot() {
			t.Fatal("could not take a free metadata detach slot")
		}
	}
	defer func() {
		for range maxDetachedMetadataFetches {
			h.proxy.releaseMetadataDetachSlot()
		}
	}()
	pullAndHangUp(t, h, entered)
	if !<-cancelled {
		t.Error("the upstream manifest request kept running after its caller left, with no slot free")
	}
}

func TestContainerHandler_ManifestFetchReleasesItsDetachSlot(t *testing.T) {
	h, entered, cancelled := hangingRegistry(t, 50*time.Millisecond)
	for range maxDetachedMetadataFetches - 1 {
		if !h.proxy.acquireMetadataDetachSlot() {
			t.Fatal("could not take a free metadata detach slot")
		}
	}
	pullAndHangUp(t, h, entered)
	<-cancelled
	deadline := time.Now().Add(5 * time.Second)
	for !h.proxy.acquireMetadataDetachSlot() {
		if time.Now().After(deadline) {
			t.Fatal("the manifest fetch kept its detach slot after it ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
