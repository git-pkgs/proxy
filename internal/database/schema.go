package database

import "fmt"

var schemaSQLite = `
CREATE TABLE IF NOT EXISTS schema_info (
	version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS packages (
	id INTEGER PRIMARY KEY,
	purl TEXT NOT NULL,
	ecosystem TEXT NOT NULL,
	name TEXT NOT NULL,
	namespace TEXT,
	latest_version TEXT,
	license TEXT,
	description TEXT,
	homepage TEXT,
	repository_url TEXT,
	upstream_url TEXT NOT NULL,
	metadata_fetched_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_packages_purl ON packages(purl);
CREATE INDEX IF NOT EXISTS idx_packages_ecosystem_name ON packages(ecosystem, name);

CREATE TABLE IF NOT EXISTS versions (
	id INTEGER PRIMARY KEY,
	purl TEXT NOT NULL,
	package_id INTEGER NOT NULL REFERENCES packages(id),
	version TEXT NOT NULL,
	license TEXT,
	integrity TEXT,
	published_at DATETIME,
	yanked INTEGER DEFAULT 0,
	metadata_fetched_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_versions_purl ON versions(purl);
CREATE INDEX IF NOT EXISTS idx_versions_package_id ON versions(package_id);
CREATE INDEX IF NOT EXISTS idx_versions_package_version ON versions(package_id, version);

CREATE TABLE IF NOT EXISTS artifacts (
	id INTEGER PRIMARY KEY,
	version_id INTEGER NOT NULL REFERENCES versions(id),
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
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_version_filename ON artifacts(version_id, filename);
CREATE INDEX IF NOT EXISTS idx_artifacts_storage_path ON artifacts(storage_path);
CREATE INDEX IF NOT EXISTS idx_artifacts_last_accessed ON artifacts(last_accessed_at);
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
	namespace TEXT,
	latest_version TEXT,
	license TEXT,
	description TEXT,
	homepage TEXT,
	repository_url TEXT,
	upstream_url TEXT NOT NULL,
	metadata_fetched_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_packages_purl ON packages(purl);
CREATE INDEX IF NOT EXISTS idx_packages_ecosystem_name ON packages(ecosystem, name);

CREATE TABLE IF NOT EXISTS versions (
	id SERIAL PRIMARY KEY,
	purl TEXT NOT NULL,
	package_id INTEGER NOT NULL REFERENCES packages(id),
	version TEXT NOT NULL,
	license TEXT,
	integrity TEXT,
	published_at TIMESTAMP,
	yanked BOOLEAN DEFAULT FALSE,
	metadata_fetched_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_versions_purl ON versions(purl);
CREATE INDEX IF NOT EXISTS idx_versions_package_id ON versions(package_id);
CREATE INDEX IF NOT EXISTS idx_versions_package_version ON versions(package_id, version);

CREATE TABLE IF NOT EXISTS artifacts (
	id SERIAL PRIMARY KEY,
	version_id INTEGER NOT NULL REFERENCES versions(id),
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
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_version_filename ON artifacts(version_id, filename);
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

func (db *DB) SchemaVersion() (int, error) {
	var version int
	err := db.Get(&version, "SELECT version FROM schema_info LIMIT 1")
	if err != nil {
		return 0, err
	}
	return version, nil
}
