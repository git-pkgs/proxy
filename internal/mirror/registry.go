package mirror

import (
	"context"
	"fmt"
)

// RegistrySource enumerates all packages in a registry for full mirroring.
// Registry enumeration is not yet implemented for any ecosystem.
type RegistrySource struct {
	Ecosystem string
}

func (s *RegistrySource) Enumerate(_ context.Context, _ func(PackageVersion) error) error {
	return fmt.Errorf("registry enumeration is not yet implemented for ecosystem %q", s.Ecosystem)
}
