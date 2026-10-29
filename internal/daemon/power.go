package daemon

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/power"
)

// refreshSleep also runs after lifecycle requests, so stopping the last
// sandbox releases the assertion without waiting for the next poll.
func (s *server) refreshSleep(ctx context.Context) {
	if s.preventSleep == nil || s.info.AllowSleep || s.d == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.d.List(ctx)
	if err != nil {
		slog.Warn("cannot check running sandboxes for sleep prevention", "err", err)
		return // keep an existing assertion when the backend cannot answer
	}
	running := false
	for _, sb := range list {
		running = running || sb.State == backend.StateRunning
	}
	if running && s.releaseSleep == nil {
		s.releaseSleep, err = s.preventSleep()
	} else if !running && s.releaseSleep != nil {
		err = s.releaseSleep()
		if err == nil {
			s.releaseSleep = nil
		}
	}
	if err != nil {
		slog.Warn("cannot update idle-sleep assertion", "err", err)
	}
}

// watchSleep detects VMs that exit without a lifecycle request.
func (s *server) watchSleep(ctx context.Context) func() {
	if runtime.GOOS != "darwin" || s.info.AllowSleep || s.d == nil {
		return func() {}
	}
	s.preventSleep = power.PreventSleep
	s.refreshSleep(ctx)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshSleep(ctx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
