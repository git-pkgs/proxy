package handler

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCooldownSupportedCoversHandlers(t *testing.T) {
	calls := regexp.MustCompile(`(?:IsAllowed|Evaluate)\("([a-z]+)"|withheld\(h\.proxy, "([a-z]+)"|IsAllowed\((helmMetadataEcosystem)`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range calls.FindAllStringSubmatch(string(src), -1) {
			if m[3] != "" {
				found[helmMetadataEcosystem] = true
				continue
			}
			found[m[1]+m[2]] = true
		}
	}
	if len(found) < 10 {
		t.Fatalf("found only %d ecosystems checking cooldown, the pattern no longer matches the handlers", len(found))
	}
	for eco := range found {
		if !CooldownSupported(eco) {
			t.Errorf("%s checks cooldown but is missing from cooldownEcosystems", eco)
		}
	}
	for eco := range cooldownEcosystems {
		if !found[eco] {
			t.Errorf("%s is in cooldownEcosystems but no handler checks cooldown for it", eco)
		}
	}
}
