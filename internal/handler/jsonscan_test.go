package handler

import (
	"bytes"
	"errors"
	"testing"
)

func TestLookupJSONString(t *testing.T) {
	doc := []byte(`{
		"name": "demo",
		"versions": {
			"1.0.0": {"dist": {"tarball": "https://example.com/a.tgz", "shasum": "x"}},
			"2.0.0": {"readme": "a } tricky \" string ] with {brackets}", "dist": {"tarball": "https://example.com/b.tgz"}},
			"3.0.0": "not an object",
			"4.0.0": {"dist": {"tarball": 42}}
		},
		"time": {"1.0.0": "2020-01-01T00:00:00Z", "1.0.0": "2021-01-01T00:00:00Z"},
		"n": -1.5e3, "t": true, "z": null, "list": [1, {"a": []}, "]"]
	}`)

	cases := []struct {
		path   []string
		want   string
		wantOK bool
	}{
		{[]string{"versions", "1.0.0", "dist", "tarball"}, "https://example.com/a.tgz", true},
		{[]string{"versions", "2.0.0", "dist", "tarball"}, "https://example.com/b.tgz", true},
		{[]string{"versions", "3.0.0", "dist", "tarball"}, "", false},
		{[]string{"versions", "4.0.0", "dist", "tarball"}, "", false},
		{[]string{"versions", "9.9.9", "dist", "tarball"}, "", false},
		{[]string{"time", "1.0.0"}, "2021-01-01T00:00:00Z", true}, // last duplicate wins
		{[]string{"name"}, "demo", true},
		{[]string{"name", "x"}, "", false},
	}
	for _, c := range cases {
		got, ok, err := lookupJSONString(doc, c.path...)
		if err != nil || ok != c.wantOK || got != c.want {
			t.Errorf("lookup %v = %q, %v, %v; want %q, %v", c.path, got, ok, err, c.want, c.wantOK)
		}
	}
}

func TestLookupJSONEscapedKey(t *testing.T) {
	doc := []byte(`{"versions": {"1.0.0": {"dist": {"tarball": "u"}}}}`)
	got, ok, err := lookupJSONString(doc, "versions", "1.0.0", "dist", "tarball")
	if err != nil || !ok || got != "u" {
		t.Errorf("lookup = %q, %v, %v", got, ok, err)
	}
}

func TestLookupJSONMalformed(t *testing.T) {
	for _, doc := range []string{
		`{"versions": {"1.0.0": {"dist": `,
		`{"versions" {}}`,
		`{"versions": {"a": 1 "b": 2}}`,
		`{"a": "unterminated}`,
		`{"a": [1, 2}`,
	} {
		_, _, err := lookupJSONString([]byte(doc), "versions", "1.0.0", "dist", "tarball")
		if !errors.Is(err, errMalformedJSON) {
			t.Errorf("%s: err = %v, want errMalformedJSON", doc, err)
		}
	}
}

func TestForEachJSONMemberOffsets(t *testing.T) {
	doc := []byte(` { "a" : 1 , "b":{"c":[true,null]} ,"d":"x\"y" } `)
	var got [][2]string
	err := forEachJSONMember(doc, func(m jsonMember) error {
		got = append(got, [2]string{string(m.key(doc)), string(m.value(doc))})
		return nil
	})
	want := [][2]string{{`"a"`, `1`}, {`"b"`, `{"c":[true,null]}`}, {`"d"`, `"x\"y"`}}
	if err != nil || len(got) != len(want) {
		t.Fatalf("members = %v, %v", got, err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("member %d = %v, want %v", i, got[i], want[i])
		}
	}

	if err := forEachJSONMember([]byte(`[1]`), func(jsonMember) error { return nil }); !errors.Is(err, errNotJSONObject) {
		t.Errorf("array: err = %v, want errNotJSONObject", err)
	}
	if err := forEachJSONMember([]byte(`{}`), func(jsonMember) error { t.Error("called for empty object"); return nil }); err != nil {
		t.Errorf("empty object: err = %v", err)
	}
}

func TestWriteFilteredJSONObject(t *testing.T) {
	var out bytes.Buffer
	err := writeFilteredJSONObject(&out, []byte(`{"a": 1, "b": {"x": 2}, "c": 3}`), func(k string) bool { return k != "b" })
	if err != nil || out.String() != `{"a": 1,"c": 3}` {
		t.Errorf("filtered = %s, %v", out.String(), err)
	}
}

func TestForEachJSONElement(t *testing.T) {
	var got []string
	err := forEachJSONElement([]byte(` [ 1, "a,]", {"b": [2]} ,[] ] `), func(e []byte) error {
		got = append(got, string(e))
		return nil
	})
	want := []string{`1`, `"a,]"`, `{"b": [2]}`, `[]`}
	if err != nil || len(got) != len(want) {
		t.Fatalf("elements = %q, %v; want %q", got, err, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("element %d = %s, want %s", i, got[i], want[i])
		}
	}

	if err := forEachJSONElement([]byte(`[]`), func([]byte) error { t.Error("called for empty array"); return nil }); err != nil {
		t.Errorf("empty array: err = %v", err)
	}
	for _, bad := range []string{`{}`, `[1,`, `[1 2]`, `[1`} {
		if err := forEachJSONElement([]byte(bad), func([]byte) error { return nil }); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
}

func TestRewriteJSONMembers(t *testing.T) {
	obj := []byte(`{ "a": 1,  "b": [2], "c" : 3 }`)
	var out bytes.Buffer
	err := rewriteJSONMembers(&out, obj, func(out *bytes.Buffer, m jsonMember) (bool, error) {
		if !jsonKeyIs(m.key(obj), "b") {
			return false, nil
		}
		out.WriteString(`"B"`)
		return true, nil
	})
	if want := `{ "a": 1,  "b": "B", "c" : 3 }`; err != nil || out.String() != want {
		t.Errorf("rewritten = %s, %v; want %s", out.String(), err, want)
	}
}

func TestJSONStringIs(t *testing.T) {
	for raw, want := range map[string]bool{
		`"~dev"`:      true,
		`"~\u0064ev"`: true,
		`"~dev2"`:     false,
		`{"~dev":1}`:  false,
		`"`:           false,
		``:            false,
		`["~dev"]`:    false,
	} {
		if got := jsonStringIs([]byte(raw), "~dev"); got != want {
			t.Errorf("jsonStringIs(%s) = %v, want %v", raw, got, want)
		}
	}
}
