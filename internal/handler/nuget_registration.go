package handler

import (
	"bytes"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// nugetFlushSize is how much of a document that isn't filtered is collected
// before it is written out.
const nugetFlushSize = 32 << 10

// The members of a registration document that are read or rewritten.
const (
	nugetMemberOther = iota
	nugetMemberID
	nugetMemberLink
	nugetMemberItems
	nugetMemberCount
	nugetMemberLower
	nugetMemberUpper
	nugetMemberCatalogEntry
	nugetMemberVersion
	nugetMemberPublished
)

func nugetMemberKind(name []byte) int {
	switch string(name) {
	case "@id":
		return nugetMemberID
	case "parent", "registration", "packageContent":
		return nugetMemberLink
	case "items":
		return nugetMemberItems
	case "count":
		return nugetMemberCount
	case "lower":
		return nugetMemberLower
	case "upper":
		return nugetMemberUpper
	case "catalogEntry":
		return nugetMemberCatalogEntry
	case "version":
		return nugetMemberVersion
	case "published":
		return nugetMemberPublished
	}
	return nugetMemberOther
}

// nugetRegistrationWriter copies a registration index, page or leaf with its
// links pointing at the proxy. With filter set it leaves out the leaves in
// cooldown and the pages that end up empty, and corrects the counts and
// bounds of the rest, as it goes: the document is never held as a tree.
//
// A document is made of nodes. A node with an items array is the index or a
// page, and the objects in that array are its nodes in turn; any other node
// is a leaf. Everything else is copied with its upstream bytes.
type nugetRegistrationWriter struct {
	h      *NuGetHandler
	id     string
	filter bool
	// pages holds the pages an index links to, by link. It is nil unless the
	// document is an index whose linked pages are to be inlined.
	pages map[string][]byte
	out   io.Writer
	// buf holds the output not written yet. A node is only known to be kept
	// once it has been read, so it is collected here and cut off again
	// otherwise.
	buf  bytes.Buffer
	name []byte // the current member's name, as it is in the document
	text []byte // scratch space for a decoded or encoded string
	// A link that is the upstream URL followed by plain path characters is
	// rewritten by putting proxyPrefix, which opens the JSON string, in place
	// of upstreamPrefix. proxyPrefix is nil when links can't be rewritten so.
	upstreamPrefix []byte
	proxyPrefix    []byte
}

// nugetLinkProbe is a path that nugetProxyLink rewrites.
const nugetLinkProbe = "/v3-flatcontainer/"

// setLinkPrefixes prepares rewriting plain links without parsing them. Nearly
// every link of a registration is one, and parsing each as a URL allocates
// more than the rest of the copy does.
func (rw *nugetRegistrationWriter) setLinkPrefixes() {
	upstream := rw.h.upstreamURL
	if strings.ContainsAny(upstream, "?#") {
		return
	}
	proxied, ok := strings.CutSuffix(rw.h.nugetProxyLink(upstream+nugetLinkProbe), nugetLinkProbe)
	if !ok {
		return
	}
	quoted, err := jsontext.AppendQuote(nil, proxied)
	if err != nil {
		return
	}
	rw.upstreamPrefix = []byte(upstream)
	rw.proxyPrefix = quoted[:len(quoted)-1]
}

// nugetArtifactPrefixBytes is nugetArtifactPrefixes for matching in a document.
var nugetArtifactPrefixBytes = func() [][]byte {
	prefixes := make([][]byte, len(nugetArtifactPrefixes))
	for i, prefix := range nugetArtifactPrefixes {
		prefixes[i] = []byte(prefix)
	}
	return prefixes
}()

// nugetLinkIsForeign reports whether a link can't be to a NuGet resource, so
// that nugetProxyLink leaves it alone: its path names none, however the link
// parses. An escaped link is not judged here.
func nugetLinkIsForeign(link []byte) bool {
	if bytes.IndexByte(link, '%') >= 0 {
		return false
	}
	for _, prefix := range nugetArtifactPrefixBytes {
		if bytes.Contains(link, prefix) {
			return false
		}
	}
	return true
}

// plainLinkPath returns the path of a link that nugetProxyLink rewrites to
// the same path below the proxy: one on the upstream, to a NuGet resource,
// with nothing in it that a URL or JSON would encode.
func (rw *nugetRegistrationWriter) plainLinkPath(link []byte) ([]byte, bool) {
	if rw.proxyPrefix == nil {
		return nil, false
	}
	path, ok := bytes.CutPrefix(link, rw.upstreamPrefix)
	if !ok || !slices.ContainsFunc(nugetArtifactPrefixBytes, func(prefix []byte) bool { return bytes.HasPrefix(path, prefix) }) {
		return nil, false
	}
	for _, c := range path {
		plain := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '/' || c == '.' || c == '-' || c == '_'
		if !plain {
			return nil, false
		}
	}
	return path, true
}

// writeLink writes a link, given as a JSON string, pointing at the proxy.
func (rw *nugetRegistrationWriter) writeLink(raw []byte) {
	link := rw.unquote(raw)
	if nugetLinkIsForeign(link) {
		rw.buf.Write(raw)
		return
	}
	if path, ok := rw.plainLinkPath(link); ok {
		rw.buf.Write(rw.proxyPrefix)
		rw.buf.Write(path)
		rw.buf.WriteByte('"')
		return
	}
	original := string(link)
	if proxied := rw.h.nugetProxyLink(original); proxied != original {
		rw.text, _ = jsontext.AppendQuote(rw.text[:0], proxied)
		raw = rw.text
	}
	rw.buf.Write(raw)
}

// nugetRelease is what an object says about the release it describes.
type nugetRelease struct {
	link      string
	version   []byte // as JSON, nil when there is none
	published string
}

// nugetNode is the state of the node being copied.
type nugetNode struct {
	depth     int
	wrote     bool // a member has been written
	own       nugetRelease
	entry     nugetRelease
	hasEntry  bool
	container bool
	kept      int
	first     []byte // version of the first and last child kept, as JSON
	last      []byte
	// Their values are only known at the end of a container, so these members
	// are held back and written last.
	count, lower, upper []byte
}

func newNuGetDecoder(document []byte) *jsontext.Decoder {
	// A bytes.Buffer is parsed in place. Duplicate names and invalid UTF-8
	// are accepted as encoding/json accepts them.
	return jsontext.NewDecoder(bytes.NewBuffer(document), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
}

// writeNuGetRegistration writes the registration document to w. The document
// and pages must have been decoded before, so that it only fails once the
// response has started if writing does.
func (h *NuGetHandler) writeNuGetRegistration(w io.Writer, document []byte, id string, filter bool, pages map[string][]byte) error {
	rw := &nugetRegistrationWriter{h: h, id: id, filter: filter, pages: pages, out: w}
	rw.setLinkPrefixes()
	dec := newNuGetDecoder(document)
	var err error
	if filter {
		_, _, err = rw.node(dec, 0)
	} else {
		err = rw.value(dec)
	}
	if err != nil {
		return err
	}
	return rw.flush()
}

func (rw *nugetRegistrationWriter) flush() error {
	_, err := rw.out.Write(rw.buf.Bytes())
	rw.buf.Reset()
	return err
}

// unquote returns the text of a JSON string. It is valid until the next call.
func (rw *nugetRegistrationWriter) unquote(raw []byte) []byte {
	return nugetUnquote(&rw.text, raw)
}

// nugetUnquote returns the text of a JSON string, in raw or in scratch.
func nugetUnquote(scratch *[]byte, raw []byte) []byte {
	if bytes.IndexByte(raw, '\\') < 0 {
		return raw[1 : len(raw)-1]
	}
	// The decoder has checked the string, so the only error left is invalid
	// UTF-8, which is replaced as encoding/json replaces it.
	*scratch, _ = jsontext.AppendUnquote((*scratch)[:0], raw)
	return *scratch
}

// readName reads the name of the next member and keeps it for writeName.
func (rw *nugetRegistrationWriter) readName(dec *jsontext.Decoder) (int, error) {
	raw, err := dec.ReadValue()
	if err != nil {
		return nugetMemberOther, err
	}
	rw.name = append(rw.name[:0], raw...)
	return nugetMemberKind(rw.unquote(raw)), nil
}

// writeName writes the name read last, or name when it is not empty.
func (rw *nugetRegistrationWriter) writeName(wrote *bool, name string) {
	if *wrote {
		rw.buf.WriteByte(',')
	}
	*wrote = true
	if name == "" {
		rw.buf.Write(rw.name)
	} else {
		rw.buf.WriteString(name)
	}
	rw.buf.WriteByte(':')
}

// value copies the value dec is at, rewriting the links in it.
func (rw *nugetRegistrationWriter) value(dec *jsontext.Decoder) error {
	switch dec.PeekKind() {
	case '{':
		return rw.object(dec, nil)
	case '[':
		return rw.array(dec)
	}
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}
	rw.buf.Write(raw)
	return nil
}

func (rw *nugetRegistrationWriter) array(dec *jsontext.Decoder) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	rw.buf.WriteByte('[')
	for i := 0; dec.PeekKind() != ']'; i++ {
		if i > 0 {
			rw.buf.WriteByte(',')
		}
		if err := rw.value(dec); err != nil {
			return err
		}
	}
	rw.buf.WriteByte(']')
	_, err := dec.ReadToken()
	return err
}

