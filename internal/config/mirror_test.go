package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMirrorConfigValidate(t *testing.T) {
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	tests := []struct {
		name     string
		schedule MirrorSchedule
		wantErr  bool
	}{
		{name: "docker tags", schedule: MirrorSchedule{Images: []string{"pkg:docker/library/alpine@3.20", "pkg:docker/nginx@stable"}, Interval: "6h"}},
		{name: "oci with tag", schedule: MirrorSchedule{Images: []string{"pkg:oci/app@v1?repository_url=ghcr.io/acme/app"}, Interval: "1m"}},
		{name: "oci digest with tag", schedule: MirrorSchedule{Images: []string{"pkg:oci/app@" + digest + "?repository_url=ghcr.io/acme/app&tag=v1"}, Interval: "1h"}},
		{name: "platforms", schedule: MirrorSchedule{Images: []string{"pkg:docker/alpine@3"}, Interval: "1h", Platforms: []string{"linux/amd64", "linux/arm64/v8"}}},
		{name: "missing interval", schedule: MirrorSchedule{Images: []string{"pkg:docker/alpine@3"}}, wantErr: true},
		{name: "bad interval", schedule: MirrorSchedule{Images: []string{"pkg:docker/alpine@3"}, Interval: "often"}, wantErr: true},
		{name: "interval under a minute", schedule: MirrorSchedule{Images: []string{"pkg:docker/alpine@3"}, Interval: "30s"}, wantErr: true},
		{name: "no images", schedule: MirrorSchedule{Interval: "1h"}, wantErr: true},
		{name: "unparsable purl", schedule: MirrorSchedule{Images: []string{"alpine:3"}, Interval: "1h"}, wantErr: true},
		{name: "not an image", schedule: MirrorSchedule{Images: []string{"pkg:npm/lodash@4.17.21"}, Interval: "1h"}, wantErr: true},
		{name: "no tag", schedule: MirrorSchedule{Images: []string{"pkg:docker/alpine"}, Interval: "1h"}, wantErr: true},
		{name: "docker digest", schedule: MirrorSchedule{Images: []string{"pkg:docker/alpine@" + digest}, Interval: "1h"}, wantErr: true},
		{name: "docker digest with tag qualifier", schedule: MirrorSchedule{Images: []string{"pkg:docker/alpine@" + digest + "?tag=3"}, Interval: "1h"}, wantErr: true},
		{name: "oci digest only", schedule: MirrorSchedule{Images: []string{"pkg:oci/app@" + digest + "?repository_url=ghcr.io/acme/app"}, Interval: "1h"}, wantErr: true},
		{name: "oci without repository_url", schedule: MirrorSchedule{Images: []string{"pkg:oci/app@v1"}, Interval: "1h"}, wantErr: true},
		{name: "platform without architecture", schedule: MirrorSchedule{Images: []string{"pkg:docker/alpine@3"}, Interval: "1h", Platforms: []string{"linux"}}, wantErr: true},
		{name: "platform list in one entry", schedule: MirrorSchedule{Images: []string{"pkg:docker/alpine@3"}, Interval: "1h", Platforms: []string{"linux/amd64,linux/arm64"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := MirrorConfig{Schedules: []MirrorSchedule{tt.schedule}}
			err := m.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMirrorScheduleIntervalDuration(t *testing.T) {
	m := MirrorConfig{Schedules: []MirrorSchedule{{Images: []string{"pkg:docker/alpine@3"}, Interval: "6h"}}}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := m.Schedules[0].IntervalDuration(); got != 6*time.Hour {
		t.Errorf("IntervalDuration() = %v, want 6h", got)
	}
}

func TestValidateRejectsMirrorSchedulesWithoutCachedArtifacts(t *testing.T) {
	cfg := Default()
	cfg.Mirror.Schedules = []MirrorSchedule{{Images: []string{"pkg:docker/alpine@3"}, Interval: "1h"}}
	cfg.Storage.CacheArtifacts = false
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() accepted mirror.schedules with storage.cache_artifacts: false")
	}
}

func TestLoadYAMLMirrorSchedules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `
mirror:
  schedules:
    - images: [pkg:docker/library/alpine@3.20]
      interval: 6h
      platforms: [linux/amd64]
    - images: [pkg:docker/nginx@stable]
      interval: 30s
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Mirror.Schedules) != 2 || cfg.Mirror.Schedules[0].Platforms[0] != "linux/amd64" {
		t.Fatalf("schedules = %+v", cfg.Mirror.Schedules)
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() accepted a 30s interval in the second schedule")
	}
}
