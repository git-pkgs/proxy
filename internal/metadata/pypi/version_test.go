package pypi

import (
	"strings"
	"testing"
)

func TestCanonicalVersion(t *testing.T) {
	for in, want := range map[string]string{
		"2.0.0":                       "2",
		"2.0.0.0":                     "2",
		"2.0":                         "2",
		"2":                           "2",
		"0":                           "0",
		"0.0":                         "0",
		"1.10.0":                      "1.10",
		"1.0.1":                       "1.0.1",
		"V2.0.0":                      "2",
		" 2.0.0\n":                    "2",
		"01.002":                      "1.2",
		"0!1.0":                       "1",
		"1!2.0":                       "1!2",
		"2.0.0-rc1":                   "2rc1",
		"2.0.0RC1":                    "2rc1",
		"2.0.0.c1":                    "2rc1",
		"2.0.0preview1":               "2rc1",
		"2.0.0alpha":                  "2a0",
		"2.0.0-beta.2":                "2b2",
		"2.0b1":                       "2b1",
		"1.0099999999999999999999999": "1.99999999999999999999999",
		"1.0.0-1":                     "1.post1",
		"1.0.0_post1":                 "1.post1",
		"1.0.0.POST01":                "1.post1",
		"1.0.0rev":                    "1.post0",
		"1.0.0-r2":                    "1.post2",
		"1.0.0.dev":                   "1.dev0",
		"1.0.0-DEV3":                  "1.dev3",
		"1.0a1.post2.dev3":            "1a1.post2.dev3",
		"1.0+Ubuntu-1_2":              "1+ubuntu.1.2",
		"1.0+abc.007":                 "1+abc.7",
		// Not PEP 440: compared as given.
		"not a version": "not a version",
		"1.0po\u017ft1": "1.0po\u017ft1",
		"1.0-foo":       "1.0-foo",
		"":              "",
	} {
		if got := CanonicalVersion(in); got != want {
			t.Errorf("CanonicalVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSimpleFiltersMatchVersionSpellings checks the simple index filters
// leave out every spelling of a filtered version.
func TestSimpleFiltersMatchVersionSpellings(t *testing.T) {
	filtered := map[string]bool{"2.0.0": true}
	files := []string{"newpkg-2.0.0.0-py3-none-any.whl", "newpkg-V2.0-py3-none-any.whl", "newpkg-2.0.0.tar.gz"}
	var html strings.Builder
	for _, f := range files {
		html.WriteString(`<a href="https://files.example/packages/aa/bb/` + f + `">` + f + "</a>\n")
	}
	html.WriteString(`<a href="https://files.example/packages/aa/bb/newpkg-1.0-py3-none-any.whl">newpkg-1.0-py3-none-any.whl</a>`)
	for _, out := range [][]byte{
		RewriteSimpleHTML([]byte(html.String()), filtered, Options{}),
		FilterSimpleHTMLLinks([]byte(html.String()), filtered),
	} {
		for _, f := range files {
			if strings.Contains(string(out), f) {
				t.Errorf("%s left in:\n%s", f, out)
			}
		}
		if !strings.Contains(string(out), "newpkg-1.0-py3-none-any.whl") {
			t.Errorf("unfiltered file removed:\n%s", out)
		}
	}

	var entries []string
	for _, f := range append(files, "newpkg-1.0-py3-none-any.whl") {
		entries = append(entries, `{"filename": "`+f+`", "url": "https://files.example/packages/aa/bb/`+f+`"}`)
	}
	out, err := RewriteSimpleJSON([]byte(`{"files": [`+strings.Join(entries, ",")+`], "versions": ["1.0", "2.0.0"]}`), map[string]bool{"2.0": true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.Contains(string(out), f) {
			t.Errorf("%s left in the JSON index:\n%s", f, out)
		}
	}
	if strings.Contains(string(out), `"2.0.0"`) || !strings.Contains(string(out), "newpkg-1.0-py3-none-any.whl") {
		t.Errorf("versions or files filtered wrongly:\n%s", out)
	}
}
