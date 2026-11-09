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
		// Group directories never hold dots or are empty in the Maven
		// layout; these would spell the same group as org/foo.
		{"org.foo/bar/1.0/bar-1.0.jar", "", "", "", ""},
		{"org/.foo/bar/1.0/bar-1.0.jar", "", "", "", ""},
		{"org//foo/bar/1.0/bar-1.0.jar", "", "", "", ""},
		// Cache identity is group:artifact, so a colon in either would
		// let org:foo/bar and org/foo:bar share one entry.
		{"org:foo/bar/1.0/bar-1.0.jar", "", "", "", ""},
		{"org/foo:bar/1.0/bar-1.0.jar", "", "", "", ""},
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
		{"apache-maven-3.9.9-bin.tar.gz", true},
		{"apache-maven-3.9.9-bin.zip", true},
		{"maven-mvnd-1.0.6-linux-amd64.tgz", true},
		{"maven-metadata.xml", false},
		{"guava-32.1.3-jre.jar.sha1", false},
		{"apache-maven-3.9.9-bin.tar.gz.sha512", false},
	}

	for _, tt := range tests {
		got := h.isArtifactFile(tt.filename)
		if got != tt.want {
			t.Errorf("isArtifactFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestMavenIsVersionListingFile(t *testing.T) {
	h := &MavenHandler{}

	tests := []struct {
		filename string
		want     bool
	}{
		{"maven-metadata.xml", true},
		{"maven-metadata.xml.sha1", true},
		{"maven-metadata.xml.sha256", true},
		{"guava-32.1.3-jre.jar", false},
		{"guava-32.1.3-jre.jar.sha1", false},
		{"com.diffplug.spotless.gradle.plugin-8.4.0.pom", false},
	}

	for _, tt := range tests {
		if got := h.isVersionListingFile(tt.filename); got != tt.want {
			t.Errorf("isVersionListingFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestMavenIsChecksumFile(t *testing.T) {
	h := &MavenHandler{}

	tests := []struct {
		filename string
		want     bool
	}{
		{"guava-32.1.3-jre.jar.sha1", true},
		{"guava-32.1.3-jre.jar.md5", true},
		{"guava-32.1.3-jre.pom.sha1", true},
		{"guava-32.1.3-jre.jar.asc", true},
		{"apache-maven-3.9.9-bin.tar.gz.sha512", true},
		{"com.diffplug.spotless.gradle.plugin-8.4.0.pom.sha1", true},
		{"guava-32.1.3-jre.jar", false},
		{"guava-32.1.3-jre.pom", false},
		{"maven-metadata.xml", false},
	}

	for _, tt := range tests {
		if got := h.isChecksumFile(tt.filename); got != tt.want {
			t.Errorf("isChecksumFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestMavenIsCoordinateFile(t *testing.T) {
	h := &MavenHandler{}

	tests := []struct {
		path string
		want bool
	}{
		{"com/google/protobuf/protoc/3.21.0/protoc-3.21.0-linux-x86_64.exe", true},
		{"com/google/protobuf/protoc/3.21.0/protoc-3.21.0-linux-x86_64.exe.sha1", true},
		{"org/example/plugin/1.0/plugin-1.0.hpi", true},
		{"org/example/lib/1.0-SNAPSHOT/lib-1.0-20240101.120000-1.exe", true},
		{"org/example/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.exe", true},
		{"com/google/protobuf/protoc/3.21.0/", false},
		{"com/google/protobuf/protoc/3.21.0/other-3.21.0.exe", false},
		{"com/google/protobuf/protoc/3.21.0/protoc-3.20.0.exe", false},
		{"archetype-catalog.xml", false},
	}

	for _, tt := range tests {
		if got := h.isCoordinateFile(tt.path); got != tt.want {
			t.Errorf("isCoordinateFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
