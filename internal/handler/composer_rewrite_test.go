package handler

import (
	"encoding/json"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// composerVersions decodes rewritten metadata and returns packageName's
// versions as a Composer client sees them: minified lists are expanded the
// way Composer's MetadataMinifier::expand does it.
func composerVersions(t *testing.T, output []byte, packageName string) []map[string]any {
	t.Helper()
	var doc struct {
		Minified string         `json:"minified"`
		Packages map[string]any `json:"packages"`
	}
	if err := json.Unmarshal(output, &doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, output)
	}
	var versions []map[string]any
	var expanded map[string]any
	entries, _ := doc.Packages[packageName].([]any)
	for _, entry := range entries {
		if doc.Minified == "composer/2.0" && entry == "~dev" {
			expanded = nil
			continue
		}
		fields, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("version entry is not an object: %v", entry)
		}
		if doc.Minified != "composer/2.0" || expanded == nil {
			expanded = fields
			versions = append(versions, fields)
			continue
		}
		expanded = maps.Clone(expanded)
		for k, v := range fields {
			if v == composerUnset {
				delete(expanded, k)
				continue
			}
			expanded[k] = v
		}
		versions = append(versions, expanded)
	}
	return versions
}

func versionNames(versions []map[string]any) []string {
	names := make([]string, len(versions))
	for i, v := range versions {
		names[i], _ = v["version"].(string)
	}
	return names
}

func distURL(t *testing.T, version map[string]any) string {
	t.Helper()
	dist, ok := version["dist"].(map[string]any)
	if !ok {
		t.Fatalf("version %v has no dist object", version["version"])
	}
	url, _ := dist["url"].(string)
	return url
}

func cooldownComposerHandler() *ComposerHandler {
	proxy := &Proxy{Logger: slog.Default()}
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	return &ComposerHandler{proxy: proxy, proxyURL: "http://proxy"}
}

// TestComposerRewriteMetadataKeepsUpstreamBytes checks that only dist URLs
// change: numbers encoding/json would round through float64, characters it
// would HTML-escape and the key order all reach the client as upstream sent
// them.
func TestComposerRewriteMetadataKeepsUpstreamBytes(t *testing.T) {
	h := &ComposerHandler{proxy: testProxy(), proxyURL: "http://proxy"}
	input := `{"packages":{"v/p":[{"version":"1.0.0","extra":{"big":12345678901234567890,"html":"<b>&</b>"},` +
		`"dist":{"type":"zip","url":"https://example.com/abc.zip","reference":"abc"}}]},"security-advisories":[]}`

	out, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(input, "https://example.com/abc.zip", "http://proxy/composer/files/v/p/1.0.0/abc.zip", 1)
	if string(out) != want {
		t.Errorf("rewrite =\n%s\nwant\n%s", out, want)
	}
}

