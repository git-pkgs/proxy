package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/registries/changes"
	"github.com/git-pkgs/registries/client"
)

// changeFeedInterval is how often an upstream change feed is read.
const changeFeedInterval = time.Minute

// changeFeedMaxBackoff caps the wait between reads of a failing feed.
const changeFeedMaxBackoff = 15 * time.Minute

// startChangeFeeds follows the change feeds the configuration turns on. Each
// instance sharing a database follows them itself: the reads are small, and
// expiring a row twice does no harm.
func (s *Server) startChangeFeeds(ctx context.Context, httpClient *http.Client, metadataTTL time.Duration) {
	c := changes.WithClient(client.NewClient(client.WithHTTPClient(httpClient)))
	var feeds []changes.Feed
	if s.cfg.Upstream.NPMChangeFeed {
		feeds = append(feeds, changes.NewNPM(c))
	}
	if s.cfg.Upstream.ComposerChangeFeed {
		feeds = append(feeds, changes.NewPackagist(c, changes.WithURL(changes.PackagistFeedURL(s.cfg.Upstream.Composer))))
	}
	if len(feeds) == 0 {
		return
	}
	if !s.cfg.CacheMetadata || metadataTTL <= 0 {
		// Every request already revalidates, so there is nothing to expire.
		s.logger.Info("change feeds not followed: metadata is not served from cache without revalidating")
		return
	}
	for _, feed := range feeds {
		s.background.Go(func() { followChangeFeed(ctx, s.db, s.logger, feed, changeFeedInterval) })
	}
}

// followChangeFeed reads feed every interval until ctx ends and expires the
// cached metadata it lists. After a failed read it waits twice as long as
// before, up to changeFeedMaxBackoff. The position in the feed is kept in
// memory: after a restart the feed is followed from its current position.
func followChangeFeed(ctx context.Context, db *database.DB, logger *slog.Logger, feed changes.Feed, interval time.Duration) {
	wait := interval
	var cursor changes.Cursor
	for {
		next, err := readChangeFeed(ctx, db, logger, feed, cursor)
		cursor = next
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait = min(wait+wait, changeFeedMaxBackoff)
			logger.Warn("change feed: read failed", "ecosystem", feed.Ecosystem(), "error", err, "retry_in", wait)
		} else {
			wait = interval
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// readChangeFeed reads feed once after cursor, or finds its current position
// when cursor is empty, and expires the cached metadata of what it lists, so
// the next request for it revalidates upstream. Only rows fetched before the
// read finished are expired: a row fetched after it was fetched after every
// change the read returned. It returns the cursor to read on from, which
// moves past the changes only once they are expired, so a failed expiry
// reads them again.
func readChangeFeed(ctx context.Context, db *database.DB, logger *slog.Logger, feed changes.Feed, cursor changes.Cursor) (changes.Cursor, error) {
	if cursor == "" {
		return feed.Start(ctx)
	}
	found, next, err := feed.Changes(ctx, cursor)
	if err != nil {
		return cursor, err
	}
	cutoff := time.Now()
	ecosystem := feed.Ecosystem()
	var expired []string
	switch {
	case found.All:
		logger.Info("change feed: upstream asked for a full resync, revalidating all cached metadata", "ecosystem", ecosystem)
		expired, err = db.ExpireAllMetadataCache(ecosystem, cutoff)
	case len(found.Names) > 0:
		expired, err = db.ExpireMetadataCache(ecosystem, found.Names, cutoff)
	}
	for _, name := range expired {
		logger.Debug("change feed: cached metadata changed upstream", "ecosystem", ecosystem, "key", name)
	}
	metrics.RecordMetadataExpiredByChangeFeed(ecosystem, len(expired))
	if err != nil {
		return cursor, err
	}
	return next, nil
}
