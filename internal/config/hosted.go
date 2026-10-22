package config

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

const defaultHostedMaxUploadSize = "100MB"

// HostedConfig configures packages pushed to the proxy through the
// /api/hosted endpoints. Pushed versions are served in place of upstream
// versions with the same number, so writes always require a token.
type HostedConfig struct {
	// Enabled registers the /api/hosted endpoints. Off by default.
	Enabled bool `json:"enabled" yaml:"enabled"`

	// Token may push and delete any package. Enabled requires it or at least
	// one entry in Tokens. Supports ${VAR_NAME} expansion like AuthConfig
	// fields.
	Token string `json:"token" yaml:"token"`

	// Tokens are further tokens, each limited to the packages its Push
	// patterns name.
	Tokens []HostedToken `json:"tokens" yaml:"tokens"`

	// MaxUploadSize caps a single pushed archive (e.g., "100MB"). Must be > 0.
	// Default: "100MB".
	MaxUploadSize string `json:"max_upload_size" yaml:"max_upload_size"`

	// NPMScopes are npm scopes (e.g. "@acme") served only from pushed
	// packages. The upstream registry is never asked about them, so a package
	// published there under the same name can't be mixed in.
	NPMScopes []string `json:"npm_scopes" yaml:"npm_scopes"`

	// ComposerVendors are Composer vendors (e.g. "acme") served only from
	// pushed packages, like NPMScopes.
	ComposerVendors []string `json:"composer_vendors" yaml:"composer_vendors"`

	maxUploadBytes int64
}

// TokenExpanded returns Token with ${VAR_NAME} references expanded.
func (h *HostedConfig) TokenExpanded() string {
	return expandEnv(h.Token)
}

// MaxUploadBytes returns the parsed MaxUploadSize. Validate must run first.
func (h *HostedConfig) MaxUploadBytes() int64 {
	return h.maxUploadBytes
}

// Validate checks the hosted package settings, applying the default upload
// size if unset.
func (h *HostedConfig) Validate() error {
	if h.MaxUploadSize == "" {
		h.MaxUploadSize = defaultHostedMaxUploadSize
	}
	size, err := ParseSize(h.MaxUploadSize)
	if err != nil {
		return fmt.Errorf("invalid hosted.max_upload_size: %w", err)
	}
	if size <= 0 {
		return fmt.Errorf("invalid hosted.max_upload_size %q: must be > 0", h.MaxUploadSize)
	}
	h.maxUploadBytes = size

	if h.Enabled && h.TokenExpanded() == "" && len(h.Tokens) == 0 {
		return fmt.Errorf("hosted.token or hosted.tokens is required when hosted.enabled is true")
	}
	if err := h.validateTokens(); err != nil {
		return err
	}
	for _, scope := range h.NPMScopes {
		if len(scope) < 2 || scope[0] != '@' || strings.Contains(scope, "/") {
			return fmt.Errorf("invalid hosted.npm_scopes entry %q: must be a scope such as \"@acme\"", scope)
		}
	}
	for _, vendor := range h.ComposerVendors {
		if vendor == "" || strings.Contains(vendor, "/") {
			return fmt.Errorf("invalid hosted.composer_vendors entry %q: must be a vendor such as \"acme\"", vendor)
		}
	}
	return nil
}

// hostedTokenEcosystems are the ecosystems a hosted.tokens push pattern can
// name: those packages can be pushed for.
var hostedTokenEcosystems = []string{"npm", "composer", "generic"}

// HostedToken is a token that may push and delete only some packages.
type HostedToken struct {
	// Name identifies the token in logs.
	Name string `json:"name" yaml:"name"`

	// Token is the secret clients send. Supports ${VAR_NAME} expansion.
	Token string `json:"token" yaml:"token"`

	// Push lists the packages the token may push and delete, as
	// "{ecosystem}:{pattern}", e.g. "npm:@acme/*" or "composer:acme/*". The
	// pattern is matched against the package name with path.Match, so "*"
	// doesn't match "/".
	Push []string `json:"push" yaml:"push"`
}

// TokenExpanded returns Token with ${VAR_NAME} references expanded.
func (t *HostedToken) TokenExpanded() string {
	return expandEnv(t.Token)
}

// SplitHostedPushPattern splits "{ecosystem}:{pattern}".
func SplitHostedPushPattern(pattern string) (ecosystem, name string, ok bool) {
	ecosystem, name, ok = strings.Cut(pattern, ":")
	return ecosystem, name, ok && ecosystem != "" && name != ""
}

func (h *HostedConfig) validateTokens() error {
	names := make(map[string]bool, len(h.Tokens))
	secrets := map[string]bool{}
	if t := h.TokenExpanded(); t != "" {
		secrets[t] = true
	}
	for i, t := range h.Tokens {
		if t.Name == "" {
			return fmt.Errorf("hosted.tokens[%d]: name is required", i)
		}
		if names[t.Name] {
			return fmt.Errorf("hosted.tokens: name %q is used twice", t.Name)
		}
		names[t.Name] = true
		secret := t.TokenExpanded()
		if secret == "" {
			return fmt.Errorf("hosted.tokens %q: token is required", t.Name)
		}
		if secrets[secret] {
			return fmt.Errorf("hosted.tokens %q: token is also used by another token", t.Name)
		}
		secrets[secret] = true
		if len(t.Push) == 0 {
			return fmt.Errorf("hosted.tokens %q: push needs at least one pattern", t.Name)
		}
		for _, pattern := range t.Push {
			ecosystem, name, ok := SplitHostedPushPattern(pattern)
			if !ok {
				return fmt.Errorf("hosted.tokens %q: push pattern %q must be {ecosystem}:{pattern}", t.Name, pattern)
			}
			if !slices.Contains(hostedTokenEcosystems, ecosystem) {
				return fmt.Errorf("hosted.tokens %q: push pattern %q: ecosystem must be one of %s",
					t.Name, pattern, strings.Join(hostedTokenEcosystems, ", "))
			}
			if _, err := path.Match(name, ""); err != nil {
				return fmt.Errorf("hosted.tokens %q: push pattern %q: %w", t.Name, pattern, err)
			}
		}
	}
	return nil
}
