package npm

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

//go:embed testdata/packument.json
var packument []byte

func TestRewriteCallbacks(t *testing.T) {
	options := Options{ProxyURL: "https://proxy.example", Cooldown: &cooldown.Config{Default: "7d"},
		DeniedVersions: map[string]bool{"2.0.0": true}, EvaluatedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	want, kept, err := Rewrite("@example/widgets", packument, options)
	if err != nil {
		t.Fatal(err)
	}
	var rewrites, filtered int
	options.OnTarballRewrite = func(version, oldURL, newURL string) {
		rewrites++
		if version != "1.0.0" || oldURL != "https://registry.npmjs.org/@example/widgets/-/widgets-1.0.0.tgz" || newURL != "https://proxy.example/npm/@example%2Fwidgets/-/widgets-1.0.0.tgz" {
			t.Errorf("rewrite event: %s, %s, %s", version, oldURL, newURL)
		}
	}
	options.OnCooldownFilter = func(version, publishedAt string) {
		filtered++
		if version != "3.0.0" || publishedAt != "2099-01-01T00:00:00Z" {
			t.Errorf("filter event: %s, %s", version, publishedAt)
		}
	}
	out, gotKept, err := Rewrite("@example/widgets", packument, options)
	if err != nil || !bytes.Equal(out, want) || !slices.Equal(gotKept, kept) || rewrites != 1 || filtered != 1 {
		t.Fatalf("callbacks: rewrites=%d, filtered=%d, kept=%v, error=%v", rewrites, filtered, gotKept, err)
	}
}

func TestRewriteHostedOverride(t *testing.T) {
	options := Options{
		ProxyURL:       "https://proxy.example",
		Cooldown:       &cooldown.Config{Default: "7d"},
		DeniedVersions: map[string]bool{"2.0.0": true},
		EvaluatedAt:    time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		LocalVersions: []LocalVersion{{Version: "3.0.0", PublishedAt: "2030-01-01T00:00:00Z",
			Entry: json.RawMessage(`{"version":"3.0.0","dist":{"tarball":"https://proxy.example/npm/@example%2Fwidgets/-/widgets-3.0.0.tgz","integrity":"sha512-local"}}`)}},
	}
	before := bytes.Clone(packument)
	localBefore := bytes.Clone(options.LocalVersions[0].Entry)
	out, kept, err := Rewrite("@example/widgets", packument, options)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(kept)
	if !slices.Equal(kept, []string{"1.0.0", "3.0.0"}) {
		t.Fatalf("kept = %v", kept)
	}
	var doc struct {
		Versions map[string]json.RawMessage `json:"versions"`
		Tags     map[string]string          `json:"dist-tags"`
		Time     map[string]string          `json:"time"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Versions) != 2 || !bytes.Equal(doc.Versions["3.0.0"], localBefore) || doc.Time["3.0.0"] != options.LocalVersions[0].PublishedAt {
		t.Fatalf("hosted override lost: %s", out)
	}
	if len(doc.Tags) != 1 || doc.Tags["latest"] != "1.0.0" || doc.Time["2.0.0"] != "" || doc.Time["created"] == "" {
		t.Fatalf("policy output: %s", out)
	}
	for _, want := range []string{"12345678901234567890", "<b>Widgets & tools</b>", "https://proxy.example/npm/@example%2Fwidgets/-/widgets-1.0.0.tgz"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lost %q", want)
		}
	}
	if !bytes.Equal(packument, before) || !bytes.Equal(options.LocalVersions[0].Entry, localBefore) || len(options.DeniedVersions) != 1 || !options.DeniedVersions["2.0.0"] {
		t.Fatal("rewrite mutated its input")
	}
	again, _, err := Rewrite("@example/widgets", packument, options)
	if err != nil || !bytes.Equal(out, again) {
		t.Fatalf("repeat changed output: %v", err)
	}
}

func TestVersionPolicyErrors(t *testing.T) {
	options := Options{ProxyURL: "https://proxy.example", Cooldown: &cooldown.Config{Default: "7d"},
		DeniedVersions: map[string]bool{"2.0.0": true}, EvaluatedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, tc := range []struct {
		specifier string
		want      error
	}{
		{"1.0.0", nil}, {"latest", nil}, {"2.0.0", ErrVersionDenied}, {"3.0.0", ErrVersionNotFound}, {"beta", ErrVersionNotFound}, {"missing", ErrVersionNotFound},
	} {
		t.Run(tc.specifier, func(t *testing.T) {
			out, err := Version("@example/widgets", tc.specifier, packument, options)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if tc.want == nil && (!bytes.Contains(out, []byte(`"version": "1.0.0"`)) || !bytes.Contains(out, []byte("https://proxy.example/npm/@example%2Fwidgets/-/widgets-1.0.0.tgz"))) {
				t.Fatalf("resolved version: %s", out)
			}
		})
	}
}

func TestRewriteCooldownEvaluationTime(t *testing.T) {
	body := []byte(`{"versions":{"1.0.0":{}},"time":{"1.0.0":"2030-01-01T00:00:00Z"}}`)
	boundary := time.Date(2030, 1, 8, 0, 0, 0, 0, time.UTC)
	options := Options{Cooldown: &cooldown.Config{Default: "7d"}}
	for _, tc := range []struct {
		at    time.Time
		count int
	}{
		{boundary.Add(-time.Nanosecond), 0}, {boundary, 1}, {boundary.Add(time.Second), 1},
	} {
		options.EvaluatedAt = tc.at
		_, kept, err := Rewrite("@example/widgets", body, options)
		if err != nil || len(kept) != tc.count {
			t.Errorf("at %s: kept %v, error %v", tc.at, kept, err)
		}
	}
	options.Cooldown = &cooldown.Config{Default: "7d", Packages: map[string]string{"pkg:npm/%40example/widgets": "0"}}
	options.EvaluatedAt = boundary.Add(-time.Hour)
	_, kept, err := Rewrite("@example/widgets", body, options)
	if err != nil || len(kept) != 1 {
		t.Fatalf("scoped exemption: kept %v, error %v", kept, err)
	}
}

func TestRewriteVersionDocument(t *testing.T) {
	body := []byte(`{"name":"@example/widgets","version":"1.0.0","dist":{"tarball":"https://cdn.example/abcdef.tgz"}}`)
	options := Options{ProxyURL: "https://proxy.example"}
	out, kept, err := Rewrite("@example/widgets", body, options)
	if err != nil || !slices.Equal(kept, []string{"1.0.0"}) || !bytes.Contains(out, []byte("https://proxy.example/npm/@example%2Fwidgets/-/widgets-1.0.0.tgz")) {
		t.Fatalf("rewrite: %s, %v, %v", out, kept, err)
	}
	options.DeniedVersions = map[string]bool{"1.0.0": true}
	if _, _, err := Rewrite("@example/widgets", body, options); !errors.Is(err, ErrVersionDenied) {
		t.Fatalf("denied version: %v", err)
	}
}
