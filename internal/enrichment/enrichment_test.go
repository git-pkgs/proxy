package enrichment

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/git-pkgs/registries"
)

func TestNew(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := New(logger)

	if svc == nil {
		t.Fatal("New() returned nil")
	}

	if svc.registries == nil {
		t.Error("registries is nil")
	}

	if svc.vulns == nil {
		t.Error("vulns is nil")
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

func TestAdaptVersion(t *testing.T) {
	tests := []struct {
		status      registries.VersionStatus
		yanked      bool
		license     string
		wantYanked  bool
		wantLicense string
	}{
		{status: registries.StatusRetracted, license: "mit", wantYanked: true, wantLicense: "MIT"},
		{status: registries.StatusYanked, yanked: true, wantYanked: true},
		{status: registries.StatusDeprecated, license: "Custom terms", wantLicense: "Custom terms"},
	}
	for _, tc := range tests {
		info := &VersionInfo{Status: string(tc.status), Yanked: tc.yanked, License: tc.license}
		adaptVersion(info)
		if info.Yanked != tc.wantYanked || info.License != tc.wantLicense {
			t.Errorf("adaptVersion(%s, %q) = yanked %v, license %q; want %v, %q", tc.status, tc.license, info.Yanked, info.License, tc.wantYanked, tc.wantLicense)
		}
	}
}
