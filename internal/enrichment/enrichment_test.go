package enrichment

import (
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

func TestNormalizeLicense(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := New(logger)

	tests := []struct {
		input    string
		expected string
	}{
		{"MIT", "MIT"},
		{"Apache 2", "Apache-2.0"},
		{"Apache-2.0", "Apache-2.0"},
		{"", ""},
	}

	for _, tc := range tests {
		result := svc.NormalizeLicense(tc.input)
		if result != tc.expected {
			t.Errorf("NormalizeLicense(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}
