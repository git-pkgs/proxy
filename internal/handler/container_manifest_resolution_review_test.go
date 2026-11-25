package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestContainerHandler_HeadResolutionKeepsConvertedManifestPerAccept(t *testing.T) {
	var requests atomic.Int32
	index := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`
	docs := map[string][2]string{
		sha256Digest([]byte(index)):    {index, ociImageIndexMediaType},
		sha256Digest([]byte(manifest)): {manifest, "application/vnd.docker.distribution.manifest.v2+json"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		ref := strings.TrimPrefix(r.URL.Path, "/v2/library/app/manifests/")
		var doc [2]string
		if ref == "latest" {
			if strings.Contains(r.Header.Get("Accept"), ociImageIndexMediaType) {
				doc = [2]string{index, ociImageIndexMediaType}
			} else {
				doc = [2]string{manifest, "application/vnd.docker.distribution.manifest.v2+json"}
			}
		} else if d, ok := docs[ref]; ok {
			doc = d
		} else {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", doc[1])
		w.Header().Set("Docker-Content-Digest", sha256Digest([]byte(doc[0])))
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(doc[0]))
		}
	}))
	t.Cleanup(srv.Close)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = srv.Client()
	proxy.MetadataTTL = time.Hour
	h := &ContainerHandler{proxy: proxy, registryURL: srv.URL, proxyURL: "http://localhost:8080"}
	do := func(method, ref, accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/library/app/manifests/"+ref, nil)
		req.Header.Set("Accept", accept)
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s = %d %s", method, ref, rec.Code, rec.Body.String())
		}
		return rec
	}
	oldDocker := "application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json"
	modern := ociImageIndexMediaType + ", " + dockerManifestListMediaType + ", application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
	head := do(http.MethodHead, "latest", oldDocker)
	do(http.MethodGet, head.Header().Get("Docker-Content-Digest"), oldDocker)
	if got := do(http.MethodGet, "latest", oldDocker); got.Body.String() != manifest || requests.Load() != 2 {
		t.Fatalf("per-Accept resolution was not reused: body=%s requests=%d", got.Body.String(), requests.Load())
	}
	got := do(http.MethodGet, "latest", modern)
	if got.Body.String() != index {
		t.Fatalf("index-accepting client got %s, want the index", got.Body.String())
	}
	if requests.Load() != 3 {
		t.Fatalf("modern client did not fetch its index: requests=%d", requests.Load())
	}
}

func TestContainerHandler_HeadIndexResolutionSharedAcrossAccepts(t *testing.T) {
	body := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
	digest := sha256Digest([]byte(body))
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", ociImageIndexMediaType)
		w.Header().Set("Docker-Content-Digest", digest)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	p, _, _, _ := setupTestProxy(t)
	p.HTTPClient, p.MetadataTTL = srv.Client(), time.Hour
	h := &ContainerHandler{proxy: p, registryURL: srv.URL}
	request := func(method, reference, accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/library/app/manifests/"+reference, nil)
		req.Header.Set("Accept", accept)
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status=%d body=%s", method, reference, rec.Code, rec.Body.String())
		}
		return rec
	}
	request(http.MethodHead, "latest", ociImageIndexMediaType)
	request(http.MethodGet, digest, ociImageIndexMediaType)
	got := request(http.MethodGet, "latest", ociImageIndexMediaType+", "+dockerManifestListMediaType)
	if got.Body.String() != body || requests.Load() != 2 {
		t.Fatalf("index resolution was not shared: body=%s requests=%d", got.Body.String(), requests.Load())
	}
}
