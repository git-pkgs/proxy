package database

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// TestOpenPostgresKeepsConnectionsIdle checks the connection-count limits
// OpenPostgres sets: the open cap admits a burst of postgresMaxIdleConns
// connections, and releasing them again leaves all of them idle in the pool.
// database/sql's default keeps only two, so the next burst would open a new
// Postgres session for almost every request. The idle-time and lifetime
// settings are not exercised here.
func TestOpenPostgresKeepsConnectionsIdle(t *testing.T) {
	url := os.Getenv("PROXY_DATABASE_URL")
	if url == "" {
		t.Skip("PROXY_DATABASE_URL not set, skipping postgres pool test")
	}

	db, err := OpenPostgres(url)
	if err != nil {
		t.Fatalf("OpenPostgres failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	const burst = postgresMaxIdleConns
	// database/sql keeps two idle connections by default; a burst that small
	// could not tell the tuned pool from the default one.
	if burst <= 2 {
		t.Fatalf("postgresMaxIdleConns = %d, want more than database/sql's default of 2", burst)
	}
	if got := db.Stats().MaxOpenConnections; got <= 0 || got < burst {
		t.Fatalf("MaxOpenConnections = %d, want a cap of at least %d", got, burst)
	}

	conns := make([]*sql.Conn, 0, burst)
	for range burst {
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatalf("taking connection %d: %v", len(conns)+1, err)
		}
		t.Cleanup(func() { _ = conn.Close() }) // release the session if an assertion below fails
		conns = append(conns, conn)
	}
	if got := db.Stats().InUse; got != burst {
		t.Fatalf("InUse = %d while holding %d connections", got, burst)
	}

	for _, conn := range conns {
		_ = conn.Close()
	}
	if got := db.Stats().Idle; got != burst {
		t.Errorf("Idle = %d after releasing %d connections, want all of them kept", got, burst)
	}
}
