package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"syscall"
	"time"
)

// Shutdown is how the agent serves POST /v1/shutdown. Only the agent in a
// VM serves it: GuestShutdown returns the real one.
type Shutdown struct {
	// Pids lists live processes. It may leave out zombies and kernel
	// threads, which do not exit on a signal.
	Pids func() ([]int, error)
	Kill func(pid int, sig syscall.Signal) error
	// Remount syncs and remounts the root filesystem read-only.
	Remount func() error
	// Reboot ends the VM. It returns only if it fails.
	Reboot func() error
}

// DefaultShutdownGrace is how long guest processes get to exit after
// SIGTERM when the request does not say.
const DefaultShutdownGrace = 10 * time.Second

// killWait bounds the wait for processes to die after SIGKILL.
const killWait = 2 * time.Second

func (s *Shutdown) serve(w http.ResponseWriter, r *http.Request) {
	var req ShutdownRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad shutdown request: %w", err))
		return
	}
	grace := DefaultShutdownGrace
	if req.GraceMS > 0 {
		grace = time.Duration(req.GraceMS) * time.Millisecond
	}
	if err := s.stop(syscall.SIGTERM, grace); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.stop(syscall.SIGKILL, killWait); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.Remount(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("remount / read-only: %w", err))
		return
	}
	w.WriteHeader(http.StatusOK)
	http.NewResponseController(w).Flush()
	if err := s.Reboot(); err != nil {
		slog.Error("reboot failed after shutdown", "err", err)
	}
}

// stop sends sig to every process but the agent and PID 1, then waits up
// to wait for them to exit. Processes still running are left to the next
// stage; only failing to list them is an error.
func (s *Shutdown) stop(sig syscall.Signal, wait time.Duration) error {
	pids, err := s.others()
	if err != nil {
		return err
	}
	for _, pid := range pids {
		if err := s.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			slog.Warn("could not signal process", "pid", pid, "signal", sig, "err", err)
		}
	}
	for deadline := time.Now().Add(wait); len(pids) > 0 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		if pids, err = s.others(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Shutdown) others() ([]int, error) {
	pids, err := s.Pids()
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	self, out := os.Getpid(), pids[:0]
	for _, pid := range pids {
		if pid != 1 && pid != self {
			out = append(out, pid)
		}
	}
	return out, nil
}
