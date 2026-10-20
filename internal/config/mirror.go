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
