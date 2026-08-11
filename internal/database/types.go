package database

import (
	"database/sql"
	"net/url"
	"strings"
	"time"
)

// Package represents a package in the database.
// Schema is compatible with git-pkgs.
type Package struct {
	ID            int64          `db:"id" json:"id"`
	PURL          string         `db:"purl" json:"purl"`
	Ecosystem     string         `db:"ecosystem" json:"ecosystem"`
	Name          string         `db:"name" json:"name"`
	LatestVersion sql.NullString `db:"latest_version" json:"latest_version,omitempty"`
	License       sql.NullString `db:"license" json:"license,omitempty"`
	Description   sql.NullString `db:"description" json:"description,omitempty"`
	Homepage      sql.NullString `db:"homepage" json:"homepage,omitempty"`
	RepositoryURL sql.NullString `db:"repository_url" json:"repository_url,omitempty"`
	RegistryURL   sql.NullString `db:"registry_url" json:"registry_url,omitempty"`
	SupplierName  sql.NullString `db:"supplier_name" json:"supplier_name,omitempty"`
	SupplierType  sql.NullString `db:"supplier_type" json:"supplier_type,omitempty"`
	Source        sql.NullString `db:"source" json:"source,omitempty"`
	EnrichedAt    sql.NullTime   `db:"enriched_at" json:"enriched_at,omitempty"`
	VulnsSyncedAt sql.NullTime   `db:"vulns_synced_at" json:"vulns_synced_at,omitempty"`
	CreatedAt     time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time      `db:"updated_at" json:"updated_at"`
}

// Version represents a package version in the database.
// Schema is compatible with git-pkgs.
type Version struct {
	ID          int64          `db:"id" json:"id"`
	PURL        string         `db:"purl" json:"purl"`
	PackagePURL string         `db:"package_purl" json:"package_purl"`
	License     sql.NullString `db:"license" json:"license,omitempty"`
	PublishedAt sql.NullTime   `db:"published_at" json:"published_at,omitempty"`
	Integrity   sql.NullString `db:"integrity" json:"integrity,omitempty"`
	Yanked      bool           `db:"yanked" json:"yanked"`
	Source      sql.NullString `db:"source" json:"source,omitempty"`
	EnrichedAt  sql.NullTime   `db:"enriched_at" json:"enriched_at,omitempty"`
	CreatedAt   time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time      `db:"updated_at" json:"updated_at"`
}

// Version extracts the version string from the PURL.
// e.g., "pkg:npm/lodash@4.17.21" -> "4.17.21"
func (v *Version) Version() string {
	return VersionFromPURL(v.PURL)
}

// DisplayPURL returns the PURL with its path components percent-decoded, for
// showing in the UI. The stored PURL keeps the canonical encoding (which is
// what the API and all lookups use); this is only a readable rendering, so that
// a version like "7.91+dfsg1-2ubuntu0.1" is not shown as "7.91%2Bdfsg1-2ubuntu0.1"
// and an npm scope is shown as "@babel" rather than "%40babel". Qualifiers and
// subpath keep their encoding, since decoding those would be ambiguous.
func (v *Version) DisplayPURL() string {
	base, suffix := v.PURL, ""
	if i := strings.IndexAny(base, "?#"); i >= 0 {
		base, suffix = base[:i], base[i:]
	}

	name, version := base, ""
	if idx := strings.LastIndex(base, "@"); idx >= 0 {
		name, version = base[:idx], "@"+decodePURLComponent(base[idx+1:])
	}

	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = decodePURLComponent(part)
	}
	return strings.Join(parts, "/") + version + suffix
}

