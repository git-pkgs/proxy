package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	maxFirstScheduleDelay = 10 * time.Minute
	// scheduleJitter spreads later runs by ±interval/scheduleJitter.
	scheduleJitter = 10
)

// Schedule mirrors a set of image PURLs every Interval.
type Schedule struct {
	PURLs     []string
	Platforms []string
	Interval  time.Duration
}

// Scheduler runs schedules in the background until its context ends.
type Scheduler struct {
	mirror    *Mirror
	schedules []Schedule
	logger    *slog.Logger
	wg        sync.WaitGroup
	// Claim, when set, reports whether this instance should run a schedule
	// that no instance sharing the database started within minGap. Each
	// instance keeps its own timer, so without it every instance would run
	// every schedule.
	Claim func(key string, minGap time.Duration) (bool, error)
	// Release, when set, drops the claim of a run that was interrupted, so
	// the next attempt runs it rather than waiting an interval.
	Release func(key string) error
	// randN returns a value in [0, n); tests replace it.
	randN func(n int64) int64
}

// NewScheduler creates a scheduler that runs the schedules through m.
func NewScheduler(m *Mirror, schedules []Schedule, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		mirror:    m,
		schedules: schedules,
		logger:    logger,
		randN:     rand.Int64N, //nolint:gosec // jitter, not security
	}
}

// Start starts one goroutine per schedule.
func (s *Scheduler) Start(ctx context.Context) {
	for i, schedule := range s.schedules {
		if schedule.Interval <= 0 {
			s.logger.Error("mirror schedule has no interval, skipped", "schedule", i)
			continue
		}
		s.wg.Add(1)
		go s.loop(ctx, i, schedule)
	}
}

// Wait blocks until every schedule has stopped after its context ended.
func (s *Scheduler) Wait() {
	s.wg.Wait()
}

func (s *Scheduler) loop(ctx context.Context, index int, schedule Schedule) {
	defer s.wg.Done()
	timer := time.NewTimer(scheduleDelay(schedule.Interval, true, s.randN))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.run(ctx, index, schedule)
		// The next run is timed from the end of this one, so runs never overlap.
		timer.Reset(scheduleDelay(schedule.Interval, false, s.randN))
	}
}

func (s *Scheduler) run(ctx context.Context, index int, schedule Schedule) {
	key := scheduleKey(schedule)
	if s.Claim != nil {
		// Later runs come at least interval - 2*spread apart, so a run
		// started more recently than that was another instance's.
		minGap := schedule.Interval - 2*(schedule.Interval/scheduleJitter) //nolint:mnd // two spreads of margin
		claimed, err := s.Claim(key, minGap)
		if err != nil {
			s.logger.Warn("mirror schedule: failed to claim run, skipping it", "schedule", index, "error", err)
			return
		}
		if !claimed {
			s.logger.Debug("mirror schedule: another instance ran it, skipping", "schedule", index)
			return
		}
	}
	started := time.Now()
	progress, err := s.mirror.Run(ctx, &PURLSource{PURLs: schedule.PURLs, Versions: s.mirror.Versions, Platforms: schedule.Platforms})
	if ctx.Err() != nil {
		if s.Claim != nil && s.Release != nil {
			if err := s.Release(key); err != nil {
				s.logger.Warn("mirror schedule: failed to release interrupted run", "schedule", index, "error", err)
			}
		}
		return
	}
	if err != nil {
		s.logger.Error("mirror schedule failed", "schedule", index, "error", err)
		return
	}
	level := slog.LevelInfo
	if progress.Failed > 0 {
		level = slog.LevelWarn
	}
	s.logger.Log(ctx, level, "mirror schedule run",
		"schedule", index, "mirrored", progress.Completed, "unchanged", progress.Skipped,
		"failed", progress.Failed, "bytes", progress.Bytes, "duration", time.Since(started).Round(time.Millisecond))
}

// scheduleDelay spreads runs so instances and schedules started together
// don't hit the registry at once. The first run comes within ten minutes, so
// a restart doesn't push a refresh back by a whole interval.
func scheduleDelay(interval time.Duration, first bool, randN func(int64) int64) time.Duration {
	if first {
		return time.Duration(randN(int64(min(interval, maxFirstScheduleDelay))))
	}
	spread := interval / scheduleJitter
	return interval - spread + time.Duration(randN(int64(2*spread)+1)) //nolint:mnd // spread on both sides
}

// scheduleKey names a schedule by what it mirrors and how often, so
// instances with the same configuration agree on it and a changed schedule
// does not inherit the old one's last run.
func scheduleKey(schedule Schedule) string {
	h := sha256.New()
	for _, purl := range schedule.PURLs {
		_, _ = fmt.Fprintf(h, "purl %s\n", purl)
	}
	for _, platform := range schedule.Platforms {
		_, _ = fmt.Fprintf(h, "platform %s\n", platform)
	}
	_, _ = fmt.Fprintf(h, "interval %s\n", schedule.Interval)
	return "mirror-schedule:" + hex.EncodeToString(h.Sum(nil))[:32]
}
