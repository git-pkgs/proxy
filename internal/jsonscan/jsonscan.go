package jsonscan

import (
	"bytes"
	"encoding/json"
	"errors"
)

// The helpers in this file walk a JSON document in place, reporting where
// object members sit as offsets into the original bytes. They let a handler
// read or replace one value in a large metadata document without decoding the
// rest of it into Go values, which for an npm packument costs many times the
// document's size.
//
// They check structure only as far as they need to find their way through
// the document. Callers that copy unvisited bytes into a response validate the
// whole document first with json.Valid.

var ErrMalformed = errors.New("malformed JSON")

// ErrNotObject is returned when a value expected to be an object is not.
var ErrNotObject = errors.New("JSON value is not an object")

// ErrStop is returned by a callback to end a walk early once it has
// found what it was looking for.
var ErrStop = errors.New("stop scanning")

// Member is one member of a JSON object as offsets into the bytes that
// were scanned. The key span includes its quotes.
type Member struct {
	KeyStart, KeyEnd int
	ValStart, ValEnd int
}

func (m Member) Key(data []byte) []byte   { return data[m.KeyStart:m.KeyEnd] }
func (m Member) Value(data []byte) []byte { return data[m.ValStart:m.ValEnd] }

func skipJSONSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipJSONString returns the index just past the string starting at data[i].
func skipJSONString(data []byte, i int) (int, error) {
	for j := i + 1; j < len(data); j++ {
		switch data[j] {
		case '\\':
			j++
		case '"':
			return j + 1, nil
		}
	}
	return 0, ErrMalformed
}

// skipJSONValue returns the index just past the value starting at data[i].
func skipJSONValue(data []byte, i int) (int, error) {
	if i >= len(data) {
		return 0, ErrMalformed
	}
	switch data[i] {
	case '"':
		return skipJSONString(data, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(data); j++ {
			switch data[j] {
			case '"':
				end, err := skipJSONString(data, j)
				if err != nil {
					return 0, err
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, ErrMalformed
	case '}', ']', ',', ':':
		return 0, ErrMalformed
	default:
		// A number, true, false or null runs to the next delimiter.
		j := i
		for j < len(data) {
			switch data[j] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return j, nil
			}
			j++
		}
		return j, nil
	}
}

// EachMember calls fn for each member of the object obj holds, in
// document order. It returns ErrNotObject if obj is not an object, and
// stops early with fn's error if fn returns one.
func EachMember(obj []byte, fn func(Member) error) error {
	i := skipJSONSpace(obj, 0)
	if i >= len(obj) || obj[i] != '{' {
		return ErrNotObject
	}
	i = skipJSONSpace(obj, i+1)
	if i < len(obj) && obj[i] == '}' {
		return nil
	}
	for {
		if i >= len(obj) || obj[i] != '"' {
			return ErrMalformed
		}
		KeyEnd, err := skipJSONString(obj, i)
		if err != nil {
			return err
		}
		colon := skipJSONSpace(obj, KeyEnd)
		if colon >= len(obj) || obj[colon] != ':' {
			return ErrMalformed
		}
		ValStart := skipJSONSpace(obj, colon+1)
		ValEnd, err := skipJSONValue(obj, ValStart)
		if err != nil {
			return err
		}
		if err := fn(Member{KeyStart: i, KeyEnd: KeyEnd, ValStart: ValStart, ValEnd: ValEnd}); err != nil {
			return err
		}
		next := skipJSONSpace(obj, ValEnd)
		if next >= len(obj) {
			return ErrMalformed
		}
		switch obj[next] {
		case ',':
			i = skipJSONSpace(obj, next+1)
		case '}':
			return nil
		default:
			return ErrMalformed
		}
	}
}

// EachElement calls fn with each element of the array arr holds, in
// order, and stops early with fn's error if fn returns one.
func EachElement(arr []byte, fn func(element []byte) error) error {
	i := skipJSONSpace(arr, 0)
	if i >= len(arr) || arr[i] != '[' {
		return ErrMalformed
	}
	i = skipJSONSpace(arr, i+1)
	if i < len(arr) && arr[i] == ']' {
		return nil
	}
	for {
		end, err := skipJSONValue(arr, i)
		if err != nil {
			return err
		}
		if err := fn(arr[i:end]); err != nil {
			return err
		}
		next := skipJSONSpace(arr, end)
		if next >= len(arr) {
			return ErrMalformed
		}
		switch arr[next] {
		case ',':
			i = skipJSONSpace(arr, next+1)
		case ']':
			return nil
		default:
			return ErrMalformed
		}
	}
}

// RewriteMembers copies the object obj holds to out byte for byte,
// except that rewrite may write a member's value itself. It returns true
// when it did; otherwise the original value is copied.
func RewriteMembers(out *bytes.Buffer, obj []byte, rewrite func(out *bytes.Buffer, m Member) (bool, error)) error {
	copied := 0
	err := EachMember(obj, func(m Member) error {
		out.Write(obj[copied:m.ValStart])
		copied = m.ValStart
		rewritten, err := rewrite(out, m)
		if rewritten {
			copied = m.ValEnd
		}
		return err
	})
	out.Write(obj[copied:])
	return err
}

// Key decodes a quoted key as EachMember reports it.
func Key(raw []byte) (string, error) {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1 : len(raw)-1]), nil
	}
	var key string
	err := json.Unmarshal(raw, &key)
	return key, err
}

