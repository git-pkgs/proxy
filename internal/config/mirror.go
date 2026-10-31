package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/git-pkgs/purl"
)

const minMirrorScheduleInterval = time.Minute

// MirrorConfig configures mirroring that the server runs on its own.
type MirrorConfig struct {
	// Schedules re-mirror container image tags at a fixed interval, so a
	// moved tag is picked up even when nobody pulls it.
	Schedules []MirrorSchedule `json:"schedules" yaml:"schedules"`

	// Refresh, when set, keeps the most used packages up to date in the
	// background.
	Refresh *MirrorRefresh `json:"refresh" yaml:"refresh"`
}

const (
	defaultMirrorRefreshTop   = 50
	defaultMirrorRefreshSince = 14 * 24 * time.Hour
)

// MirrorRefresh mirrors new releases of the packages clients use most, so
// they are cached before the first client asks for them.
type MirrorRefresh struct {
	// Interval is how often the refresh runs, at least one minute (e.g.
	// "24h").
	Interval string `json:"interval" yaml:"interval"`

	// Top is how many packages per ecosystem are refreshed, the ones with
	// the most downloads among those used within Since. Default: 50.
	Top int `json:"top" yaml:"top"`

	// Since is how recently a package must have been downloaded to be
	// refreshed (e.g. "14d"). Default: 14d.
	Since string `json:"since" yaml:"since"`

	// MaxVersions caps how many new versions one run mirrors. 0 means no
	// cap.
	MaxVersions int `json:"max_versions" yaml:"max_versions"`

	// MaxBytes caps how much one run downloads (e.g. "2GB"). Empty means no
	// cap.
	MaxBytes string `json:"max_bytes" yaml:"max_bytes"`

	interval time.Duration
	since    time.Duration
	maxBytes int64
}

// IntervalDuration returns the parsed Interval. Validate must run first.
func (r *MirrorRefresh) IntervalDuration() time.Duration { return r.interval }

// TopPackages returns Top, or its default.
func (r *MirrorRefresh) TopPackages() int {
	if r.Top == 0 {
		return defaultMirrorRefreshTop
	}
	return r.Top
}

// SinceDuration returns the parsed Since, or its default. Validate must run
// first.
func (r *MirrorRefresh) SinceDuration() time.Duration { return r.since }

// MaxVersionsPerRun returns MaxVersions; 0 means no cap.
func (r *MirrorRefresh) MaxVersionsPerRun() int { return r.MaxVersions }

// MaxBytesPerRun returns the parsed MaxBytes; 0 means no cap. Validate must
// run first.
func (r *MirrorRefresh) MaxBytesPerRun() int64 { return r.maxBytes }

func (r *MirrorRefresh) validate() error {
	d, err := time.ParseDuration(r.Interval)
	if err != nil {
		return fmt.Errorf("invalid mirror.refresh.interval %q: %w", r.Interval, err)
	}
	if d < minMirrorScheduleInterval {
		return fmt.Errorf("invalid mirror.refresh.interval %q: must be at least %s", r.Interval, minMirrorScheduleInterval)
	}
	r.interval = d
	if r.Top < 0 {
		return fmt.Errorf("invalid mirror.refresh.top %d: must not be negative", r.Top)
	}
	r.since = defaultMirrorRefreshSince
	if r.Since != "" {
		since, err := parseDayDuration(r.Since)
		if err != nil {
			return fmt.Errorf("invalid mirror.refresh.since %q: %w", r.Since, err)
		}
		if since <= 0 {
			return fmt.Errorf("invalid mirror.refresh.since %q: must be positive", r.Since)
		}
		r.since = since
	}
	if r.MaxVersions < 0 {
		return fmt.Errorf("invalid mirror.refresh.max_versions %d: must not be negative", r.MaxVersions)
	}
	r.maxBytes = 0
	if r.MaxBytes != "" {
		n, err := ParseSize(r.MaxBytes)
		if err != nil {
			return fmt.Errorf("invalid mirror.refresh.max_bytes %q: %w", r.MaxBytes, err)
		}
		r.maxBytes = n
	}
	return nil
}

// MirrorSchedule is a set of image PURLs mirrored every Interval.
type MirrorSchedule struct {
	// Images are pkg:docker or pkg:oci PURLs with a tag.
	Images []string `json:"images" yaml:"images"`

	// Interval is how often the images are mirrored, at least one minute
	// (e.g. "6h").
	Interval string `json:"interval" yaml:"interval"`

	// Platforms limit which platforms of an image index are mirrored, as
	// os/architecture[/variant]. Empty mirrors every platform.
	Platforms []string `json:"platforms" yaml:"platforms"`

	interval time.Duration
}

// IntervalDuration returns the parsed Interval. Validate must run first.
func (s *MirrorSchedule) IntervalDuration() time.Duration {
	return s.interval
}

// Validate checks every schedule.
func (m *MirrorConfig) Validate() error {
	for i := range m.Schedules {
		if err := m.Schedules[i].validate(fmt.Sprintf("mirror.schedules[%d]", i)); err != nil {
			return err
		}
	}
	if m.Refresh != nil {
		return m.Refresh.validate()
	}
	return nil
}

func (s *MirrorSchedule) validate(field string) error {
	d, err := time.ParseDuration(s.Interval)
	if err != nil {
		return fmt.Errorf("invalid %s.interval %q: %w", field, s.Interval, err)
	}
	if d < minMirrorScheduleInterval {
		return fmt.Errorf("invalid %s.interval %q: must be at least %s", field, s.Interval, minMirrorScheduleInterval)
	}
	s.interval = d

	if len(s.Images) == 0 {
		return fmt.Errorf("%s.images must not be empty", field)
	}
	for _, image := range s.Images {
		if err := validateScheduledImage(image); err != nil {
			return fmt.Errorf("invalid %s.images entry %q: %w", field, image, err)
		}
	}
	for _, platform := range s.Platforms {
		if err := validatePlatform(platform); err != nil {
			return fmt.Errorf("invalid %s.platforms entry %q: %w", field, platform, err)
		}
	}
	return nil
}

func validateScheduledImage(image string) error {
	p, err := purl.Parse(image)
	if err != nil {
		return err
	}
	if p.Type != "docker" && p.Type != "oci" {
		return fmt.Errorf("only pkg:docker and pkg:oci images can be scheduled")
	}
	if p.Version == "" {
		return fmt.Errorf("a tag is required")
	}
	repositoryURL := p.Qualifiers.Map()["repository_url"]
	if p.Type == "oci" && repositoryURL == "" {
		return fmt.Errorf("pkg:oci needs a repository_url qualifier")
	}
	// Only pkg:oci mirrors the tag qualifier next to the digest.
	pinned := strings.HasPrefix(p.Version, "sha256:")
	if pinned && (p.Type == "docker" || p.Qualifiers.Map()["tag"] == "") {
		return fmt.Errorf("a digest never moves, so there is nothing to refresh; use a tag")
	}
	return nil
}

// validatePlatform matches handler.ParsePlatform, which config cannot import.
func validatePlatform(value string) error {
	os, rest, _ := strings.Cut(value, "/")
	architecture, variant, _ := strings.Cut(rest, "/")
	if os == "" || architecture == "" || strings.Contains(variant, "/") || strings.Contains(value, ",") {
		return fmt.Errorf("want os/architecture[/variant]")
	}
	return nil
}
