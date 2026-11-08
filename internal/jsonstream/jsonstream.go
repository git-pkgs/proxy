// Package jsonstream copies JSON objects from a reader to a writer one member
// at a time, so a document can be filtered without holding it in memory.
// Values that are kept are written with their original bytes.
package jsonstream

import (
	"bytes"
	"encoding/json/jsontext"
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
// exactly once, with Raw, Skip, Copy or Object.
type Member struct {
	Key      string
	rawKey   []byte
	dec      *jsontext.Decoder
	out      *objectWriter
	consumed bool
}

// Raw reads the member's value without writing it. Follow it with Write to
// keep the member. The bytes are only valid until the next member is read.
func (m *Member) Raw() ([]byte, error) {
	if m.consumed {
		return nil, errConsumed
	}
	m.consumed = true
	return m.dec.ReadValue()
}

// Skip reads past the member's value, leaving the member out.
func (m *Member) Skip() error {
	if m.consumed {
		return errConsumed
	}
	m.consumed = true
	return m.dec.SkipValue()
}

// Write writes the member with value as its raw JSON value.
func (m *Member) Write(value []byte) error {
	if !m.consumed {
		return errUnconsumed
	}
	if err := m.out.key(m.rawKey); err != nil {
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

// Object writes the member and copies its value, which must be an object,
// calling fn for each of its members. A null value is written as null.
func (m *Member) Object(fn func(*Member) error) error {
	if m.consumed {
		return errConsumed
	}
	m.consumed = true
	if err := m.out.key(m.rawKey); err != nil {
		return err
	}
	return copyObject(m.dec, m.out.w, fn, true)
}

// CopyObject copies the object read from r to w, calling fn for each member.
// fn decides what is written by how it consumes the member's value; a member
// whose value fn leaves unconsumed is copied unchanged. As with
// encoding/json, of repeated names each is passed to fn, and invalid UTF-8
// is accepted.
func CopyObject(r io.Reader, w io.Writer, fn func(*Member) error) error {
	dec := jsontext.NewDecoder(r, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if err := copyObject(dec, w, fn, false); err != nil {
		return err
	}
	_, err := dec.ReadToken()
	switch err {
	case io.EOF:
		return nil
	case nil:
		return errors.New("unexpected data after the top-level object")
	}
	return err
}

func copyObject(dec *jsontext.Decoder, w io.Writer, fn func(*Member) error, allowNull bool) error {
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if tok.Kind() == 'n' && allowNull {
		_, err := io.WriteString(w, "null")
		return err
	}
	if tok.Kind() != '{' {
		return ErrNotObject
	}
	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}
	out := &objectWriter{w: w}
	m := &Member{dec: dec, out: out}
	// keyBuf holds the current key; ReadValue's bytes don't outlast fn.
	var keyBuf []byte
	for dec.PeekKind() != '}' {
		// The key is read raw, so it is written with its own bytes.
		rawKey, err := dec.ReadValue()
		if err != nil {
			return err
		}
		if rawKey.Kind() != '"' {
			return fmt.Errorf("object key is %v, not a string", rawKey.Kind())
		}
		keyBuf = append(keyBuf[:0], rawKey...)
		key, err := jsontext.AppendUnquote(nil, keyBuf)
		if err != nil {
			return err
		}
		*m = Member{Key: string(key), rawKey: keyBuf, dec: dec, out: out}
		if err := fn(m); err != nil {
			return err
		}
		if !m.consumed {
			if err := m.Copy(); err != nil {
				return err
			}
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	_, err = io.WriteString(w, "}")
	return err
}

type objectWriter struct {
	w     io.Writer
	wrote bool
	buf   bytes.Buffer
}

// key writes the separator and the key's bytes for the next member.
func (o *objectWriter) key(rawKey []byte) error {
	o.buf.Reset()
	if o.wrote {
		o.buf.WriteByte(',')
	}
	o.buf.Write(rawKey)
	o.buf.WriteByte(':')
	o.wrote = true
	_, err := o.w.Write(o.buf.Bytes())
	return err
}
