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

func TestMavenShouldFallbackToPluginPortal(t *testing.T) {
	h := &MavenHandler{}

	tests := []struct {
		name     string
		urlPath  string
		filename string
		want     bool
	}{
		{
			name:     "marker pom",
			urlPath:  "com/diffplug/spotless/com.diffplug.spotless.gradle.plugin/8.4.0/com.diffplug.spotless.gradle.plugin-8.4.0.pom",
			filename: "com.diffplug.spotless.gradle.plugin-8.4.0.pom",
			want:     true,
		},
		{
			name:     "marker pom checksum",
			urlPath:  "com/diffplug/spotless/com.diffplug.spotless.gradle.plugin/8.4.0/com.diffplug.spotless.gradle.plugin-8.4.0.pom.sha1",
			filename: "com.diffplug.spotless.gradle.plugin-8.4.0.pom.sha1",
			want:     true,
		},
		{
			name:     "marker metadata",
			urlPath:  "com/diffplug/spotless/com.diffplug.spotless.gradle.plugin/maven-metadata.xml",
			filename: "maven-metadata.xml",
			want:     true,
		},
		{
			name:     "marker metadata checksum",
			urlPath:  "com/diffplug/spotless/com.diffplug.spotless.gradle.plugin/maven-metadata.xml.sha256",
			filename: "maven-metadata.xml.sha256",
			want:     true,
		},
		{
			name:     "non marker pom checksum",
			urlPath:  "com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.pom.sha1",
			filename: "guava-32.1.3-jre.pom.sha1",
			want:     false,
		},
		{
			name:     "jar checksum",
			urlPath:  "com/diffplug/spotless/com.diffplug.spotless.gradle.plugin/8.4.0/com.diffplug.spotless.gradle.plugin-8.4.0.jar.sha1",
			filename: "com.diffplug.spotless.gradle.plugin-8.4.0.jar.sha1",
			want:     false,
		},
		{
			name:     "path too short",
			urlPath:  "com.diffplug.spotless.gradle.plugin/maven-metadata.xml",
			filename: "maven-metadata.xml",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := h.shouldFallbackToPluginPortal(tt.urlPath, tt.filename)
			if got != tt.want {
				t.Errorf("shouldFallbackToPluginPortal(%q, %q) = %v, want %v", tt.urlPath, tt.filename, got, tt.want)
			}
		})
	}
}
