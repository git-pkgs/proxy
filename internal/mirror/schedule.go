package mirror

import (
	"context"
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
	started := time.Now()
	progress, err := s.mirror.Run(ctx, &PURLSource{PURLs: schedule.PURLs, Platforms: schedule.Platforms})
	if ctx.Err() != nil {
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
