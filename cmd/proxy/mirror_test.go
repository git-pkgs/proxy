package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/mirror"
)

func TestMirrorExitCode(t *testing.T) {
	tests := []struct {
		name     string
		progress *mirror.Progress
		want     int
	}{
		{"nil progress", nil, 0},
		{"everything mirrored", &mirror.Progress{Total: 2, Completed: 2}, 0},
		{"everything cached", &mirror.Progress{Total: 2, Skipped: 2}, 0},
		{"one failure", &mirror.Progress{Total: 2, Completed: 1, Failed: 1}, 1},
		{"all failed", &mirror.Progress{Total: 3, Failed: 3}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mirrorExitCode(tt.progress); got != tt.want {
				t.Errorf("mirrorExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

// proxy mirror used to exit 0 after printing "N failed", so a CI step that
// prewarms the cache passed even when every download failed (#500). An
// ecosystem the resolver rejects fails without touching the network.
func TestMirrorExitsNonZeroWhenDownloadsFail(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMirrorFailureProcess$")
	cmd.Env = append(os.Environ(),
		"PROXY_TEST_MIRROR_FAILURE=1",
		"PROXY_TEST_MIRROR_DIR="+dir,
	)
	output, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("mirror with a failed download exited with %v, want status 1\n%s", err, output)
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Fatalf("exit status = %d, want 1\n%s", code, output)
	}
	if !strings.Contains(string(output), "1 failed") {
		t.Errorf("output does not report the failure:\n%s", output)
	}
	if !strings.Contains(string(output), "bogus/foo@1.0.0") {
		t.Errorf("output does not list the failed package:\n%s", output)
	}
}

func TestMirrorFailureProcess(*testing.T) {
	if os.Getenv("PROXY_TEST_MIRROR_FAILURE") != "1" {
		return
	}
	dir := os.Getenv("PROXY_TEST_MIRROR_DIR")
	os.Args = []string{
		"proxy", "mirror",
		"--storage-url", "file://" + filepath.Join(dir, "store"),
		"--database-path", filepath.Join(dir, "cache.db"),
		"pkg:bogus/foo@1.0.0",
	}
	main()
}
