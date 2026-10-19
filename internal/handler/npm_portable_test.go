package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/git-pkgs/cooldown"
)

func TestNPMMetadataLogsPolicyAndURLChanges(t *testing.T) {
	body, err := os.ReadFile("../metadata/npm/testdata/packument.json")
	if err != nil {
		t.Fatal(err)
	}
	f := newNPMHostedFixture(t, map[string]string{"/@example%2Fwidgets": string(body)})
	f.proxy.Cooldown = &cooldown.Config{Default: "7d"}
	setTestDenylist(t, f.proxy, "pkg:npm/@example/widgets@2.0.0")
	var logs bytes.Buffer
	f.proxy.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if rec := f.get(t, "/@example/widgets"); rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	want := map[string]map[string]string{
		"cooldown: filtering npm version": {"level": "INFO", "package": "@example/widgets", "version": "3.0.0", "published": "2099-01-01T00:00:00Z"},
		"rewrote tarball URL": {"level": "DEBUG", "package": "@example/widgets", "version": "1.0.0",
			"old": "https://registry.npmjs.org/@example/widgets/-/widgets-1.0.0.tgz", "new": "http://proxy.local/npm/@example%2Fwidgets/-/widgets-1.0.0.tgz"},
	}
	decoder := json.NewDecoder(&logs)
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		message, _ := record["msg"].(string)
		fields, ok := want[message]
		if !ok {
			continue
		}
		for key, value := range fields {
			if record[key] != value {
				t.Errorf("%s: %s = %v, want %q", message, key, record[key], value)
			}
		}
		delete(want, message)
	}
	if len(want) != 0 {
		t.Errorf("missing log records: %v", want)
	}
}

func TestNPMRoutesWithPortableMetadata(t *testing.T) {
	body, err := os.ReadFile("../metadata/npm/testdata/packument.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, hosted := range []bool{false, true} {
		name := "upstream"
		if hosted {
			name = "hosted override"
		}
		t.Run(name, func(t *testing.T) {
			f := newNPMHostedFixture(t, map[string]string{"/@example%2Fwidgets": string(body)})
			f.proxy.Cooldown = &cooldown.Config{Default: "7d"}
			setTestDenylist(t, f.proxy, "pkg:npm/@example/widgets@2.0.0")
			if hosted {
				f.push(t, "@example/widgets", "3.0.0", `{"name":"@example/widgets","version":"3.0.0","dependencies":{"local":"1.0.0"}}`)
			}
			rec := f.get(t, "/@example/widgets")
			doc := decodePackument(t, rec)
			count := 1
			if hosted {
				count = 2
			}
			if len(doc.Versions) != count || doc.DistTags["latest"] != "1.0.0" || len(doc.DistTags) != 1 {
				t.Fatalf("filtered metadata: %s", rec.Body.String())
			}
			if doc.Versions["1.0.0"].Dist.Tarball != "http://proxy.local/npm/@example%2Fwidgets/-/widgets-1.0.0.tgz" || !strings.Contains(rec.Body.String(), "12345678901234567890") {
				t.Fatalf("rewritten metadata: %s", rec.Body.String())
			}
			if hosted && doc.Versions["3.0.0"].Dependencies["local"] != "1.0.0" {
				t.Fatalf("hosted override: %s", rec.Body.String())
			}
			checkNPMVersionResponses(t, f, hosted)
		})
	}
}

func checkNPMVersionResponses(t *testing.T, f npmHostedFixture, hosted bool) {
	t.Helper()
	for specifier, want := range map[string]int{"latest": http.StatusOK, "1.0.0": http.StatusOK, "2.0.0": http.StatusForbidden, "3.0.0": http.StatusNotFound} {
		if hosted && specifier == "3.0.0" {
			want = http.StatusOK
		}
		got := f.get(t, "/@example/widgets/"+specifier)
		if got.Code != want {
			t.Errorf("%s: HTTP %d, want %d: %s", specifier, got.Code, want, got.Body.String())
		}
	}
}