// object copies the object dec is at, rewriting the links in it. release,
// when not nil, receives what the object says about its release.
func (rw *nugetRegistrationWriter) object(dec *jsontext.Decoder, release *nugetRelease) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	rw.buf.WriteByte('{')
	wrote := false
	for dec.PeekKind() != '}' {
		kind, err := rw.readName(dec)
		if err != nil {
			return err
		}
		rw.writeName(&wrote, "")
		if err := rw.member(dec, kind, release); err != nil {
			return err
		}
		// Nothing written so far can be cut off again unless nodes are
		// filtered, so only then does the output wait for the node to end.
		if !rw.filter && rw.buf.Len() >= nugetFlushSize {
			if err := rw.flush(); err != nil {
				return err
			}
		}
	}
	rw.buf.WriteByte('}')
	_, err := dec.ReadToken()
	return err
}

// member copies the value of a member of the given kind.
func (rw *nugetRegistrationWriter) member(dec *jsontext.Decoder, kind int, release *nugetRelease) error {
	if kind == nugetMemberOther || dec.PeekKind() != '"' {
		return rw.value(dec)
	}
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}
	switch kind {
	case nugetMemberID, nugetMemberLink:
		if kind == nugetMemberID && release != nil && rw.pages != nil {
			release.link = string(rw.unquote(raw))
		}
		rw.writeLink(raw)
		return nil
	case nugetMemberVersion:
		if release != nil {
			release.version = append(release.version[:0], raw...)
		}
	case nugetMemberPublished:
		if release != nil {
			release.published = string(rw.unquote(raw))
		}
	}
	rw.buf.Write(raw)
	return nil
}

