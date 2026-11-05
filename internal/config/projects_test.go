package config

import (
	"testing"
	"time"
)

func TestProjectsConfigValidate(t *testing.T) {
	t.Setenv("PROJECTS_TEST_TOKEN", "from-env")

	tests := []struct {
		name     string
		projects ProjectsConfig
		wantErr  bool
		maxAge   time.Duration
	}{
		{name: "disabled without token", projects: ProjectsConfig{}},
		{name: "enabled without token", projects: ProjectsConfig{Enabled: true}, wantErr: true},
		{name: "enabled with token", projects: ProjectsConfig{Enabled: true, Token: "s3cret"}},
		{name: "token from env reference", projects: ProjectsConfig{Enabled: true, Token: "${PROJECTS_TEST_TOKEN}"}},
		{name: "token env reference unset", projects: ProjectsConfig{Enabled: true, Token: "${PROJECTS_TEST_UNSET}"}, wantErr: true},
		{name: "max age in days", projects: ProjectsConfig{Enabled: true, Token: "s3cret", MaxAge: "90d"}, maxAge: 90 * 24 * time.Hour},
		{name: "max age in hours", projects: ProjectsConfig{Enabled: true, Token: "s3cret", MaxAge: "36h"}, maxAge: 36 * time.Hour},
		{name: "max age zero keeps projects", projects: ProjectsConfig{Enabled: true, Token: "s3cret", MaxAge: "0"}},
		{name: "invalid max age", projects: ProjectsConfig{Enabled: true, Token: "s3cret", MaxAge: "soon"}, wantErr: true},
		{name: "negative max age", projects: ProjectsConfig{Enabled: true, Token: "s3cret", MaxAge: "-1d"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.projects.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got := tt.projects.MaxAgeDuration(); err == nil && got != tt.maxAge {
				t.Errorf("MaxAgeDuration() = %v, want %v", got, tt.maxAge)
			}
		})
	}
}

func TestLoadFromEnvProjects(t *testing.T) {
	cfg := Default()
	t.Setenv("PROXY_PROJECTS_ENABLED", "true")
	t.Setenv("PROXY_PROJECTS_TOKEN", "env-token")
	t.Setenv("PROXY_PROJECTS_MAX_AGE", "30d")

	cfg.LoadFromEnv()

	if !cfg.Projects.Enabled {
		t.Error("Projects.Enabled not loaded from PROXY_PROJECTS_ENABLED")
	}
	if cfg.Projects.TokenExpanded() != "env-token" {
		t.Errorf("Projects.Token = %q, want env-token", cfg.Projects.Token)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := cfg.Projects.MaxAgeDuration(); got != 30*24*time.Hour {
		t.Errorf("MaxAgeDuration() = %v, want 30 days", got)
	}
}

func TestValidateRejectsProjectsWithoutToken(t *testing.T) {
	cfg := Default()
	cfg.Projects = ProjectsConfig{Enabled: true}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() accepted projects.enabled without a token")
	}
}
