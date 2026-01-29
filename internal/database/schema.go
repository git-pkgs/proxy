package database

import "fmt"

// Schema for proxy-specific tables. The packages and versions tables
// are compatible with git-pkgs, allowing the proxy to use an existing
// git-pkgs database as a starting point.

var schemaSQLite = `
CREATE TABLE IF NOT EXISTS schema_info (
	version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS packages (
	id INTEGER PRIMARY KEY,
	purl TEXT NOT NULL,
	ecosystem TEXT NOT NULL,
	name TEXT NOT NULL,
	latest_version TEXT,
	license TEXT,
	description TEXT,
	homepage TEXT,
	repository_url TEXT,
	registry_url TEXT,
	supplier_name TEXT,
	supplier_type TEXT,
	source TEXT,
	enriched_at DATETIME,
	vulns_synced_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_packages_purl ON packages(purl);
CREATE INDEX IF NOT EXISTS idx_packages_ecosystem_name ON packages(ecosystem, name);

CREATE TABLE IF NOT EXISTS versions (
	id INTEGER PRIMARY KEY,
	purl TEXT NOT NULL,
	package_purl TEXT NOT NULL,
	license TEXT,
	published_at DATETIME,
	integrity TEXT,
	yanked INTEGER DEFAULT 0,
	source TEXT,
	enriched_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_versions_purl ON versions(purl);
CREATE INDEX IF NOT EXISTS idx_versions_package_purl ON versions(package_purl);

CREATE TABLE IF NOT EXISTS artifacts (
	id INTEGER PRIMARY KEY,
	version_purl TEXT NOT NULL,
	filename TEXT NOT NULL,
	upstream_url TEXT NOT NULL,
	storage_path TEXT,
	content_hash TEXT,
	size INTEGER,
	content_type TEXT,
	fetched_at DATETIME,
	hit_count INTEGER DEFAULT 0,
	last_accessed_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_version_filename ON artifacts(version_purl, filename);
CREATE INDEX IF NOT EXISTS idx_artifacts_storage_path ON artifacts(storage_path);
CREATE INDEX IF NOT EXISTS idx_artifacts_last_accessed ON artifacts(last_accessed_at);

CREATE TABLE IF NOT EXISTS vulnerabilities (
	id INTEGER PRIMARY KEY,
	vuln_id TEXT NOT NULL,
	ecosystem TEXT NOT NULL,
	package_name TEXT NOT NULL,
	severity TEXT,
	summary TEXT,
	fixed_version TEXT,
	cvss_score REAL,
	"references" TEXT,
	fetched_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_vulns_id_pkg ON vulnerabilities(vuln_id, ecosystem, package_name);
CREATE INDEX IF NOT EXISTS idx_vulns_ecosystem_pkg ON vulnerabilities(ecosystem, package_name);
`

var schemaPostgres = `
CREATE TABLE IF NOT EXISTS schema_info (
	version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS packages (
	id SERIAL PRIMARY KEY,
	purl TEXT NOT NULL,
	ecosystem TEXT NOT NULL,
	name TEXT NOT NULL,
	latest_version TEXT,
	license TEXT,
	description TEXT,
	homepage TEXT,
	repository_url TEXT,
	registry_url TEXT,
	supplier_name TEXT,
	supplier_type TEXT,
	source TEXT,
	enriched_at TIMESTAMP,
	vulns_synced_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_packages_purl ON packages(purl);
CREATE INDEX IF NOT EXISTS idx_packages_ecosystem_name ON packages(ecosystem, name);

CREATE TABLE IF NOT EXISTS versions (
	id SERIAL PRIMARY KEY,
	purl TEXT NOT NULL,
	package_purl TEXT NOT NULL,
	license TEXT,
	published_at TIMESTAMP,
	integrity TEXT,
	yanked BOOLEAN DEFAULT FALSE,
	source TEXT,
	enriched_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_versions_purl ON versions(purl);
CREATE INDEX IF NOT EXISTS idx_versions_package_purl ON versions(package_purl);

CREATE TABLE IF NOT EXISTS artifacts (
	id SERIAL PRIMARY KEY,
	version_purl TEXT NOT NULL,
	filename TEXT NOT NULL,
	upstream_url TEXT NOT NULL,
	storage_path TEXT,
	content_hash TEXT,
	size BIGINT,
	content_type TEXT,
	fetched_at TIMESTAMP,
	hit_count BIGINT DEFAULT 0,
	last_accessed_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_version_filename ON artifacts(version_purl, filename);
CREATE INDEX IF NOT EXISTS idx_artifacts_storage_path ON artifacts(storage_path);
CREATE INDEX IF NOT EXISTS idx_artifacts_last_accessed ON artifacts(last_accessed_at);

CREATE TABLE IF NOT EXISTS vulnerabilities (
	id SERIAL PRIMARY KEY,
	vuln_id TEXT NOT NULL,
	ecosystem TEXT NOT NULL,
	package_name TEXT NOT NULL,
	severity TEXT,
	summary TEXT,
	fixed_version TEXT,
	cvss_score REAL,
	"references" TEXT,
	fetched_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_vulns_id_pkg ON vulnerabilities(vuln_id, ecosystem, package_name);
CREATE INDEX IF NOT EXISTS idx_vulns_ecosystem_pkg ON vulnerabilities(ecosystem, package_name);
`