// TestComposerRewriteMetadataStaysMinified checks that minified metadata is
// rewritten without expanding it: each entry still carries only what changed
// from the one before, plus its own dist now that dist URLs name the version.
func TestComposerRewriteMetadataStaysMinified(t *testing.T) {
	h := &ComposerHandler{proxy: testProxy(), proxyURL: "http://proxy"}
	input := `{"minified":"composer/2.0","packages":{"v/p":[` +
		`{"name":"v/p","version":"2.0.0","require":{"php": ">=8.1"},"dist":{"url":"https://example.com/a.zip","type":"zip"}},` +
		`{"version":"1.1.0","dist":{"url":"https://example.com/b.zip","type":"zip"}},` +
		`{"version":"1.0.0","require":"__unset"}` +
		`]}}`

	out, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"minified":"composer/2.0","packages":{"v/p":[` +
		`{"name":"v/p","version":"2.0.0","require":{"php": ">=8.1"},"dist":{"url":"http://proxy/composer/files/v/p/2.0.0/a.zip","type":"zip"}},` +
		`{"version":"1.1.0","dist":{"url":"http://proxy/composer/files/v/p/1.1.0/b.zip","type":"zip"}},` +
		`{"version":"1.0.0","dist":{"url":"http://proxy/composer/files/v/p/1.0.0/b.zip","type":"zip"},"require":"__unset"}` +
		`]}}`
	if string(out) != want {
		t.Errorf("rewrite =\n%s\nwant\n%s", out, want)
	}
}

// TestComposerRewriteMetadataCooldownCarriesChanges checks that the changes a
// filtered entry made to the inherited fields still reach the next entry
// that is kept.
func TestComposerRewriteMetadataCooldownCarriesChanges(t *testing.T) {
	old := time.Now().Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	recent := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	input := `{"minified":"composer/2.0","packages":{"v/p":[
		{"name":"v/p","description":"Seven","version":"7.0.0","time":"` + recent + `",
		 "require-dev":{"phpunit/phpunit":"^11"},"dist":{"url":"https://example.com/7.zip","type":"zip"}},
		{"version":"6.4.0","time":"` + old + `","dist":{"url":"https://example.com/640.zip","type":"zip"}},
		{"description":"Backport","version":"5.4.9","time":"` + recent + `","require-dev":"__unset",
		 "dist":{"url":"https://example.com/549.zip","type":"zip"}},
		{"version":"5.4.8","time":"` + old + `"}
	]}}`

	out, err := cooldownComposerHandler().rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	versions := composerVersions(t, out, "v/p")
	if got := versionNames(versions); strings.Join(got, ",") != "6.4.0,5.4.8" {
		t.Fatalf("versions = %v, want [6.4.0 5.4.8]", got)
	}

	v640, v548 := versions[0], versions[1]
	if v640["name"] != "v/p" || v640["description"] != "Seven" || v640["require-dev"] == nil {
		t.Errorf("6.4.0 lost fields inherited from the filtered 7.0.0: %v", v640)
	}
	if v548["description"] != "Backport" {
		t.Errorf("5.4.8 description = %v, want the one set by the filtered 5.4.9", v548["description"])
	}
	if _, ok := v548["require-dev"]; ok {
		t.Errorf("5.4.8 require-dev = %v, want it unset by the filtered 5.4.9", v548["require-dev"])
	}
	if got := distURL(t, v548); got != "http://proxy/composer/files/v/p/5.4.8/549.zip" {
		t.Errorf("5.4.8 dist url = %q", got)
	}
}

// TestComposerRewriteMetadataCooldownAfterDevReset checks that a "~dev"
// reset survives when the first dev entry after it is filtered.
func TestComposerRewriteMetadataCooldownAfterDevReset(t *testing.T) {
	old := time.Now().Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	recent := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	input := `{"minified":"composer/2.0","packages":{"v/p":[
		{"name":"v/p","license":["MIT"],"version":"1.0.0","time":"` + old + `",
		 "dist":{"url":"https://example.com/1.zip","type":"zip"}},
		"~dev",
		{"name":"v/p","description":"Dev","version":"dev-main","time":"` + recent + `",
		 "dist":{"url":"https://example.com/main.zip","type":"zip"}},
		{"version":"1.x-dev","time":"` + old + `"}
	]}}`

	out, err := cooldownComposerHandler().rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	versions := composerVersions(t, out, "v/p")
	if got := versionNames(versions); strings.Join(got, ",") != "1.0.0,1.x-dev" {
		t.Fatalf("versions = %v, want [1.0.0 1.x-dev]", got)
	}
	dev := versions[1]
	if _, ok := dev["license"]; ok {
		t.Error("1.x-dev inherited license across the ~dev reset")
	}
	if dev["name"] != "v/p" || dev["description"] != "Dev" {
		t.Errorf("1.x-dev lost fields inherited from the filtered dev-main: %v", dev)
	}
	if got := distURL(t, dev); got != "http://proxy/composer/files/v/p/1.x-dev/main.zip" {
		t.Errorf("1.x-dev dist url = %q", got)
	}
}

func TestComposerRewriteMetadataInvalidJSON(t *testing.T) {
	h := &ComposerHandler{proxy: testProxy(), proxyURL: "http://proxy"}
	if _, err := h.rewriteMetadata([]byte(`{"packages":{"v/p":[{"version":"1.0.0"}]}`)); err == nil {
		t.Error("expected an error for truncated JSON")
	}
}

func TestComposerRewriteMetadataWithoutPackages(t *testing.T) {
	h := &ComposerHandler{proxy: testProxy(), proxyURL: "http://proxy"}
	for _, input := range []string{`{"other":1}`, `{"packages":[]}`} {
		out, err := h.rewriteMetadata([]byte(input))
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if string(out) != input {
			t.Errorf("rewrite of %s = %s, want it unchanged", input, out)
		}
	}
}

func TestComposerDistURL(t *testing.T) {
	minified := `{"minified":"composer/2.0","packages":{"v/p":[
		{"name":"v/p","version":"3.0.0","dist":{"url":"https://example.com/3.zip","type":"zip"}},
		{"version":"2.0.0"},
		{"version":"1.0.0","dist":"__unset"},
		"~dev",
		{"version":"dev-main","dist":{"url":"https://example.com/main.zip","type":"zip"}}
	]}}`
	expanded := `{"packages":{"v/p":[
		{"version":"2.0.0","dist":{"url":"https://example.com/2.zip","type":"zip"}},
		{"version":"1.0.0"}
	]}}`

	tests := []struct {
		name, body, version, want string
	}{
		{"own dist", minified, "3.0.0", "https://example.com/3.zip"},
		{"inherited dist", minified, "2.0.0", "https://example.com/3.zip"},
		{"unset dist", minified, "1.0.0", ""},
		{"after dev reset", minified, "dev-main", "https://example.com/main.zip"},
		{"missing version", minified, "9.9.9", ""},
		{"expanded", expanded, "2.0.0", "https://example.com/2.zip"},
		{"expanded inherits nothing", expanded, "1.0.0", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := composerDistURL([]byte(tt.body), "v/p", tt.version)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("composerDistURL(%s) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestComposerDistURLMalformed(t *testing.T) {
	if _, err := composerDistURL([]byte(`{"packages":{"v/p":[{"version":`), "v/p", "1.0.0"); err == nil {
		t.Error("expected an error for truncated JSON")
	}
}

// TestComposerRewriteMetadataRewritesEveryPackage checks that each package
// list in the document is rewritten under its own name, as before.
func TestComposerRewriteMetadataRewritesEveryPackage(t *testing.T) {
	h := &ComposerHandler{proxy: testProxy(), proxyURL: "http://proxy"}
	input := `{"packages":{
		"v/a":[{"version":"1.0.0","dist":{"url":"https://example.com/a.zip","type":"zip"}}],
		"v/b":[{"version":"2.0.0","dist":{"url":"https://example.com/b.zip","type":"zip"}}],
		"v/c":"not a list"
	}}`
	out, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if got := distURL(t, composerVersions(t, out, "v/a")[0]); got != "http://proxy/composer/files/v/a/1.0.0/a.zip" {
		t.Errorf("v/a dist url = %q", got)
	}
	if got := distURL(t, composerVersions(t, out, "v/b")[0]); got != "http://proxy/composer/files/v/b/2.0.0/b.zip" {
		t.Errorf("v/b dist url = %q", got)
	}
	if !strings.Contains(string(out), `"v/c":"not a list"`) {
		t.Errorf("non-list package value was not copied unchanged:\n%s", out)
	}
}
