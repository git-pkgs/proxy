package handler

import (
	"net/http"
	"testing"
)

func TestGemDownloadSharesPublishTimeMetadata(t *testing.T) {
	u, proxy, h := gemCacheSetup(t)
	wantStatus(t, serveGet(t, h, gemInfoPath), http.StatusOK, "")
	seedPackage(t, proxy.DB, proxy.Storage.(*mockStorage), "gem", "testgem", "1.0.0", "testgem-1.0.0.gem", "gem bytes")
	wantStatus(t, serveGet(t, h, "/gems/testgem-1.0.0.gem"), http.StatusOK, "")
	if n, _ := u.counts(gemVersionsPath); n != 1 {
		t.Fatalf("versions API fetched %d times, want 1", n)
	}
}

func TestHexDownloadSharesPublishTimeMetadata(t *testing.T) {
	u, proxy, h, _ := hexCacheSetup(t, 0)
	wantStatus(t, serveGet(t, h, hexPackagePath), http.StatusOK, "")
	seedPackage(t, proxy.DB, proxy.Storage.(*mockStorage), "hex", "testpkg", "1.0.0", "testpkg-1.0.0.tar", "hex bytes")
	wantStatus(t, serveGet(t, h, "/tarballs/testpkg-1.0.0.tar"), http.StatusOK, "")
	if n, _ := u.counts(hexAPIPath); n != 1 {
		t.Fatalf("Hex API fetched %d times, want 1", n)
	}
}