// node copies the index, page or leaf dec is at. It reports whether the node
// is to be kept, and the version of its release as JSON.
func (rw *nugetRegistrationWriter) node(dec *jsontext.Decoder, depth int) (bool, []byte, error) {
	if dec.PeekKind() != '{' {
		return false, nil, rw.value(dec)
	}
	if _, err := dec.ReadToken(); err != nil {
		return false, nil, err
	}
	rw.buf.WriteByte('{')
	n := &nugetNode{depth: depth}
	for dec.PeekKind() != '}' {
		kind, err := rw.readName(dec)
		if err != nil {
			return false, nil, err
		}
		if err := rw.nodeMember(dec, kind, n); err != nil {
			return false, nil, err
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return false, nil, err
	}
	if err := rw.endNode(n); err != nil {
		return false, nil, err
	}
	release := &n.own
	if n.hasEntry {
		release = &n.entry
	}
	if n.container {
		return n.kept > 0, release.version, nil
	}
	return rw.h.nugetPublishedAllowed(rw.id, release.published), release.version, nil
}

// linksPage reports whether a node at depth is a page of an index whose
// linked pages are inlined.
func (rw *nugetRegistrationWriter) linksPage(depth int) bool {
	return rw.pages != nil && depth == 1
}

func (rw *nugetRegistrationWriter) nodeMember(dec *jsontext.Decoder, kind int, n *nugetNode) error {
	var held *[]byte
	switch kind {
	case nugetMemberItems:
		if dec.PeekKind() == '[' {
			rw.writeName(&n.wrote, "")
			return rw.items(dec, n)
		}
		if rw.linksPage(n.depth) {
			// The leaves of the linked page take its place.
			return dec.SkipValue()
		}
	case nugetMemberCatalogEntry:
		// A standalone leaf has a link here and its release in the leaf.
		if dec.PeekKind() == '{' {
			rw.writeName(&n.wrote, "")
			n.hasEntry = true
			n.entry = nugetRelease{}
			return rw.object(dec, &n.entry)
		}
	case nugetMemberCount:
		held = &n.count
	case nugetMemberLower:
		held = &n.lower
	case nugetMemberUpper:
		held = &n.upper
	}
	if held != nil {
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		*held = append((*held)[:0], raw...)
		return nil
	}
	rw.writeName(&n.wrote, "")
	return rw.member(dec, kind, &n.own)
}

// items copies the array dec is at, the children of n, without those that
// are not kept.
func (rw *nugetRegistrationWriter) items(dec *jsontext.Decoder, n *nugetNode) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	n.container = true
	rw.buf.WriteByte('[')
	wrote := false
	for dec.PeekKind() != ']' {
		// What the document's root has so far is final. Below it a node can
		// still be cut off, by the page it is in ending up empty.
		if n.depth == 0 {
			if err := rw.flush(); err != nil {
				return err
			}
		}
		mark := rw.buf.Len()
		if wrote {
			rw.buf.WriteByte(',')
		}
		// A child that is not an object is left out.
		var keep bool
		var version []byte
		if dec.PeekKind() == '{' {
			var err error
			if keep, version, err = rw.node(dec, n.depth+1); err != nil {
				return err
			}
		} else if err := dec.SkipValue(); err != nil {
			return err
		}
		if !keep {
			rw.buf.Truncate(mark)
			continue
		}
		wrote = true
		if n.kept == 0 {
			n.first = append(n.first[:0], version...)
		}
		n.last = append(n.last[:0], version...)
		n.kept++
	}
	rw.buf.WriteByte(']')
	_, err := dec.ReadToken()
	return err
}

