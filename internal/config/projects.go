package config

import (
	"fmt"
	"time"
)

// ProjectsConfig configures the project inventory: repositories upload an
// SBOM to /api/projects, and the proxy shows which of them use which package
// versions.
type ProjectsConfig struct {
	// Enabled registers the /api/projects endpoints and the UI pages. Off by
	// default.
	Enabled bool `json:"enabled" yaml:"enabled"`

	// Token is the bearer token required to upload and delete projects.
	// Required when Enabled. Supports ${VAR_NAME} expansion like AuthConfig
	// fields.
	Token string `json:"token" yaml:"token"`

	// MaxAge removes projects that have not uploaded an SBOM for this long
	// (e.g., "90d"). Empty or "0" keeps them until they are deleted.
	MaxAge string `json:"max_age" yaml:"max_age"`

	maxAge time.Duration
}

// TokenExpanded returns Token with ${VAR_NAME} references expanded.
func (p *ProjectsConfig) TokenExpanded() string {
	return expandEnv(p.Token)
}

// MaxAgeDuration returns the parsed MaxAge, 0 to keep projects. Validate
// must run first.
func (p *ProjectsConfig) MaxAgeDuration() time.Duration {
	return p.maxAge
}

// Validate checks the project inventory settings.
func (p *ProjectsConfig) Validate() error {
	p.maxAge = 0
	if p.MaxAge != "" {
		d, err := parseRetentionDuration("projects.max_age", p.MaxAge)
		if err != nil {
			return err
		}
		p.maxAge = d
	}
	if p.Enabled && p.TokenExpanded() == "" {
		return fmt.Errorf("projects.token is required when projects.enabled is true")
	}
	return nil
}