// schemaArtifactsOnly contains just the artifacts table for adding to existing git-pkgs databases.
var schemaArtifactsSQLite = `
CREATE TABLE IF NOT EXISTS artifacts (
	id INTEGER PRIMARY KEY,
	version_purl TEXT NOT NULL,
	filename TEXT NOT NULL,
	upstream_url TEXT NOT NULL,
	storage_path TEXT,
	content_hash TEXT,
	size INTEGER,
	content_type TEXT,
	fetched_at DATETIME,
	hit_count INTEGER DEFAULT 0,
	last_accessed_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_version_filename ON artifacts(version_purl, filename);
CREATE INDEX IF NOT EXISTS idx_artifacts_storage_path ON artifacts(storage_path);
CREATE INDEX IF NOT EXISTS idx_artifacts_last_accessed ON artifacts(last_accessed_at);
`

var schemaArtifactsPostgres = `
CREATE TABLE IF NOT EXISTS artifacts (
	id SERIAL PRIMARY KEY,
	version_purl TEXT NOT NULL,
	filename TEXT NOT NULL,
	upstream_url TEXT NOT NULL,
	storage_path TEXT,
	content_hash TEXT,
	size BIGINT,
	content_type TEXT,
	fetched_at TIMESTAMP,
	hit_count BIGINT DEFAULT 0,
	last_accessed_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_version_filename ON artifacts(version_purl, filename);
CREATE INDEX IF NOT EXISTS idx_artifacts_storage_path ON artifacts(storage_path);
CREATE INDEX IF NOT EXISTS idx_artifacts_last_accessed ON artifacts(last_accessed_at);
`

func (db *DB) CreateSchema() error {
	if err := db.OptimizeForBulkWrites(); err != nil {
		return err
	}

	var schema string
	if db.dialect == DialectPostgres {
		schema = schemaPostgres
	} else {
		schema = schemaSQLite
	}

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("executing schema: %w", err)
	}

	query := db.Rebind("INSERT INTO schema_info (version) VALUES (?)")
	if _, err := db.Exec(query, SchemaVersion); err != nil {
		return fmt.Errorf("setting schema version: %w", err)
	}

	return db.OptimizeForReads()
}

// EnsureArtifactsTable adds the artifacts table to an existing database
// (e.g., a git-pkgs database) if it doesn't already exist.
func (db *DB) EnsureArtifactsTable() error {
	var schema string
	if db.dialect == DialectPostgres {
		schema = schemaArtifactsPostgres
	} else {
		schema = schemaArtifactsSQLite
	}

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("creating artifacts table: %w", err)
	}

	return nil
}

func (db *DB) SchemaVersion() (int, error) {
	var version int
	err := db.Get(&version, "SELECT version FROM schema_info LIMIT 1")
	if err != nil {
		return 0, err
	}
	return version, nil
}

// HasTable checks if a table exists in the database.
func (db *DB) HasTable(name string) (bool, error) {
	var exists bool
	var query string

	if db.dialect == DialectPostgres {
		query = "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = $1)"
	} else {
		query = "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)"
	}

	err := db.Get(&exists, query, name)
	return exists, err
}
