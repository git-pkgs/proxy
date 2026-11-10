package server

import (
	"context"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
)

// mirrorScheduleShutdownWait bounds how long shutdown waits for a scheduled
// run to stop before storage and database are closed under it.
const mirrorScheduleShutdownWait = 30 * time.Second

func (s *Server) startMirrorSchedules(ctx context.Context, proxy *handler.Proxy) {
	schedules := mirrorSchedules(s.cfg.Mirror)
	if len(schedules) == 0 {
		return
	}
	// One worker, so a schedule never fans out against the registry.
	m := s.newMirror(proxy, 1)
	s.scheduler = mirror.NewScheduler(m, schedules, s.logger)
	s.scheduler.Claim = s.db.ClaimJobRun
	s.scheduler.Release = s.db.ReleaseJobRun
	s.scheduler.Start(ctx)
	s.logger.Info("mirror schedules enabled", "schedules", len(schedules), "refresh", s.cfg.Mirror.Refresh != nil)
}

// mirrorSchedules returns the schedules of a validated mirror configuration,
// the refresh last.
func mirrorSchedules(cfg config.MirrorConfig) []mirror.Schedule {
	var schedules []mirror.Schedule
	for _, schedule := range cfg.Schedules {
		schedules = append(schedules, mirror.Schedule{PURLs: schedule.Images, Platforms: schedule.Platforms, Interval: schedule.IntervalDuration()})
	}
	if r := cfg.Refresh; r != nil {
		schedules = append(schedules, mirror.Schedule{
			Refresh: &mirror.Refresh{
				Top:         r.TopPackages(),
				Since:       r.SinceDuration(),
				MaxVersions: r.MaxVersionsPerRun(),
				MaxBytes:    r.MaxBytesPerRun(),
			},
			Interval: r.IntervalDuration(),
		})
	}
	return schedules
}

func (s *Server) waitForMirrorSchedules(ctx context.Context) {
	if s.scheduler == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, mirrorScheduleShutdownWait)
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.scheduler.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.logger.Warn("mirror schedule still running at shutdown")
	}
}
