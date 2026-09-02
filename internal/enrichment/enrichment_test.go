package enrichment

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

func TestNew(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := New(logger)

	if svc == nil {
		t.Fatal("New() returned nil")
	}

	if svc.regClient == nil {
		t.Error("regClient is nil")
	}

	if svc.vulnSource == nil {
		t.Error("vulnSource is nil")
	}
}

func TestSwiftRegistryIdentitySkipsPURLDependentLookups(t *testing.T) {
	svc := New(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	ctx := context.Background()

	packageInfo, err := svc.EnrichPackage(ctx, "swift", "apple/example")
	if err != nil || packageInfo != nil {
		t.Errorf("EnrichPackage() = %#v, %v; want nil, nil", packageInfo, err)
	}

	versionInfo, err := svc.EnrichVersion(ctx, "swift", "apple/example", "1.2.3")
	if err != nil || versionInfo != nil {
		t.Errorf("EnrichVersion() = %#v, %v; want nil, nil", versionInfo, err)
	}

	vulnerabilities, err := svc.CheckVulnerabilities(ctx, "swift", "apple/example", "1.2.3")
	if err != nil || vulnerabilities != nil {
		t.Errorf("CheckVulnerabilities() = %#v, %v; want nil, nil", vulnerabilities, err)
	}

	latest, err := svc.GetLatestVersion(ctx, "swift", "apple/example")
	if err != nil || latest != "" {
		t.Errorf("GetLatestVersion() = %q, %v; want empty string, nil", latest, err)
	}

	packages := []struct{ Ecosystem, Name string }{{Ecosystem: "swift", Name: "apple/example"}}
	if got := svc.BulkEnrichPackages(ctx, packages); len(got) != 0 {
		t.Errorf("BulkEnrichPackages() = %#v, want empty result", got)
	}
}

func TestIsOutdated(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := New(logger)

	tests := []struct {
		current  string
		latest   string
		expected bool
	}{
		{"1.0.0", "2.0.0", true},
		{"2.0.0", "2.0.0", false},
		{"2.0.0", "1.0.0", false},
		{"1.0.0", "", false},
		{"", "2.0.0", false},
		{"1.2.3", "1.2.4", true},
		{"1.2.4", "1.2.3", false},
	}

	for _, tc := range tests {
		result := svc.IsOutdated(tc.current, tc.latest)
		if result != tc.expected {
			t.Errorf("IsOutdated(%q, %q) = %v, want %v", tc.current, tc.latest, result, tc.expected)
		}
	}
}

func TestCategorizeLicense(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := New(logger)

	tests := []struct {
		license  string
		expected LicenseCategory
	}{
		{"MIT", LicensePermissive},
		{"Apache-2.0", LicensePermissive},
		{"BSD-3-Clause", LicensePermissive},
		{"GPL-3.0", LicenseCopyleft},
		{"AGPL-3.0", LicenseCopyleft},
		{"LGPL-2.1", LicenseCopyleft},
		{"", LicenseUnknown},
		{"Unknown", LicenseUnknown},
	}

	for _, tc := range tests {
		result := svc.CategorizeLicense(tc.license)
		if result != tc.expected {
			t.Errorf("CategorizeLicense(%q) = %v, want %v", tc.license, result, tc.expected)
		}
	}
}
