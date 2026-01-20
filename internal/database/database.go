package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const SchemaVersion = 1

type DB struct {
	*sql.DB
	path string
}

func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func Create(path string) (*DB, error) {
	if Exists(path) {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing existing database: %w", err)
		}
	}

	db, err := Open(path)
	if err != nil {
		return nil, err
	}

	if err := db.CreateSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}

	return db, nil
}

func Open(path string) (*DB, error) {
	// Ensure parent directory exists
	if dir := filepath.Dir(path); dir != "." && dir != "/" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating database directory: %w", err)
		}
	}

	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	db := &DB{DB: sqlDB, path: path}
	if err := db.OptimizeForReads(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("optimizing database: %w", err)
	}

	return db, nil
}

func OpenOrCreate(path string) (*DB, error) {
	if Exists(path) {
		return Open(path)
	}
	return Create(path)
}

func (db *DB) OptimizeForBulkWrites() error {
	_, err := db.Exec(`
		PRAGMA synchronous = OFF;
		PRAGMA journal_mode = WAL;
		PRAGMA cache_size = -64000;
	`)
	return err
}

func (db *DB) OptimizeForReads() error {
	_, err := db.Exec(`
		PRAGMA synchronous = NORMAL;
		PRAGMA journal_mode = WAL;
	`)
	return err
}

func (db *DB) Path() string {
	return db.path
}
