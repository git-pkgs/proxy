package mirror

import (
	"context"
	"testing"
)

func TestRegistrySourceUnsupported(t *testing.T) {
	source := &RegistrySource{Ecosystem: "golang"}
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for unsupported ecosystem")
	}
}

func TestRegistrySourceNPMNotImplemented(t *testing.T) {
	source := &RegistrySource{Ecosystem: "npm"}
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected not-implemented error")
	}
}

func TestRegistrySourcePyPINotImplemented(t *testing.T) {
	source := &RegistrySource{Ecosystem: "pypi"}
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected not-implemented error")
	}
}

func TestRegistrySourceCargoNotImplemented(t *testing.T) {
	source := &RegistrySource{Ecosystem: "cargo"}
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected not-implemented error")
	}
}
