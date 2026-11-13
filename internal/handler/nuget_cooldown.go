package handler

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

var nugetRegistrationPrefixes = []string{
	"/v3/registration5-semver1/",
	"/v3/registration5-gz-semver1/",
	"/v3/registration5-gz-semver2/",
}

var nugetArtifactPrefixes = append([]string{"/v3-flatcontainer/"}, nugetRegistrationPrefixes...)

const nugetRegistrationPath = "/v3/registration5-gz-semver2/"

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
		document, err = decodeNuGetDocument(body)
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

func decodeNuGetDocument(body []byte) (map[string]any, error) {
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("parsing NuGet metadata: %w", err)
	}
	if document == nil {
		return nil, fmt.Errorf("empty NuGet metadata")
	}
	return document, nil
}

// nugetTimes holds the members of a registration index, page or leaf that
// decide which versions are in cooldown. Reading only them skips nearly all of
// a registration.
type nugetTimes struct {
	// ID is the node's link. It is kept for the pages of an index only.
	ID           string
	Version      string
	Published    string
	CatalogEntry nugetCatalogTimes
	// Items is nil unless the node has an items array.
	Items []nugetTimes
}

// nugetCatalogTimes is the catalogEntry of a registration leaf. It stays unset
// unless the entry is an object: standalone leaves carry a link there.
type nugetCatalogTimes struct {
	set       bool
	version   string
	published string
}

// release returns the version and publish time of a leaf. A leaf embedded in a
// page carries them in catalogEntry, a standalone leaf at its root.
func (t *nugetTimes) release() (version, published string) {
	if t.CatalogEntry.set {
		return t.CatalogEntry.version, t.CatalogEntry.published
	}
	return t.Version, t.Published
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

// decodeNuGetTimes reads a registration document the way writeNuGetRegistration
// copies it, so that what is decided here holds for what is written there:
// with the same decoder, members by their exact names, and a member of an
// unexpected type left empty. A node that has one of these members twice is
// refused, as which of the two counts would be the reader's choice.
func decodeNuGetTimes(body []byte) (*nugetTimes, error) {
	dec := newNuGetDecoder(body)
	if dec.PeekKind() == 'n' {
		return nil, fmt.Errorf("empty NuGet metadata")
	}
	document := &nugetTimes{}
	var scratch []byte
	var err error
	if dec.PeekKind() == '{' {
		err = document.decode(dec, 0, &scratch)
	} else if _, err = dec.ReadValue(); err == nil {
		err = errors.New("not an object")
	}
	if err == nil {
		// Nothing may follow the document.
		if _, err = dec.ReadToken(); errors.Is(err, io.EOF) {
			return document, nil
		} else if err == nil {
			err = errors.New("data after the document")
		}
	}
	return nil, fmt.Errorf("parsing NuGet metadata: %w", err)
}

// decode reads the node dec is at, which is depth levels below the document.
func (t *nugetTimes) decode(dec *jsontext.Decoder, depth int, scratch *[]byte) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	seen := 0
	for dec.PeekKind() != '}' {
		kind, err := readNuGetMember(dec, &seen, scratch)
		if err != nil {
			return err
		}
		switch value := dec.PeekKind(); {
		case kind == nugetMemberItems && value == '[':
			err = t.decodeItems(dec, depth, scratch)
		case kind == nugetMemberCatalogEntry && value == '{':
			err = t.CatalogEntry.decode(dec, scratch)
		case kind == nugetMemberID && value == '"' && depth == 1:
			t.ID, err = readNuGetString(dec, scratch)
		case kind == nugetMemberVersion && value == '"':
			t.Version, err = readNuGetString(dec, scratch)
		case kind == nugetMemberPublished && value == '"':
			t.Published, err = readNuGetString(dec, scratch)
		default:
			err = dec.SkipValue()
		}
		if err != nil {
			return err
		}
	}
	_, err := dec.ReadToken()
	return err
}

// decodeItems reads the items array dec is at. A child that is not an object
// is an empty node.
func (t *nugetTimes) decodeItems(dec *jsontext.Decoder, depth int, scratch *[]byte) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	t.Items = []nugetTimes{}
	for dec.PeekKind() != ']' {
		t.Items = append(t.Items, nugetTimes{})
		var err error
		if dec.PeekKind() == '{' {
			err = t.Items[len(t.Items)-1].decode(dec, depth+1, scratch)
		} else {
			err = dec.SkipValue()
		}
		if err != nil {
			return err
		}
	}
	_, err := dec.ReadToken()
	return err
}

func (e *nugetCatalogTimes) decode(dec *jsontext.Decoder, scratch *[]byte) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	e.set = true
	seen := 0
	for dec.PeekKind() != '}' {
		kind, err := readNuGetMember(dec, &seen, scratch)
		if err != nil {
			return err
		}
		switch value := dec.PeekKind(); {
		case kind == nugetMemberVersion && value == '"':
			e.version, err = readNuGetString(dec, scratch)
		case kind == nugetMemberPublished && value == '"':
			e.published, err = readNuGetString(dec, scratch)
		default:
			err = dec.SkipValue()
		}
		if err != nil {
			return err
		}
	}
	_, err := dec.ReadToken()
	return err
}

