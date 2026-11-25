package handler

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/metadata/nuget"
)

var nugetRegistrationPrefixes = nuget.RegistrationPrefixes

const nugetRegistrationPath = nuget.RegistrationPath

// nugetTimes is what decides which versions of a registration are in
// cooldown (see nuget.Times).
type nugetTimes = nuget.Times

// nugetFlushSize is how much of a registration that isn't filtered is
// collected before it is written out.
const nugetFlushSize = nuget.FlushSize

// metadataOptions returns the inputs of the nuget transformations, with
// cooldown evaluated now.
func (h *NuGetHandler) metadataOptions() nuget.Options {
	o := nuget.Options{UpstreamURL: h.upstreamURL, ProxyURL: h.proxyURL, EvaluatedAt: time.Now()}
	if h.proxy != nil {
		o.Cooldown = h.proxy.cooldown()
		o.OnWithheld = func(id, version string, publishedAt time.Time) {
			h.proxy.recordWithheld("nuget", id, version, publishedAt)
		}
	}
	return o
}

func (h *NuGetHandler) cooldownEnabled() bool {
	return h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled()
}

func (h *NuGetHandler) nugetCooldownApplies(id string) bool {
	return h.cooldownEnabled() && h.proxy.Cooldown.For("nuget", canonicalPackagePURL("nuget", strings.ToLower(id))) > 0
}

// Cache upstream documents, not filtered results, so policy changes and elapsed
// time take effect even while metadata is fresh. Include the upstream in the key.
func (h *NuGetHandler) nugetMetadata(ctx context.Context, path string) (map[string]any, error) {
	var document map[string]any
	err := h.readNuGetMetadata(ctx, path, func(body []byte) (err error) {
		document, err = nuget.DecodeDocument(body)
		return err
	})
	if err != nil {
		return nil, err
	}
	return document, nil
}

// readNuGetMetadata fetches a document and passes it to decode, which keeps
// what its caller needs. decode can run more than once, and the document is
// the one it last accepted. It must not modify body.
func (h *NuGetHandler) readNuGetMetadata(ctx context.Context, path string, decode func(body []byte) error) error {
	target := h.upstreamURL + path
	key := fmt.Sprintf("_cooldown/%x", sha256.Sum256([]byte(target)))
	validate := func(body []byte) error {
		// Normally the HTTP transport decodes gzip. Also support compressed cached
		// bytes and clients with transparent decompression disabled, with the same
		// metadata limit applied to the decompressed document.
		if bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
			reader, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				return err
			}
			defer func() { _ = reader.Close() }()
			body, err = h.proxy.ReadMetadata(reader)
			if err != nil {
				return err
			}
		}
		return decode(body)
	}
	_, _, err := h.proxy.fetchOrCacheMetadata(ctx, "nuget", key, target, "", validate)
	return err
}

func (h *NuGetHandler) nugetTimesMetadata(ctx context.Context, path string) (*nugetTimes, error) {
	document, _, err := h.nugetTimesDocument(ctx, path)
	return document, err
}

