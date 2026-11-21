package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	dockerClientAccept = "application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json"
	ociIndexOnlyAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
	convertedListBody  = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json","manifests":[]}`
	convertedAMD64Body = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`
	dockerV2MediaType  = "application/vnd.docker.distribution.manifest.v2+json"
	wildcardAccept     = dockerV2MediaType + ", */*"
	refusedListAccept  = ociImageIndexMediaType + ", " + dockerManifestListMediaType + ";q=0, " + dockerV2MediaType
	dockerListAccept   = dockerManifestListMediaType + ", " + dockerV2MediaType
	convertedOCIBody   = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
)

// convertingRegistry answers like distribution: the index for a client that
// names its type, else the default platform's manifest. A wildcard doesn't
// count as naming it, and neither does a type refused with q=0.
func convertingRegistry(t *testing.T, index string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, mediaType := convertedAMD64Body, dockerV2MediaType
		indexType := dockerManifestListMediaType
		if index == convertedOCIBody {
			indexType = ociImageIndexMediaType
		}
		if containerManifestNames(r.Header.Get("Accept"), indexType) {
			body, mediaType = index, indexType
		}
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Docker-Content-Digest", sha256Digest([]byte(body)))
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func getManifestWithAccept(t *testing.T, h *ContainerHandler, accept string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil)
	request.Header.Set("Accept", accept)
	response := httptest.NewRecorder()
	h.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

// TestContainerConvertedManifestKeepsTheSharedList checks a single-platform
// manifest a registry converted for a client without Docker manifest list
// support doesn't replace, or become, the list other clients share.
func TestContainerConvertedManifestKeepsTheSharedList(t *testing.T) {
	for name, tc := range map[string]struct {
		index string
		order []string
	}{
		"list cached first":      {convertedListBody, []string{dockerClientAccept, ociIndexOnlyAccept, dockerClientAccept}},
		"converted cached first": {convertedListBody, []string{ociIndexOnlyAccept, ociIndexOnlyAccept, dockerClientAccept}},
		"wildcard client first":  {convertedListBody, []string{wildcardAccept, dockerClientAccept}},
		"refused list type":      {convertedListBody, []string{refusedListAccept, dockerClientAccept}},
		"converted OCI index":    {convertedOCIBody, []string{dockerListAccept, dockerClientAccept}},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := convertingRegistry(t, tc.index)
			proxy, _, _, _ := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.MetadataTTL = time.Hour
			h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}

			for i, accept := range tc.order {
				want := tc.index
				if accept != dockerClientAccept {
					want = convertedAMD64Body
				}
				if got := getManifestWithAccept(t, h, accept); got != want {
					t.Fatalf("request %d got %s, want %s", i+1, got, want)
				}
			}
		})
	}
}

// TestContainerConvertedManifestIsSharedByDigest checks a converted manifest
// is still stored under its digest for every client, with the upstream down.
func TestContainerConvertedManifestIsSharedByDigest(t *testing.T) {
	upstreamUp := true
	registry := convertingRegistry(t, convertedListBody)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !upstreamUp {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		registry.Config.Handler.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.MetadataTTL = time.Hour
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}

	getManifestWithAccept(t, h, ociIndexOnlyAccept)
	upstreamUp = false
	request := httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/"+sha256Digest([]byte(convertedAMD64Body)), nil)
	request.Header.Set("Accept", dockerClientAccept)
	response := httptest.NewRecorder()
	h.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != convertedAMD64Body {
		t.Fatalf("by digest: %d %s, want the converted manifest", response.Code, response.Body.String())
	}
}

// TestContainerIndexIsSharedFromAClientLackingTheOtherType checks an index
// is shared whichever index type the client that fetched it accepts.
func TestContainerIndexIsSharedFromAClientLackingTheOtherType(t *testing.T) {
	for mediaType, accept := range map[string]string{
		ociImageIndexMediaType:      ociIndexOnlyAccept,
		dockerManifestListMediaType: dockerListAccept,
	} {
		t.Run(mediaType, func(t *testing.T) {
			index := `{"schemaVersion":2,"mediaType":"` + mediaType + `","manifests":[]}`
			requests := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", mediaType)
				w.Header().Set("Docker-Content-Digest", sha256Digest([]byte(index)))
				_, _ = io.WriteString(w, index)
			}))
			defer upstream.Close()
			proxy, _, _, _ := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.MetadataTTL = time.Hour
			h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}

			for _, accept := range []string{accept, dockerClientAccept} {
				if got := getManifestWithAccept(t, h, accept); got != index {
					t.Fatalf("got %s, want the index", got)
				}
			}
			if requests != 1 {
				t.Errorf("upstream requests = %d, want 1", requests)
			}
		})
	}
}
