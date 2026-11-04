package npm

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestPortableImports keeps the package free of the proxy's infrastructure:
// no handler, database, storage, metrics, HTTP or logging, so it builds for
// targets such as WASI where those are not available.
func TestPortableImports(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not found")
	}
	out, err := exec.Command(goTool, "list", "-deps", "-f", "{{.Standard}} {{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	allowed := []string{
		"github.com/Masterminds/semver/v3",
		"github.com/git-pkgs/cooldown",
		"github.com/git-pkgs/proxy/internal/jsonscan",
		"github.com/git-pkgs/proxy/internal/jsonstream",
		"github.com/git-pkgs/proxy/internal/metadata/npm",
		"github.com/git-pkgs/proxy/internal/packageurl",
		"github.com/git-pkgs/purl",
		"github.com/git-pkgs/vers",
		"github.com/package-url/packageurl-go",
	}
	// database/sql/driver comes in through semver, which implements its
	// Valuer interface; it is only interfaces, no driver.
	forbiddenStd := []string{"database/sql", "log/slog", "net", "net/http"}
	for line := range strings.Lines(string(out)) {
		standard, path, _ := strings.Cut(strings.TrimSpace(line), " ")
		if standard == "true" {
			if slices.Contains(forbiddenStd, path) {
				t.Errorf("imports %s", path)
			}
			continue
		}
		if !slices.Contains(allowed, path) {
			t.Errorf("imports %s, which is not on the portable list", path)
		}
	}
}