// nugetDecidingMembers are the members that decide what a node is and whether
// its release is in cooldown.
const nugetDecidingMembers = 1<<nugetMemberID | 1<<nugetMemberItems | 1<<nugetMemberCatalogEntry |
	1<<nugetMemberVersion | 1<<nugetMemberPublished

// readNuGetMember reads the name of the next member of an object and returns
// its kind. seen collects the deciding members the object has had.
func readNuGetMember(dec *jsontext.Decoder, seen *int, scratch *[]byte) (int, error) {
	raw, err := dec.ReadValue()
	if err != nil {
		return nugetMemberOther, err
	}
	kind := nugetMemberKind(nugetUnquote(scratch, raw))
	if bit := 1 << kind; bit&nugetDecidingMembers != 0 {
		if *seen&bit != 0 {
			return kind, fmt.Errorf("member %s is repeated", raw)
		}
		*seen |= bit
	}
	return kind, nil
}

func readNuGetString(dec *jsontext.Decoder, scratch *[]byte) (string, error) {
	raw, err := dec.ReadValue()
	if err != nil {
		return "", err
	}
	return string(nugetUnquote(scratch, raw)), nil
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
	if writeUpstreamHeld(w, err) {
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
	versions, ok := document["versions"].([]any)
	if !ok {
		h.nugetMetadataError(w, fmt.Errorf("missing NuGet versions"))
		return
	}
	filtered := make([]any, 0, len(versions))
	for _, value := range versions {
		version, ok := value.(string)
		if !ok {
			h.nugetMetadataError(w, fmt.Errorf("invalid NuGet version"))
			return
		}
		if !blocked[nugetVersionKey(version)] {
			filtered = append(filtered, value)
		}
	}
	document["versions"] = filtered
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(document)
}

func nugetVersionKey(version string) string {
	version, _, _ = strings.Cut(version, "+")
	return strings.ToLower(version)
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
	_, published := leaf.release()
	return h.nugetPublishedAllowed(id, published), nil
}

// Missing/invalid timestamps retain the existing permissive behavior, but
// fetch and JSON errors must not bypass the policy.
func (h *NuGetHandler) nugetPublishedAllowed(id, published string) bool {
	when, err := time.Parse(time.RFC3339, published)
	if err != nil {
		return true
	}
	return h.proxy.cooldown().IsAllowed("nuget", canonicalPackagePURL("nuget", strings.ToLower(id)), when)
}

func (h *NuGetHandler) collectNuGetBlockedVersions(document *nugetTimes, id string, blocked map[string]bool) {
	if version, published := document.release(); version != "" && !h.nugetPublishedAllowed(id, published) {
		blocked[nugetVersionKey(version)] = true
	}
	for i := range document.Items {
		h.collectNuGetBlockedVersions(&document.Items[i], id, blocked)
	}
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
	if applyCooldown && document.Items == nil {
		if _, published := document.release(); !h.nugetPublishedAllowed(id, published) {
			JSONError(w, http.StatusNotFound, "version not found")
			return
		}
	}
	out := &countingWriter{w: w}
	buffered := bufio.NewWriterSize(out, nugetFlushSize)
	w.Header().Set(headerContentType, contentTypeJSON)
	err = h.writeNuGetRegistration(buffered, body, id, applyCooldown, pages)
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
	for _, prefix := range nugetRegistrationPrefixes {
		if rest, ok := strings.CutPrefix(path, prefix); ok {
			id, _, _ := strings.Cut(rest, "/")
			return id
		}
	}
	return ""
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
	base, err := url.Parse(h.upstreamURL + indexPath)
	if err != nil {
		return "", err
	}
	pagePrefix := strings.TrimSuffix(base.Path, "index.json") + "page/"
	target, err := base.Parse(link)
	if err != nil || target.Scheme != base.Scheme || target.Host != base.Host ||
		!strings.HasPrefix(target.Path, pagePrefix) || containsPathTraversal(target.Path) || target.RawQuery != "" || target.Fragment != "" {
		return "", fmt.Errorf("invalid registration page URL: %q", link)
	}
	upstream, _ := url.Parse(h.upstreamURL)
	return strings.TrimPrefix(target.Path, upstream.Path), nil
}

func (h *NuGetHandler) nugetProxyLink(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	upstream, err := url.Parse(h.upstreamURL)
	if err != nil {
		return link
	}
	path := u.Path
	if u.Host == upstream.Host {
		path = strings.TrimPrefix(path, upstream.Path)
	}
	for _, prefix := range nugetArtifactPrefixes {
		if strings.HasPrefix(path, prefix) {
			proxy, err := url.Parse(h.proxyURL + "/nuget" + path)
			if err != nil {
				return link
			}
			proxy.RawQuery = u.RawQuery
			proxy.Fragment = u.Fragment
			return proxy.String()
		}
	}
	return link
}
