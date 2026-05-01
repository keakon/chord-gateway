package main

import (
	"log/slog"
	"strings"
	"time"
)

// TerminateGroup attempts to gracefully stop the chord process and, if needed,
// force-kill its whole process group.
func (p *ChordProcess) TerminateGroup(grace time.Duration) {
	if p == nil {
		return
	}
	p.exitOnce.Do(func() {
		// Mark as stopped by gateway to avoid crash restarts.
		p.mu.Lock()
		p.stoppedByGateway = true
		// Close stdin first to let chord exit gracefully.
		if p.stdin != nil {
			_ = p.stdin.Close()
			p.stdin = nil
		}
		cmd := p.cmd
		pgid := p.pgid
		p.mu.Unlock()

		// If no process, nothing to do.
		if cmd == nil || cmd.Process == nil {
			return
		}

		// Best-effort: send SIGTERM to process group.
		if pgid > 0 {
			_ = terminateProcessGroup(pgid)
		} else {
			_ = terminateProcess(cmd.Process)
		}

		// Wait for graceful exit up to grace.
		if grace <= 0 {
			grace = 2 * time.Second
		}
		done := make(chan struct{})
		go func() {
			p.waitOnce.Do(func() {
				_ = cmd.Wait()
			})
			close(done)
		}()

		select {
		case <-done:
			return
		case <-time.After(grace):
		}

		// Force kill.
		if pgid > 0 {
			_ = killProcessGroup(pgid)
		} else {
			_ = cmd.Process.Kill()
		}
	})
}

// handleExit cleans up when the chord process exits.
func (p *ChordProcess) handleExit() {
	p.mu.Lock()
	p.state.Busy = false
	p.state.UpdatedAt = time.Now().Format(time.RFC3339)
	if p.stdin != nil {
		p.stdin.Close()
		p.stdin = nil
	}
	if p.cancel != nil {
		p.cancel()
	}
	cmd := p.cmd
	pid := 0
	if cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	crashed := !p.stoppedByGateway
	autoRestart := p.autoRestart && crashed
	key := p.key
	state := p.state
	stderr := ""
	if p.stderr != nil {
		stderr = p.stderr.String()
	}
	// If this is an expected init failure (e.g. session lock), don't spam auto-restart.
	// Cobra prints errors to stderr; chord exits with code 1 on init errors.
	if strings.Contains(stderr, "acquire session lock") {
		autoRestart = false
	}
	p.mu.Unlock()

	// Serialize Cmd.Wait calls. TerminateGroup may call Wait concurrently.
	// Waiting here guarantees ProcessState is fully populated before reading it.
	if cmd != nil {
		p.waitOnce.Do(func() {
			_ = cmd.Wait()
		})
	}

	exitCode := 0
	if cmd != nil && cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	slog.Info("chord process exited", "key", key, "pid", pid, "exit_code", exitCode, "crashed", crashed)
	if crashed && strings.TrimSpace(stderr) != "" {
		// Keep stderr short; full details are usually in chord.log.
		slog.Warn("chord process stderr", "key", key, "pid", pid, "stderr", truncateStderr(stderr, 2000))
	}

	if p.onEvent != nil {
		p.onEvent(key, "exit", state)
	}

	if autoRestart {
		go func() {
			slog.Info("auto-restarting crashed chord process in 5s", "key", key)
			time.Sleep(5 * time.Second)
			// Use the manager to respawn; it handles the procs map.
			if p.mgr != nil {
				if _, err := p.mgr.GetOrSpawnForKey(key); err != nil {
					slog.Error("auto-restart failed", "key", key, "error", err)
				} else {
					slog.Info("auto-restart succeeded", "key", key)
				}
			}
		}()
	}
}

// IdleCheckLoop periodically checks all processes and closes idle ones.
func (m *ChordManager) IdleCheckLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		m.mu.Lock()
		idle := make([]*ChordProcess, 0)
		timeout := m.cfg.Load().IdleTimeoutDuration()
		for _, p := range m.procs {
			p.mu.Lock()
			if time.Since(p.lastActivity) > timeout {
				idle = append(idle, p)
			}
			p.mu.Unlock()
		}
		// Remove idle procs from map.
		for _, p := range idle {
			delete(m.procs, p.key)
		}
		m.mu.Unlock()

		// Close stdin for idle processes to let them exit gracefully.
		// Then, if they don't exit quickly, terminate the whole process group.
		for _, p := range idle {
			p.mu.Lock()
			if p.state.PendingConfirm != nil || p.state.PendingQuestion != nil {
				p.state.ExpiredConfirm = p.state.PendingConfirm
				p.state.ExpiredQuestion = p.state.PendingQuestion
				p.state.PendingConfirm = nil
				p.state.PendingQuestion = nil
				p.state.Busy = false
				p.state.LastError = ""
				p.state.UpdatedAt = time.Now().Format(time.RFC3339)
				state := p.state
				p.mu.Unlock()
				if p.onEvent != nil {
					p.onEvent(p.key, "idle_timeout", state)
				}
			} else {
				p.mu.Unlock()
			}
			slog.Info("idle timeout, stopping chord process", "workspace", p.workspaceID)
			p.TerminateGroup(2 * time.Second)
		}
	}
}
