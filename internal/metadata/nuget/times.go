package nuget

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
)

// Times holds the members of a registration index, page or leaf that
// decide which versions are in cooldown. Reading only them skips nearly all of
// a registration.
type Times struct {
	// ID is the node's link. It is kept for the pages of an index only.
	ID           string
	Version      string
	Published    string
	CatalogEntry CatalogTimes
	// CatalogLink is the catalogEntry of a standalone leaf, a link.
	CatalogLink string
	// Items is nil unless the node has an items array.
	Items []Times
}

// CatalogTimes is the catalogEntry of a registration leaf. It stays unset
// unless the entry is an object: standalone leaves carry a link there.
type CatalogTimes struct {
	set       bool
	version   string
	published string
}

// Release returns the version and publish time of a leaf. A leaf embedded in a
// page carries them in catalogEntry, a standalone leaf at its root.
func (t *Times) Release() (version, published string) {
	if t.CatalogEntry.set {
		return t.CatalogEntry.version, t.CatalogEntry.published
	}
	return t.Version, t.Published
}

// DecodeTimes reads a registration document the way WriteRegistration
// copies it, so that what is decided here holds for what is written there:
// with the same decoder, members by their exact names, and a member of an
// unexpected type left empty. A node that has one of these members twice is
// refused, as which of the two counts would be the reader's choice.
func DecodeTimes(body []byte) (*Times, error) {
	dec := newDecoder(body)
	if dec.PeekKind() == 'n' {
		return nil, fmt.Errorf("empty NuGet metadata")
	}
	document := &Times{}
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
func (t *Times) decode(dec *jsontext.Decoder, depth int, scratch *[]byte) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	seen := 0
	for dec.PeekKind() != '}' {
		kind, err := readMember(dec, &seen, scratch)
		if err != nil {
			return err
		}
		switch value := dec.PeekKind(); {
		case kind == memberItems && value == '[':
			err = t.decodeItems(dec, depth, scratch)
		case kind == memberCatalogEntry && value == '{':
			err = t.CatalogEntry.decode(dec, scratch)
		case kind == memberCatalogEntry && value == '"' && depth == 0:
			t.CatalogLink, err = readString(dec, scratch)
		case kind == memberID && value == '"' && depth == 1:
			t.ID, err = readString(dec, scratch)
		case kind == memberVersion && value == '"':
			t.Version, err = readString(dec, scratch)
		case kind == memberPublished && value == '"':
			t.Published, err = readString(dec, scratch)
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
func (t *Times) decodeItems(dec *jsontext.Decoder, depth int, scratch *[]byte) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	t.Items = []Times{}
	for dec.PeekKind() != ']' {
		t.Items = append(t.Items, Times{})
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

func (e *CatalogTimes) decode(dec *jsontext.Decoder, scratch *[]byte) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	e.set = true
	seen := 0
	for dec.PeekKind() != '}' {
		kind, err := readMember(dec, &seen, scratch)
		if err != nil {
			return err
		}
		switch value := dec.PeekKind(); {
		case kind == memberVersion && value == '"':
			e.version, err = readString(dec, scratch)
		case kind == memberPublished && value == '"':
			e.published, err = readString(dec, scratch)
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

// decidingMembers are the members that decide what a node is and whether
// its release is in cooldown.
const decidingMembers = 1<<memberID | 1<<memberItems | 1<<memberCatalogEntry |
	1<<memberVersion | 1<<memberPublished

// readMember reads the name of the next member of an object and returns
// its kind. seen collects the deciding members the object has had.
func readMember(dec *jsontext.Decoder, seen *int, scratch *[]byte) (int, error) {
	raw, err := dec.ReadValue()
	if err != nil {
		return memberOther, err
	}
	kind := memberKind(unquote(scratch, raw))
	if bit := 1 << kind; bit&decidingMembers != 0 {
		if *seen&bit != 0 {
			return kind, fmt.Errorf("member %s is repeated", raw)
		}
		*seen |= bit
	}
	return kind, nil
}

func readString(dec *jsontext.Decoder, scratch *[]byte) (string, error) {
	raw, err := dec.ReadValue()
	if err != nil {
		return "", err
	}
	return string(unquote(scratch, raw)), nil
}
