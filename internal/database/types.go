package database

import (
	"database/sql"
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
	if idx := strings.LastIndex(v.PURL, "@"); idx >= 0 {
		return v.PURL[idx+1:]
	}
	return ""
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

// Vulnerability represents a cached vulnerability record.
type Vulnerability struct {
	ID           int64          `db:"id" json:"id"`
	VulnID       string         `db:"vuln_id" json:"vuln_id"`
	Ecosystem    string         `db:"ecosystem" json:"ecosystem"`
	PackageName  string         `db:"package_name" json:"package_name"`
	Severity     sql.NullString `db:"severity" json:"severity,omitempty"`
	Summary      sql.NullString `db:"summary" json:"summary,omitempty"`
	FixedVersion sql.NullString `db:"fixed_version" json:"fixed_version,omitempty"`
	CVSSScore    sql.NullFloat64 `db:"cvss_score" json:"cvss_score,omitempty"`
	References   sql.NullString `db:"references" json:"references,omitempty"`
	FetchedAt    sql.NullTime   `db:"fetched_at" json:"fetched_at,omitempty"`
	CreatedAt    time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time      `db:"updated_at" json:"updated_at"`
}
