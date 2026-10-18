package jsonstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func copyString(t *testing.T, in string, fn func(*Member) error) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := CopyObject(json.NewDecoder(strings.NewReader(in)), &out, fn)
	return out.String(), err
}

func TestCopyObjectKeepsValueBytes(t *testing.T) {
	in := `{"a": 1.50, "b" : {"x": "<&>", "y":[1, 2]}, "c":null, "d":"é"}`
	got, err := copyString(t, in, func(m *Member) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":1.50,"b":{"x": "<&>", "y":[1, 2]},"c":null,"d":"é"}`; got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestCopyObjectFiltersNested(t *testing.T) {
	in := `{"info":{"subdir":"noarch"},"packages":{"keep":{"n":1},"drop":{"n":2},"also":{"n":3}},"tail":[]}`
	got, err := copyString(t, in, func(m *Member) error {
		if m.Key != "packages" {
			return m.Copy()
		}
		return m.Object(func(entry *Member) error {
			raw, err := entry.Raw()
			if err != nil {
				return err
			}
			if entry.Key == "drop" {
				return nil
			}
			return entry.Write(raw)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"info":{"subdir":"noarch"},"packages":{"keep":{"n":1},"also":{"n":3}},"tail":[]}`; got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	if !json.Valid([]byte(got)) {
		t.Error("output is not valid JSON")
	}
}

func TestCopyObjectSkipReplaceAndEmpty(t *testing.T) {
	got, err := copyString(t, `{"first":1,"secret":"x","n":2,"empty":{}}`, func(m *Member) error {
		switch m.Key {
		case "first", "secret":
			return m.Skip()
		case "n":
			if _, err := m.Raw(); err != nil {
				return err
			}
			return m.Write([]byte(`3`))
		case "empty":
			return m.Object(func(*Member) error { return nil })
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"n":3,"empty":{}}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestCopyObjectKeys(t *testing.T) {
	got, err := copyString(t, `{"a<b":1,"quote\"d":2,"tab\t":3}`, func(m *Member) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a<b":1,"quote\"d":2,"tab\t":3}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestMemberMisuse(t *testing.T) {
	_, err := copyString(t, `{"a":1}`, func(m *Member) error { return m.Write([]byte("1")) })
	if !errors.Is(err, errUnconsumed) {
		t.Errorf("Write before Raw: err = %v", err)
	}
	_, err = copyString(t, `{"a":1}`, func(m *Member) error {
		if err := m.Skip(); err != nil {
			return err
		}
		return m.Skip()
	})
	if !errors.Is(err, errConsumed) {
		t.Errorf("second Skip: err = %v", err)
	}
}

func TestObjectNull(t *testing.T) {
	got, err := copyString(t, `{"packages":null}`, func(m *Member) error {
		return m.Object(func(*Member) error { return nil })
	})
	if err != nil || got != `{"packages":null}` {
		t.Errorf("got %s, %v", got, err)
	}
}

func TestCopyObjectErrors(t *testing.T) {
	tests := map[string]string{
		"not an object":        `[1,2]`,
		"truncated":            `{"a":{"b":1}`,
		"truncated value":      `{"a":"abc`,
		"trailing data":        `{"a":1} {"b":2}`,
		"nested not an object": `{"packages":[1]}`,
		"empty":                ``,
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := copyString(t, in, func(m *Member) error {
				if m.Key == "packages" {
					return m.Object(func(*Member) error { return nil })
				}
				return nil
			})
			if err == nil {
				t.Error("expected an error")
			}
		})
	}
	if _, err := copyString(t, `[1]`, func(*Member) error { return nil }); !errors.Is(err, ErrNotObject) {
		t.Errorf("err = %v, want ErrNotObject", err)
	}
}