// endNode writes what is left of a node once its members have been read.
func (rw *nugetRegistrationWriter) endNode(n *nugetNode) error {
	if rw.linksPage(n.depth) && !n.container {
		if err := rw.linkedItems(n); err != nil {
			return err
		}
	}
	if n.container {
		n.count = append(n.count[:0], strconv.Itoa(n.kept)...)
		// Page bounds describe the retained leaves, not versions hidden by cooldown.
		if n.lower != nil && n.kept > 0 {
			n.lower, n.upper = nugetVersionJSON(n.first), nugetVersionJSON(n.last)
		}
	}
	for _, held := range []struct {
		name  string
		value []byte
	}{{`"count"`, n.count}, {`"lower"`, n.lower}, {`"upper"`, n.upper}} {
		if held.value == nil {
			continue
		}
		rw.writeName(&n.wrote, held.name)
		if held.value[0] != '{' && held.value[0] != '[' {
			rw.buf.Write(held.value)
			continue
		}
		// Links are rewritten at any depth, so in here too.
		if err := rw.value(newNuGetDecoder(held.value)); err != nil {
			return err
		}
	}
	rw.buf.WriteByte('}')
	return nil
}

// nugetVersionJSON returns the version of a leaf as JSON. A leaf without one
// leaves an empty version behind, which is not always nil.
func nugetVersionJSON(version []byte) []byte {
	if len(version) == 0 {
		return []byte("null")
	}
	return version
}

// linkedItems writes the leaves of the page that n, a page of the index
// without leaves of its own, links to.
func (rw *nugetRegistrationWriter) linkedItems(n *nugetNode) error {
	page, ok := rw.pages[n.own.link]
	if !ok {
		return fmt.Errorf("registration page %q was not loaded", n.own.link)
	}
	dec := newNuGetDecoder(page)
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	for dec.PeekKind() != '}' {
		kind, err := rw.readName(dec)
		if err != nil {
			return err
		}
		if kind != nugetMemberItems || dec.PeekKind() != '[' {
			err = dec.SkipValue()
		} else {
			rw.writeName(&n.wrote, "")
			err = rw.items(dec, n)
		}
		if err != nil {
			return err
		}
	}
	if !n.container {
		return fmt.Errorf("registration page %q has no leaves", n.own.link)
	}
	return nil
}
