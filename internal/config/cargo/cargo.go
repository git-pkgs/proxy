package cargo

import (
	"fmt"
	"net/url"
)

// Config configures routes
type Config struct {
	IncludeDefault bool          `json:"include_default" yaml:"include_default"`
	Route          []RouteConfig `json:"route" yaml:"route"`
}

// RouteConfig configures a route
type RouteConfig struct {
	Path     string           `json:"path" yaml:"path"`
	Upstream []UpstreamConfig `json:"upstream" yaml:"upstream"`
}

// UpstreamConfig configures an upstream (source)
type UpstreamConfig struct {
	Name   string `json:"name" yaml:"name"`
	Index  string `json:"index" yaml:"index"`
	Crates string `json:"crates" yaml:"crates"`
}

// RouteDefault is the default route
var RouteDefault = RouteConfig{
	Path: "/cargo",
	Upstream: []UpstreamConfig{
		{
			Name:   "crates.io",
			Index:  "https://index.crates.io",
			Crates: "https://static.crates.io/crates",
		},
	},
}

func (c *Config) Validate() error {
	for _, route := range c.Route {
		if err := route.Validate(); err != nil {
			return err
		}
	}

	return nil
}

func (r *RouteConfig) Validate() error {
	// TODO: validate Path

	if len(r.Upstream) == 0 {
		return fmt.Errorf("cargo route %q does not have any upstreams", r.Path)
	}
	if len(r.Upstream) > 1 {
		return fmt.Errorf("cargo route %q has multiple upstreams; this is not yet supported", r.Path)
	}

	for _, upstream := range r.Upstream {
		if err := upstream.Validate(); err != nil {
			return err
		}
	}

	return nil
}

func (u *UpstreamConfig) Validate() error {
	if _, err := url.Parse(u.Index); err != nil {
		return fmt.Errorf("cargo upstream index %q is not a valid URL", u.Index)
	}

	if _, err := url.Parse(u.Crates); err != nil {
		return fmt.Errorf("cargo upstream crates %q is not a valid URL", u.Crates)
	}

	return nil
}