// KeyIs reports whether a quoted key decodes to name.
func KeyIs(raw []byte, name string) bool {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1:len(raw)-1]) == name
	}
	key, err := Key(raw)
	return err == nil && key == name
}

// StringIs reports whether raw is a JSON string that decodes to s.
func StringIs(raw []byte, s string) bool {
	return len(raw) >= 2 && raw[0] == '"' && KeyIs(raw, s)
}

// FindMember returns the member of obj named name. When the key repeats,
// the last one wins, as it does for JSON.parse and encoding/json.
func FindMember(obj []byte, name string) (Member, bool, error) {
	var found Member
	ok := false
	err := EachMember(obj, func(m Member) error {
		if KeyIs(m.Key(obj), name) {
			found, ok = m, true
		}
		return nil
	})
	return found, ok, err
}

// Lookup follows path through nested objects in doc and returns the
// value at its end. It returns nil and no error when a key along the path is
// missing or names something other than an object.
func Lookup(doc []byte, path ...string) ([]byte, error) {
	value := doc
	for _, name := range path {
		m, ok, err := FindMember(value, name)
		if errors.Is(err, ErrNotObject) {
			return nil, nil
		}
		if err != nil || !ok {
			return nil, err
		}
		value = m.Value(value)
	}
	return value, nil
}

// LookupString is Lookup for a string value. ok is false when the
// value is missing or is not a string.
func LookupString(doc []byte, path ...string) (string, bool, error) {
	raw, err := Lookup(doc, path...)
	if err != nil || len(raw) == 0 || raw[0] != '"' {
		return "", false, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, err
	}
	return s, true, nil
}

// WriteFilteredObject writes the object obj holds, keeping the members
// whose decoded key keep accepts, followed by extra string members.
func WriteFilteredObject(out *bytes.Buffer, obj []byte, keep func(string) bool, extra [][2]string) error {
	out.WriteByte('{')
	first := true
	err := EachMember(obj, func(m Member) error {
		key, err := Key(m.Key(obj))
		if err != nil {
			return err
		}
		if !keep(key) {
			return nil
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(obj[m.KeyStart:m.ValEnd])
		return nil
	})
	for _, member := range extra {
		if !first {
			out.WriteByte(',')
		}
		first = false
		key, _ := json.Marshal(member[0])
		value, _ := json.Marshal(member[1])
		out.Write(key)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return err
}
