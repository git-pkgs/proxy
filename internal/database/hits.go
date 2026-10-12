package database

import (
	"cmp"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Recording each cache hit as its own UPDATE makes every cached download a
// write transaction, and SQLite runs on a single connection, so concurrent
// downloads queue behind those writes. BatchHits counts hits in memory and
// writes them in one transaction per interval instead.

type hitKey struct{ versionPURL, filename string }

type hitEntry struct {
	count int64
	last  time.Time
}

type hitBatch struct {
	mu       sync.Mutex
	pending  map[hitKey]hitEntry
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func (b *hitBatch) add(k hitKey, e hitEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.pending[k]
	cur.count += e.count
	if e.last.After(cur.last) {
		cur.last = e.last
	}
	b.pending[k] = cur
}

func (b *hitBatch) take() map[hitKey]hitEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	pending := b.pending
	b.pending = map[hitKey]hitEntry{}
	return pending
}

// BatchHits makes RecordArtifactHit buffer hits and write them every
// interval. An interval of zero or less leaves every hit written immediately.
// Close writes whatever is still pending. Failed writes are logged to logger.
func (db *DB) BatchHits(interval time.Duration, logger *slog.Logger) {
	if interval <= 0 || db.hits != nil {
		return
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	b := &hitBatch{pending: map[hitKey]hitEntry{}, stop: make(chan struct{}), done: make(chan struct{})}
	db.hits = b
	go func() {
		defer close(b.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := db.flushHits(); err != nil {
					logger.Warn("failed to write cache hits, retrying next flush", "error", err)
				}
			case <-b.stop:
				if err := db.flushHits(); err != nil {
					logger.Error("failed to write cache hits on close, dropping them", "error", err)
				}
				return
			}
		}
	}()
}

// flushHits writes the pending hits in one transaction. If the transaction
// fails they are put back for the next flush, so a failed write delays
// counts rather than losing them.
func (db *DB) flushHits() error {
	pending := db.hits.take()
	if len(pending) == 0 {
		return nil
	}

	err := db.writeHits(pending)
	if err != nil {
		for k, e := range pending {
			db.hits.add(k, e)
		}
	}
	return err
}

func (db *DB) writeHits(pending map[hitKey]hitEntry) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Timestamps only move forward: with proxies sharing a database, an older
	// batch can flush after a newer hit is already written.
	stmt, err := tx.Preparex(db.Rebind(`
		UPDATE artifacts
		SET hit_count = hit_count + ?,
		    last_accessed_at = CASE WHEN last_accessed_at IS NULL OR last_accessed_at < ? THEN ? ELSE last_accessed_at END,
		    updated_at = CASE WHEN updated_at IS NULL OR updated_at < ? THEN ? ELSE updated_at END
		WHERE version_purl = ? AND filename = ?
	`))
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	// A fixed order keeps proxies sharing a Postgres database from locking
	// the same rows in opposite orders and deadlocking.
	keys := make([]hitKey, 0, len(pending))
	for k := range pending {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b hitKey) int {
		return cmp.Or(cmp.Compare(a.versionPURL, b.versionPURL), cmp.Compare(a.filename, b.filename))
	})
	for _, k := range keys {
		e := pending[k]
		last := e.last.UTC()
		if _, err := stmt.Exec(e.count, last, last, e.last, e.last, k.versionPURL, k.filename); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close writes any batched hits, then closes the database.
func (db *DB) Close() error {
	if b := db.hits; b != nil {
		b.stopOnce.Do(func() { close(b.stop) })
		<-b.done
	}
	return db.DB.Close()
}
