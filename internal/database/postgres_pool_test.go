package database

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// TestOpenPostgresKeepsConnectionsIdle checks the pool limits OpenPostgres
// sets. Taking a burst of connections and releasing them again must leave all
// of them idle in the pool; database/sql's default keeps only two, so the
// next burst would open a new Postgres session for almost every request.
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

	if got := db.Stats().MaxOpenConnections; got != postgresMaxOpenConns {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, postgresMaxOpenConns)
	}

	const burst = 16
	conns := make([]*sql.Conn, 0, burst)
	for range burst {
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatalf("taking connection %d: %v", len(conns)+1, err)
		}
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
