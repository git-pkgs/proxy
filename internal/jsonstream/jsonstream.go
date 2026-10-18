// Package jsonstream copies JSON objects from a reader to a writer one member
// at a time, so a document can be filtered without holding it in memory.
// Values that are kept are written with their original bytes.
package jsonstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrNotObject is returned when a value expected to be an object is not.
var ErrNotObject = errors.New("JSON value is not an object")

var (
	errConsumed   = errors.New("jsonstream: member value already consumed")
	errUnconsumed = errors.New("jsonstream: Write before the member value was read")
)

// Member is one member of an object being copied. Its value must be consumed
// exactly once, with Raw, Skip or Object.
type Member struct {
	Key      string
	dec      *json.Decoder
	out      *objectWriter
	consumed bool
}

// Raw reads the member's value without writing it. Follow it with Write to
// keep the member.
func (m *Member) Raw() (json.RawMessage, error) {
	if m.consumed {
		return nil, errConsumed
	}
	m.consumed = true
	var raw json.RawMessage
	if err := m.dec.Decode(&raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// Write writes the member with value as its raw JSON value.
func (m *Member) Write(value []byte) error {
	if !m.consumed {
		return errUnconsumed
	}
	if err := m.out.key(m.Key); err != nil {
		return err
	}
	_, err := m.out.w.Write(value)
	return err
}

// Copy writes the member with its value unchanged.
func (m *Member) Copy() error {
	raw, err := m.Raw()
	if err != nil {
		return err
	}
	return m.Write(raw)
}

// Skip reads the member's value and leaves the member out.
func (m *Member) Skip() error {
	_, err := m.Raw()
	return err
}

// Object writes the member and copies its value, which must be an object,
// calling fn for each of its members. A null value is written as null.
func (m *Member) Object(fn func(*Member) error) error {
	if m.consumed {
		return errConsumed
	}
	m.consumed = true
	if err := m.out.key(m.Key); err != nil {
		return err
	}
	return copyObject(m.dec, m.out.w, fn, true)
}

// CopyObject copies the object read from dec to w, calling fn for each
// member. fn decides what is written by how it consumes the member's value; a
// member whose value fn leaves unconsumed is copied unchanged.
func CopyObject(dec *json.Decoder, w io.Writer, fn func(*Member) error) error {
	if err := copyObject(dec, w, fn, false); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("unexpected data after the top-level object")
		}
		return err
	}
	return nil
}

func copyObject(dec *json.Decoder, w io.Writer, fn func(*Member) error, allowNull bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil && allowNull {
		_, err := io.WriteString(w, "null")
		return err
	}
	if tok != json.Delim('{') {
		return ErrNotObject
	}
	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}
	out := &objectWriter{w: w}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("object key is %T, not a string", tok)
		}
		m := &Member{Key: key, dec: dec, out: out}
		if err := fn(m); err != nil {
			return err
		}
		if !m.consumed {
			if err := m.Copy(); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	_, err = io.WriteString(w, "}")
	return err
}

type objectWriter struct {
	w     io.Writer
	wrote bool
}

// key writes the separator and key for the next member. Keys are re-encoded
// without HTML escaping, so plain keys keep their original bytes.
func (o *objectWriter) key(k string) error {
	var buf bytes.Buffer
	if o.wrote {
		buf.WriteByte(',')
	}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(k); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1) // Encode appends a newline
	buf.WriteByte(':')
	o.wrote = true
	_, err := o.w.Write(buf.Bytes())
	return err
}
