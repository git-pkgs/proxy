package server

import (
	"context"
	"time"

	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
)

// mirrorScheduleShutdownWait bounds how long shutdown waits for a scheduled
// run to stop before storage and database are closed under it.
const mirrorScheduleShutdownWait = 30 * time.Second

func (s *Server) startMirrorSchedules(ctx context.Context, proxy *handler.Proxy) {
	if len(s.cfg.Mirror.Schedules) == 0 {
		return
	}
	// One worker, so a schedule never fans out against the registry.
	m := mirror.New(proxy, s.db, s.storage, s.logger, 1)
	m.Images = s.newContainerHandler(proxy)
	schedules := make([]mirror.Schedule, len(s.cfg.Mirror.Schedules))
	for i, schedule := range s.cfg.Mirror.Schedules {
		schedules[i] = mirror.Schedule{PURLs: schedule.Images, Platforms: schedule.Platforms, Interval: schedule.IntervalDuration()}
	}
	s.scheduler = mirror.NewScheduler(m, schedules, s.logger)
	s.scheduler.Start(ctx)
	s.logger.Info("mirror schedules enabled", "schedules", len(schedules))
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
