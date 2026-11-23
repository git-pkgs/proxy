package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func signatureBody(gen int32) string { return fmt.Sprintf("signature of release generation %d\n", gen) }

// signatureCase is a repository whose release file has a detached
// signature: Debian Release and Release.gpg, RPM repomd.xml and
// repomd.xml.asc.
type signatureCase struct {
	releaseIndexCase
	signaturePath string
}

func signatureCases() []signatureCase {
	cases := releaseIndexCases()
	debian := cases[0]
	debian.name = "debian Release.gpg"
	debian.archive.releasePath = "dists/stable/Release"
	return []signatureCase{
		{releaseIndexCase: debian, signaturePath: "dists/stable/Release.gpg"},
		{releaseIndexCase: cases[1], signaturePath: "f/39/os/repodata/repomd.xml.asc"},
	}
}

// signedArchive serves tc's archive and a signature for each generation of
// its release file, counting the signature requests.
func signedArchive(t *testing.T, tc signatureCase) (http.Handler, func(path string), *atomic.Int32) {
	t.Helper()
	a := tc.archive
	var signatures atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+tc.signaturePath {
			signatures.Add(1)
			_, _ = w.Write([]byte(signatureBody(a.gen.Load())))
			return
		}
		a.serve(w, r)
	}))
	t.Cleanup(upstream.Close)
	proxy, db, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	h, key := tc.handler(proxy, upstream.URL)
	expire := func(path string) {
		t.Helper()
		if _, err := db.ExpireMetadataCache(tc.ecosystem, []string{key(path)}, time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return h, expire, &signatures
}

// TestSignatureFollowsRefetchedRelease checks that a cached detached
// signature isn't served with a release file fetched after it, which it
// can't sign.
func TestSignatureFollowsRefetchedRelease(t *testing.T) {
	for _, tc := range signatureCases() {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.archive
			h, expire, signatures := signedArchive(t, tc)

			getBody(t, h, a.releasePath)
			getBody(t, h, tc.signaturePath)

			// An index the cached release file doesn't list shows upstream
			// has moved on, and the release file is expired.
			a.gen.Store(1)
			getBody(t, h, a.indexPath)
			getBody(t, h, a.releasePath)
			if got := getBody(t, h, tc.signaturePath); got != signatureBody(1) {
				t.Errorf("signature after the release file was expired = %q, want %q", got, signatureBody(1))
			}

			// The release file's entry runs out before the signature's.
			a.gen.Store(2)
			expire(a.releasePath)
			getBody(t, h, a.releasePath)
			if got := getBody(t, h, tc.signaturePath); got != signatureBody(2) {
				t.Errorf("signature after the release file's TTL = %q, want %q", got, signatureBody(2))
			}

			// A signature fetched after the release file is served from the
			// cache.
			before := signatures.Load()
			if got := getBody(t, h, tc.signaturePath); got != signatureBody(2) {
				t.Errorf("cached signature = %q", got)
			}
			if got := signatures.Load(); got != before {
				t.Errorf("signature fetched %d more times, want it served from the cache", got-before)
			}
		})
	}
}

// TestSignatureWithoutCachedRelease checks that a signature asked for
// whose release file isn't cached is served as other metadata.
func TestSignatureWithoutCachedRelease(t *testing.T) {
	for _, tc := range signatureCases() {
		t.Run(tc.name, func(t *testing.T) {
			h, _, signatures := signedArchive(t, tc)
			for range 2 {
				if got := getBody(t, h, tc.signaturePath); got != signatureBody(0) {
					t.Errorf("signature = %q, want %q", got, signatureBody(0))
				}
			}
			if got := signatures.Load(); got != 1 {
				t.Errorf("signature fetched %d times, want it cached", got)
			}
		})
	}
}