// nugetTimesDocument is nugetTimesMetadata that also returns the document
// itself, for a caller that goes on to copy it.
func (h *NuGetHandler) nugetTimesDocument(ctx context.Context, path string) (*nugetTimes, []byte, error) {
	var document *nugetTimes
	var body []byte
	err := h.readNuGetMetadata(ctx, path, func(candidate []byte) error {
		decoded, err := decodeNuGetTimes(candidate)
		if err == nil {
			document, body = decoded, candidate
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return document, body, nil
}

// decodeNuGetTimes reads a registration document the way
// writeNuGetRegistration copies it (see nuget.DecodeTimes).
func decodeNuGetTimes(body []byte) (*nugetTimes, error) {
	return nuget.DecodeTimes(body)
}

// Prefer semver2, but a configured source may advertise only an older hive.
// Retry only advertised aliases on 404; transport/validation errors must not
// silently switch to a hive with less complete metadata. Keep requests on the
// configured upstream, consistent with the service-index route rewriting.
func (h *NuGetHandler) nugetRegistrationMetadata(ctx context.Context, suffix string) (*nugetTimes, string, error) {
	path := nugetRegistrationPath + suffix
	document, err := h.nugetTimesMetadata(ctx, path)
	if !errors.Is(err, ErrUpstreamNotFound) {
		return document, path, err
	}
	index, indexErr := h.nugetMetadata(ctx, "/v3/index.json")
	if indexErr != nil {
		return nil, path, indexErr
	}
	resources, _ := index["resources"].([]any)
	seen := map[string]bool{nugetRegistrationPath: true}
	for _, resource := range resources {
		entry, _ := resource.(map[string]any)
		service, _ := entry["@type"].(string)
		id, _ := entry["@id"].(string)
		if id == "" || !strings.HasPrefix(service, "RegistrationsBaseUrl") {
			continue
		}
		prefix := strings.TrimPrefix(h.rewriteNuGetURL(id, service), h.proxyURL+"/nuget")
		if !slices.Contains(nugetRegistrationPrefixes, prefix) || seen[prefix] {
			continue
		}
		seen[prefix] = true
		path = prefix + suffix
		document, err = h.nugetTimesMetadata(ctx, path)
		if !errors.Is(err, ErrUpstreamNotFound) {
			return document, path, err
		}
	}
	return nil, path, err
}

func (h *NuGetHandler) nugetMetadataError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUpstreamNotFound) {
		JSONError(w, http.StatusNotFound, "package metadata not found")
		return
	}
	if h.proxy.writeUpstreamRefusal(w, err) {
		return
	}
	h.proxy.Logger.Warn("failed to process NuGet metadata", "error", err)
	JSONError(w, http.StatusBadGateway, "failed to process package metadata")
}

func (h *NuGetHandler) handleVersionList(w http.ResponseWriter, r *http.Request) {
	if !h.cooldownEnabled() {
		h.proxyUpstream(w, r)
		return
	}
	id := strings.ToLower(r.PathValue("id"))
	document, err := h.nugetMetadata(r.Context(), "/v3-flatcontainer/"+url.PathEscape(id)+"/index.json")
	if err != nil {
		h.nugetMetadataError(w, err)
		return
	}
	blocked := make(map[string]bool)
	// A globally enabled policy may still exempt this package or ecosystem.
	// Keep metadata caching, but do not require publication data in that case.
	if h.nugetCooldownApplies(id) {
		registration, registrationPath, err := h.nugetRegistrationMetadata(r.Context(), url.PathEscape(id)+"/index.json")
		if err == nil {
			err = h.expandNuGetPageTimes(r.Context(), registration, registrationPath, nil)
		}
		if err != nil {
			h.nugetMetadataError(w, err)
			return
		}
		h.collectNuGetBlockedVersions(registration, id, blocked)
	}
	if err := nuget.FilterVersions(document, blocked); err != nil {
		h.nugetMetadataError(w, err)
		return
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(document)
}

func nugetVersionKey(version string) string {
	return nuget.VersionKey(version)
}

func (h *NuGetHandler) nugetDownloadAllowed(ctx context.Context, id, version string) (bool, error) {
	if !h.nugetCooldownApplies(id) {
		return true, nil
	}
	suffix := url.PathEscape(strings.ToLower(id)) + "/" + url.PathEscape(nugetVersionKey(version)) + ".json"
	leaf, _, err := h.nugetRegistrationMetadata(ctx, suffix)
	if err != nil {
		return false, err
	}
	_, published := leaf.Release()
	if nugetUnlistedTime(published) {
		// No time is one nugetPublishedAllowed lets through.
		if published, err = h.nugetCatalogCreated(ctx, leaf.CatalogLink); err != nil {
			return false, err
		}
	}
	return h.nugetPublishedAllowed(id, version, published), nil
}

func nugetUnlistedTime(published string) bool {
	return nuget.UnlistedTime(published)
}

// nugetCatalogCreated returns the time the catalog entry at link says its
// version was created, or nothing if the link is not one into the upstream
// or the entry has no such time.
func (h *NuGetHandler) nugetCatalogCreated(ctx context.Context, link string) (string, error) {
	path, ok := h.nugetCatalogPath(link)
	if !ok {
		return "", nil
	}
	var entry struct {
		Created string `json:"created"`
	}
	err := h.readNuGetMetadata(ctx, path, func(body []byte) error {
		entry.Created = ""
		if err := json.Unmarshal(body, &entry); err != nil {
			return fmt.Errorf("parsing NuGet catalog entry: %w", err)
		}
		return nil
	})
	return entry.Created, err
}

// nugetCatalogPath returns the path below the upstream of a catalog entry
// link, and whether the link is one into the upstream.
func (h *NuGetHandler) nugetCatalogPath(link string) (string, bool) {
	return h.metadataOptions().CatalogPath(link)
}

// nugetPublishedAllowed reports whether cooldown lets the version through
// (see nuget.Options.PublishedAllowed).
func (h *NuGetHandler) nugetPublishedAllowed(id, version, published string) bool {
	return h.metadataOptions().PublishedAllowed(id, version, published)
}

func (h *NuGetHandler) collectNuGetBlockedVersions(document *nugetTimes, id string, blocked map[string]bool) {
	h.metadataOptions().CollectBlocked(document, id, blocked)
}

func (h *NuGetHandler) handleRegistration(w http.ResponseWriter, r *http.Request) {
	if !h.cooldownEnabled() {
		h.proxyUpstream(w, r)
		return
	}
	id := nugetRegistrationID(r.URL.Path)
	applyCooldown := h.nugetCooldownApplies(id)
	// Everything the response is made of is loaded and checked before any of
	// it is written, so a registration that can't be filtered is an error
	// response and not a cut-off document.
	document, body, err := h.nugetTimesDocument(r.Context(), r.URL.Path)
	var pages map[string][]byte
	if err == nil && applyCooldown && strings.HasSuffix(r.URL.Path, "/index.json") {
		pages = make(map[string][]byte)
		err = h.expandNuGetPageTimes(r.Context(), document, r.URL.Path, pages)
	}
	if err != nil {
		h.nugetMetadataError(w, err)
		return
	}
	// The leaf check and the copy decide cooldown at the same time.
	opts := h.metadataOptions()
	if applyCooldown && document.Items == nil {
		version, published := document.Release()
		if version == "" {
			version = nugetRegistrationLeafVersion(r.URL.Path)
		}
		if !opts.PublishedAllowed(id, version, published) {
			JSONError(w, http.StatusNotFound, "version not found")
			return
		}
	}
	out := &countingWriter{w: w}
	buffered := bufio.NewWriterSize(out, nugetFlushSize)
	w.Header().Set(headerContentType, contentTypeJSON)
	err = nuget.WriteRegistration(buffered, body, id, applyCooldown, pages, opts)
	if err == nil {
		err = buffered.Flush()
	}
	if err != nil {
		h.proxy.Logger.Error("failed to write NuGet registration", "path", r.URL.Path, "error", err)
		if out.n == 0 {
			JSONError(w, http.StatusBadGateway, "failed to process package metadata")
			return
		}
		// Abort rather than end a partial document as if it were complete.
		panic(http.ErrAbortHandler)
	}
}

func nugetRegistrationID(path string) string {
	return nuget.RegistrationID(path)
}

// nugetRegistrationLeafVersion returns the version a registration leaf's
// path names (see nuget.RegistrationLeafVersion).
func nugetRegistrationLeafVersion(path string) string {
	return nuget.RegistrationLeafVersion(path)
}

// expandNuGetPageTimes gives each page of a registration index that only
// links to its leaves the leaves of the page it links to. With bodies not nil
// the linked pages themselves are kept in it, by link. Only index pages are
// expanded, never arbitrary upstream links followed: requests are pinned to
// this configured upstream and the current package's page path.
func (h *NuGetHandler) expandNuGetPageTimes(ctx context.Context, index *nugetTimes, path string, bodies map[string][]byte) error {
	if index.Items == nil {
		return fmt.Errorf("missing registration pages")
	}
	for i := range index.Items {
		page := &index.Items[i]
		if page.Items != nil {
			continue
		}
		pagePath, err := h.nugetRegistrationPagePath(path, page.ID)
		if err != nil {
			return err
		}
		pageDocument, body, err := h.nugetTimesDocument(ctx, pagePath)
		if err != nil {
			return err
		}
		if pageDocument.Items == nil {
			return fmt.Errorf("missing registration leaves")
		}
		page.Items = pageDocument.Items
		if bodies != nil {
			bodies[page.ID] = body
		}
	}
	return nil
}

// nugetRegistrationPagePath returns the upstream path of a page that the
// registration index at indexPath links to.
func (h *NuGetHandler) nugetRegistrationPagePath(indexPath, link string) (string, error) {
	return h.metadataOptions().RegistrationPagePath(indexPath, link)
}

func (h *NuGetHandler) nugetProxyLink(link string) string {
	return h.metadataOptions().ProxyLink(link)
}

// writeNuGetRegistration writes the registration document to w (see
// nuget.WriteRegistration).
func (h *NuGetHandler) writeNuGetRegistration(w io.Writer, document []byte, id string, filter bool, pages map[string][]byte) error {
	return nuget.WriteRegistration(w, document, id, filter, pages, h.metadataOptions())
}
