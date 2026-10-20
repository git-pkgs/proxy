package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/mirror"
)

// mirrorTestEnv carries the mirror command's arguments into the helper
// process, one per line.
const mirrorTestEnv = "PROXY_TEST_MIRROR_ARGS"

// writeScanningMirrorConfig writes a config with scanning enabled and storage
// and database under dir. The scanner address is never reached.
func writeScanningMirrorConfig(t *testing.T, dir string) string {
	t.Helper()
	cfg := "storage:\n" +
		"  url: file://" + filepath.Join(dir, "artifacts") + "\n" +
		"database:\n" +
		"  path: " + filepath.Join(dir, "proxy.db") + "\n" +
		"scanning:\n" +
		"  enabled: true\n" +
		"  signing_key: test-signing-key\n" +
		"  scanners:\n" +
		"    - name: test\n" +
		"      url: http://127.0.0.1:1/scan\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// runMirrorProcess runs proxy mirror with args in a helper process and
// checks that it exits with wantCode.
func runMirrorProcess(t *testing.T, wantCode int, args ...string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMirrorHelperProcess$")
	cmd.Env = append(os.Environ(), mirrorTestEnv+"="+strings.Join(args, "\n"))
	output, err := cmd.CombinedOutput()

	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("proxy mirror %v: %v\n%s", args, err, output)
	}
	if code != wantCode {
		t.Fatalf("proxy mirror %v exited with status %d, want %d\n%s", args, code, wantCode, output)
	}
	return string(output)
}

// With scanning enabled, proxy mirror refuses packages rather than caching
// them unscanned: scanners fetch staged bytes from the running server, which
// the command does not start.
// A refused package counts as failed, so the command exits 1.
func TestMirrorCommandRefusesPackagesWhileScanning(t *testing.T) {
	dir := t.TempDir()
	configPath := writeScanningMirrorConfig(t, dir)

	output := runMirrorProcess(t, 1, "-config", configPath, "pkg:npm/lodash@4.17.21")

	for _, want := range []string{mirrorScanningPackagesReason, "0 downloaded", "1 failed"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}

	db, err := database.Open(filepath.Join(dir, "proxy.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer func() { _ = db.Close() }()
	stats, err := db.GetCacheStats()
	if err != nil {
		t.Fatalf("GetCacheStats: %v", err)
	}
	if stats.TotalArtifacts != 0 {
		t.Errorf("cached artifacts = %d, want 0", stats.TotalArtifacts)
	}

	var stored []string
	_ = filepath.WalkDir(filepath.Join(dir, "artifacts"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			stored = append(stored, path)
		}
		return nil
	})
	if len(stored) != 0 {
		t.Errorf("files written to storage: %v", stored)
	}
}

// A dry run caches nothing, so it still lists what would be mirrored.
func TestMirrorCommandDryRunWhileScanning(t *testing.T) {
	configPath := writeScanningMirrorConfig(t, t.TempDir())

	output := runMirrorProcess(t, 0, "-config", configPath, "-dry-run", "pkg:npm/lodash@4.17.21")

	if !strings.Contains(output, "Would mirror 1 package versions") {
		t.Errorf("dry run output:\n%s", output)
	}
	if strings.Contains(output, mirrorScanningPackagesReason) {
		t.Errorf("dry run reported the refusal:\n%s", output)
	}
}

func TestRefuseUnscannedMirroring(t *testing.T) {
	m := mirror.New(nil, nil, nil, nil, 1)
	refuseUnscannedMirroring(m)

	if m.RefusePackages != mirrorScanningPackagesReason {
		t.Errorf("RefusePackages = %q, want %q", m.RefusePackages, mirrorScanningPackagesReason)
	}
	images, ok := m.Images.(mirror.UnavailableImages)
	if !ok || images.Reason != mirrorScanningImagesReason {
		t.Errorf("Images = %#v, want UnavailableImages with the scanning reason", m.Images)
	}
}

func TestMirrorHelperProcess(*testing.T) {
	args := os.Getenv(mirrorTestEnv)
	if args == "" {
		return
	}
	os.Args = append([]string{"proxy", "mirror"}, strings.Split(args, "\n")...)
	main()
}

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