// VersionFromPURL extracts the decoded version string from a PURL.
//
// PURL percent-encodes characters that are not safe in a path component, so a
// Debian version like "7.91+dfsg1-2ubuntu0.1" is stored as
// "pkg:deb/nmap@7.91%2Bdfsg1-2ubuntu0.1". The raw substring after "@" is
// therefore not the version: it must be percent-decoded before being displayed
// or used to build a URL, otherwise "%2B" leaks into the UI and round-tripping
// the value back into a PURL double-encodes it.
//
// e.g., "pkg:npm/lodash@4.17.21" -> "4.17.21"
func VersionFromPURL(p string) string {
	// Qualifiers ("?key=value") and subpath ("#path") follow the version.
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	idx := strings.LastIndex(p, "@")
	if idx < 0 {
		return ""
	}
	return decodePURLComponent(p[idx+1:])
}

// decodePURLComponent percent-decodes a single PURL path component, returning
// the input unchanged if it is not valid percent-encoding.
func decodePURLComponent(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	decoded, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return decoded
}

// Artifact represents a cached artifact in the database.
// This table is proxy-specific and not part of git-pkgs.
type Artifact struct {
	ID             int64          `db:"id" json:"id"`
	VersionPURL    string         `db:"version_purl" json:"version_purl"`
	Filename       string         `db:"filename" json:"filename"`
	UpstreamURL    string         `db:"upstream_url" json:"upstream_url"`
	StoragePath    sql.NullString `db:"storage_path" json:"storage_path,omitempty"`
	ContentHash    sql.NullString `db:"content_hash" json:"content_hash,omitempty"`
	Size           sql.NullInt64  `db:"size" json:"size,omitempty"`
	ContentType    sql.NullString `db:"content_type" json:"content_type,omitempty"`
	FetchedAt      sql.NullTime   `db:"fetched_at" json:"fetched_at,omitempty"`
	HitCount       int64          `db:"hit_count" json:"hit_count"`
	LastAccessedAt sql.NullTime   `db:"last_accessed_at" json:"last_accessed_at,omitempty"`
	CreatedAt      time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time      `db:"updated_at" json:"updated_at"`
}

// IsCached returns true if the artifact has been fetched and stored locally.
func (a *Artifact) IsCached() bool {
	return a.StoragePath.Valid && a.FetchedAt.Valid
}

// MetadataCacheEntry represents a cached metadata blob for offline serving.
type MetadataCacheEntry struct {
	ID           int64          `db:"id" json:"id"`
	Ecosystem    string         `db:"ecosystem" json:"ecosystem"`
	Name         string         `db:"name" json:"name"`
	StoragePath  string         `db:"storage_path" json:"storage_path"`
	ETag         sql.NullString `db:"etag" json:"etag,omitempty"`
	ContentType  sql.NullString `db:"content_type" json:"content_type,omitempty"`
	Size         sql.NullInt64  `db:"size" json:"size,omitempty"`
	LastModified sql.NullTime   `db:"last_modified" json:"last_modified,omitempty"`
	FetchedAt    sql.NullTime   `db:"fetched_at" json:"fetched_at,omitempty"`
	CreatedAt    time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time      `db:"updated_at" json:"updated_at"`
}

// Vulnerability represents a cached vulnerability record.
type Vulnerability struct {
	ID           int64           `db:"id" json:"id"`
	VulnID       string          `db:"vuln_id" json:"vuln_id"`
	Ecosystem    string          `db:"ecosystem" json:"ecosystem"`
	PackageName  string          `db:"package_name" json:"package_name"`
	Severity     sql.NullString  `db:"severity" json:"severity,omitempty"`
	Summary      sql.NullString  `db:"summary" json:"summary,omitempty"`
	FixedVersion sql.NullString  `db:"fixed_version" json:"fixed_version,omitempty"`
	CVSSScore    sql.NullFloat64 `db:"cvss_score" json:"cvss_score,omitempty"`
	References   sql.NullString  `db:"references" json:"references,omitempty"`
	FetchedAt    sql.NullTime    `db:"fetched_at" json:"fetched_at,omitempty"`
	CreatedAt    time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time       `db:"updated_at" json:"updated_at"`
}
