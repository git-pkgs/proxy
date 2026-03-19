package mirror

import (
	"context"
	"fmt"
)

// RegistrySource enumerates all packages in a registry for full mirroring.
type RegistrySource struct {
	Ecosystem string
}

// supportedRegistries lists ecosystems that support enumeration.
var supportedRegistries = map[string]bool{
	"npm":   true,
	"pypi":  true,
	"cargo": true,
}

func (s *RegistrySource) Enumerate(ctx context.Context, fn func(PackageVersion) error) error {
	if !supportedRegistries[s.Ecosystem] {
		return fmt.Errorf("registry enumeration not supported for ecosystem %q; supported: npm, pypi, cargo", s.Ecosystem)
	}

	switch s.Ecosystem {
	case "npm":
		return s.enumerateNPM(ctx, fn)
	case "pypi":
		return s.enumeratePyPI(ctx, fn)
	case "cargo":
		return s.enumerateCargo(ctx, fn)
	default:
		return fmt.Errorf("unsupported ecosystem: %s", s.Ecosystem)
	}
}

func (s *RegistrySource) enumerateNPM(_ context.Context, _ func(PackageVersion) error) error {
	return fmt.Errorf("npm registry enumeration not yet implemented")
}

func (s *RegistrySource) enumeratePyPI(_ context.Context, _ func(PackageVersion) error) error {
	return fmt.Errorf("pypi registry enumeration not yet implemented")
}

func (s *RegistrySource) enumerateCargo(_ context.Context, _ func(PackageVersion) error) error {
	return fmt.Errorf("cargo registry enumeration not yet implemented")
}
