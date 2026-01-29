package database

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

const SchemaVersion = 1

type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

type DB struct {
	*sqlx.DB
	dialect Dialect
	path    string
}

func (db *DB) Dialect() Dialect {
	return db.dialect
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
	if dir := filepath.Dir(path); dir != "." && dir != "/" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating database directory: %w", err)
		}
	}

	sqlDB, err := sqlx.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	db := &DB{DB: sqlDB, dialect: DialectSQLite, path: path}
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

func OpenPostgres(url string) (*DB, error) {
	sqlDB, err := sqlx.Open("postgres", url)
	if err != nil {
		return nil, fmt.Errorf("opening postgres database: %w", err)
	}

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}

	return &DB{DB: sqlDB, dialect: DialectPostgres}, nil
}

func OpenPostgresOrCreate(url string) (*DB, error) {
	db, err := OpenPostgres(url)
	if err != nil {
		return nil, err
	}

	var exists bool
	err = db.Get(&exists, "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'schema_info')")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("checking schema: %w", err)
	}

	if !exists {
		if err := db.CreateSchema(); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("creating schema: %w", err)
		}
	}

	return db, nil
}

func (db *DB) OptimizeForBulkWrites() error {
	if db.dialect == DialectPostgres {
		return nil
	}
	_, err := db.Exec(`
		PRAGMA synchronous = OFF;
		PRAGMA journal_mode = WAL;
		PRAGMA cache_size = -64000;
	`)
	return err
}

func (db *DB) OptimizeForReads() error {
	if db.dialect == DialectPostgres {
		return nil
	}
	_, err := db.Exec(`
		PRAGMA synchronous = NORMAL;
		PRAGMA journal_mode = WAL;
	`)
	return err
}

func (db *DB) Path() string {
	return db.path
}

func (db *DB) Rebind(query string) string {
	return db.DB.Rebind(query)
}
