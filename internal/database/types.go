package database

import (
	"database/sql"
	"time"
)

type Package struct {
	ID                int64          `json:"id"`
	PURL              string         `json:"purl"`
	Ecosystem         string         `json:"ecosystem"`
	Name              string         `json:"name"`
	Namespace         sql.NullString `json:"namespace,omitempty"`
	LatestVersion     sql.NullString `json:"latest_version,omitempty"`
	License           sql.NullString `json:"license,omitempty"`
	Description       sql.NullString `json:"description,omitempty"`
	Homepage          sql.NullString `json:"homepage,omitempty"`
	RepositoryURL     sql.NullString `json:"repository_url,omitempty"`
	UpstreamURL       string         `json:"upstream_url"`
	MetadataFetchedAt sql.NullTime   `json:"metadata_fetched_at,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

type Version struct {
	ID                int64          `json:"id"`
	PURL              string         `json:"purl"`
	PackageID         int64          `json:"package_id"`
	Version           string         `json:"version"`
	License           sql.NullString `json:"license,omitempty"`
	Integrity         sql.NullString `json:"integrity,omitempty"`
	PublishedAt       sql.NullTime   `json:"published_at,omitempty"`
	Yanked            bool           `json:"yanked"`
	MetadataFetchedAt sql.NullTime   `json:"metadata_fetched_at,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

type Artifact struct {
	ID             int64          `json:"id"`
	VersionID      int64          `json:"version_id"`
	Filename       string         `json:"filename"`
	UpstreamURL    string         `json:"upstream_url"`
	StoragePath    sql.NullString `json:"storage_path,omitempty"`
	ContentHash    sql.NullString `json:"content_hash,omitempty"`
	Size           sql.NullInt64  `json:"size,omitempty"`
	ContentType    sql.NullString `json:"content_type,omitempty"`
	FetchedAt      sql.NullTime   `json:"fetched_at,omitempty"`
	HitCount       int64          `json:"hit_count"`
	LastAccessedAt sql.NullTime   `json:"last_accessed_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// IsCached returns true if the artifact has been fetched and stored locally.
func (a *Artifact) IsCached() bool {
	return a.StoragePath.Valid && a.FetchedAt.Valid
}
