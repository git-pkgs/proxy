package config

import (
	"github.com/git-pkgs/proxy/internal/config/cargo"
	"github.com/git-pkgs/proxy/internal/config/debian"
)

// Ecosystem configuration (routes and upstreams)
type EcosystemConfig struct {
	Cargo  cargo.Config  `json:"cargo" yaml:"cargo"`
	Debian debian.Config `json:"debian" yaml:"debian"`
}
