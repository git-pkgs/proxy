package handler

import (
	"log/slog"
	"testing"
)

func TestMavenParsePath(t *testing.T) {
	h := &MavenHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		path         string
		wantGroup    string
		wantArtifact string
		wantVersion  string
		wantFilename string
	}{
		{
			"com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar",
			"com.google.guava", "guava", "32.1.3-jre", "guava-32.1.3-jre.jar",
		},
		{
			"org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.jar",
			"org.apache.commons", "commons-lang3", "3.14.0", "commons-lang3-3.14.0.jar",
		},
		{
			"junit/junit/4.13.2/junit-4.13.2.jar",
			"junit", "junit", "4.13.2", "junit-4.13.2.jar",
		},
		{
			"short/path",
			"", "", "", "",
		},
	}

	for _, tt := range tests {
		group, artifact, version, filename := h.parsePath(tt.path)
		if group != tt.wantGroup || artifact != tt.wantArtifact || version != tt.wantVersion || filename != tt.wantFilename {
			t.Errorf("parsePath(%q) = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
				tt.path, group, artifact, version, filename,
				tt.wantGroup, tt.wantArtifact, tt.wantVersion, tt.wantFilename)
		}
	}
}

func TestMavenIsArtifactFile(t *testing.T) {
	h := &MavenHandler{}

	tests := []struct {
		filename string
		want     bool
	}{
		{"guava-32.1.3-jre.jar", true},
		{"guava-32.1.3-jre.pom", true},
		{"guava-32.1.3-jre.module", true},
		{"app-1.0.war", true},
		{"lib-1.0.aar", true},
		{"maven-metadata.xml", false},
		{"guava-32.1.3-jre.jar.sha1", false},
	}

	for _, tt := range tests {
		got := h.isArtifactFile(tt.filename)
		if got != tt.want {
			t.Errorf("isArtifactFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestMavenIsMetadataFile(t *testing.T) {
	h := &MavenHandler{}

	tests := []struct {
		name     string
		filename string
		want     bool
	}{
		{
			name:     "pom is artifact, not metadata",
			filename: "com.diffplug.spotless.gradle.plugin-8.4.0.pom",
			want:     false,
		},
		{
			name:     "pom checksum is metadata",
			filename: "com.diffplug.spotless.gradle.plugin-8.4.0.pom.sha1",
			want:     true,
		},
		{
			name:     "metadata file",
			filename: "maven-metadata.xml",
			want:     true,
		},
		{
			name:     "metadata checksum",
			filename: "maven-metadata.xml.sha256",
			want:     true,
		},
		{
			name:     "jar checksum is metadata",
			filename: "guava-32.1.3-jre.jar.sha1",
			want:     true,
		},
		{
			name:     "asc signature is metadata",
			filename: "guava-32.1.3-jre.jar.asc",
			want:     true,
		},
		{
			name:     "regular jar is not metadata",
			filename: "guava-32.1.3-jre.jar",
			want:     false,
		},
		{
			name:     "pom checksum is metadata",
			filename: "guava-32.1.3-jre.pom.sha1",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := h.isMetadataFile(tt.filename)
			if got != tt.want {
				t.Errorf("isMetadataFile(%q) = %v, want %v", tt.filename, got, tt.want)
			}
		})
	}
}
