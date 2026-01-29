package database

import (
	"database/sql"
	"time"
)

type Package struct {
	ID                int64          `db:"id" json:"id"`
	PURL              string         `db:"purl" json:"purl"`
	Ecosystem         string         `db:"ecosystem" json:"ecosystem"`
	Name              string         `db:"name" json:"name"`
	Namespace         sql.NullString `db:"namespace" json:"namespace,omitempty"`
	LatestVersion     sql.NullString `db:"latest_version" json:"latest_version,omitempty"`
	License           sql.NullString `db:"license" json:"license,omitempty"`
	Description       sql.NullString `db:"description" json:"description,omitempty"`
	Homepage          sql.NullString `db:"homepage" json:"homepage,omitempty"`
	RepositoryURL     sql.NullString `db:"repository_url" json:"repository_url,omitempty"`
	UpstreamURL       string         `db:"upstream_url" json:"upstream_url"`
	MetadataFetchedAt sql.NullTime   `db:"metadata_fetched_at" json:"metadata_fetched_at,omitempty"`
	CreatedAt         time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time      `db:"updated_at" json:"updated_at"`
}

type Version struct {
	ID                int64          `db:"id" json:"id"`
	PURL              string         `db:"purl" json:"purl"`
	PackageID         int64          `db:"package_id" json:"package_id"`
	Version           string         `db:"version" json:"version"`
	License           sql.NullString `db:"license" json:"license,omitempty"`
	Integrity         sql.NullString `db:"integrity" json:"integrity,omitempty"`
	PublishedAt       sql.NullTime   `db:"published_at" json:"published_at,omitempty"`
	Yanked            bool           `db:"yanked" json:"yanked"`
	MetadataFetchedAt sql.NullTime   `db:"metadata_fetched_at" json:"metadata_fetched_at,omitempty"`
	CreatedAt         time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time      `db:"updated_at" json:"updated_at"`
}

type Artifact struct {
	ID             int64          `db:"id" json:"id"`
	VersionID      int64          `db:"version_id" json:"version_id"`
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

func (a *Artifact) IsCached() bool {
	return a.StoragePath.Valid && a.FetchedAt.Valid
}
