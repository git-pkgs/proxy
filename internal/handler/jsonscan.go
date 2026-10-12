package handler

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

var errMalformedJSON = errors.New("malformed JSON")

// errNotJSONObject is returned when a value expected to be an object is not.
var errNotJSONObject = errors.New("JSON value is not an object")

// errStopScan is returned by a callback to end a walk early once it has
// found what it was looking for.
var errStopScan = errors.New("stop scanning")

// jsonMember is one member of a JSON object as offsets into the bytes that
// were scanned. The key span includes its quotes.
type jsonMember struct {
	keyStart, keyEnd int
	valStart, valEnd int
}

func (m jsonMember) key(data []byte) []byte   { return data[m.keyStart:m.keyEnd] }
func (m jsonMember) value(data []byte) []byte { return data[m.valStart:m.valEnd] }

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
	return 0, errMalformedJSON
}

// skipJSONValue returns the index just past the value starting at data[i].
func skipJSONValue(data []byte, i int) (int, error) {
	if i >= len(data) {
		return 0, errMalformedJSON
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
		return 0, errMalformedJSON
	case '}', ']', ',', ':':
		return 0, errMalformedJSON
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

// forEachJSONMember calls fn for each member of the object obj holds, in
// document order. It returns errNotJSONObject if obj is not an object, and
// stops early with fn's error if fn returns one.
func forEachJSONMember(obj []byte, fn func(jsonMember) error) error {
	i := skipJSONSpace(obj, 0)
	if i >= len(obj) || obj[i] != '{' {
		return errNotJSONObject
	}
	i = skipJSONSpace(obj, i+1)
	if i < len(obj) && obj[i] == '}' {
		return nil
	}
	for {
		if i >= len(obj) || obj[i] != '"' {
			return errMalformedJSON
		}
		keyEnd, err := skipJSONString(obj, i)
		if err != nil {
			return err
		}
		colon := skipJSONSpace(obj, keyEnd)
		if colon >= len(obj) || obj[colon] != ':' {
			return errMalformedJSON
		}
		valStart := skipJSONSpace(obj, colon+1)
		valEnd, err := skipJSONValue(obj, valStart)
		if err != nil {
			return err
		}
		if err := fn(jsonMember{keyStart: i, keyEnd: keyEnd, valStart: valStart, valEnd: valEnd}); err != nil {
			return err
		}
		next := skipJSONSpace(obj, valEnd)
		if next >= len(obj) {
			return errMalformedJSON
		}
		switch obj[next] {
		case ',':
			i = skipJSONSpace(obj, next+1)
		case '}':
			return nil
		default:
			return errMalformedJSON
		}
	}
}

// forEachJSONElement calls fn with each element of the array arr holds, in
// order, and stops early with fn's error if fn returns one.
func forEachJSONElement(arr []byte, fn func(element []byte) error) error {
	i := skipJSONSpace(arr, 0)
	if i >= len(arr) || arr[i] != '[' {
		return errMalformedJSON
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
			return errMalformedJSON
		}
		switch arr[next] {
		case ',':
			i = skipJSONSpace(arr, next+1)
		case ']':
			return nil
		default:
			return errMalformedJSON
		}
	}
}

// rewriteJSONMembers copies the object obj holds to out byte for byte,
// except that rewrite may write a member's value itself. It returns true
// when it did; otherwise the original value is copied.
func rewriteJSONMembers(out *bytes.Buffer, obj []byte, rewrite func(out *bytes.Buffer, m jsonMember) (bool, error)) error {
	copied := 0
	err := forEachJSONMember(obj, func(m jsonMember) error {
		out.Write(obj[copied:m.valStart])
		copied = m.valStart
		rewritten, err := rewrite(out, m)
		if rewritten {
			copied = m.valEnd
		}
		return err
	})
	out.Write(obj[copied:])
	return err
}

// jsonKey decodes a quoted key as forEachJSONMember reports it.
func jsonKey(raw []byte) (string, error) {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1 : len(raw)-1]), nil
	}
	var key string
	err := json.Unmarshal(raw, &key)
	return key, err
}

// jsonKeyIs reports whether a quoted key decodes to name.
func jsonKeyIs(raw []byte, name string) bool {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1:len(raw)-1]) == name
	}
	key, err := jsonKey(raw)
	return err == nil && key == name
}

// jsonStringIs reports whether raw is a JSON string that decodes to s.
func jsonStringIs(raw []byte, s string) bool {
	return len(raw) >= 2 && raw[0] == '"' && jsonKeyIs(raw, s)
}

// findJSONMember returns the member of obj named name. When the key repeats,
// the last one wins, as it does for JSON.parse and encoding/json.
func findJSONMember(obj []byte, name string) (jsonMember, bool, error) {
	var found jsonMember
	ok := false
	err := forEachJSONMember(obj, func(m jsonMember) error {
		if jsonKeyIs(m.key(obj), name) {
			found, ok = m, true
		}
		return nil
	})
	return found, ok, err
}

// lookupJSON follows path through nested objects in doc and returns the
// value at its end. It returns nil and no error when a key along the path is
// missing or names something other than an object.
func lookupJSON(doc []byte, path ...string) ([]byte, error) {
	value := doc
	for _, name := range path {
		m, ok, err := findJSONMember(value, name)
		if errors.Is(err, errNotJSONObject) {
			return nil, nil
		}
		if err != nil || !ok {
			return nil, err
		}
		value = m.value(value)
	}
	return value, nil
}

// lookupJSONString is lookupJSON for a string value. ok is false when the
// value is missing or is not a string.
func lookupJSONString(doc []byte, path ...string) (string, bool, error) {
	raw, err := lookupJSON(doc, path...)
	if err != nil || len(raw) == 0 || raw[0] != '"' {
		return "", false, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, err
	}
	return s, true, nil
}
