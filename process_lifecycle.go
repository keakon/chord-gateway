package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/keakon/golog/log"
)

const defaultAutoRestartDelay = 5 * time.Second

// TerminateGroup attempts to gracefully stop the chord process and, if needed,
// force-kill its whole process group.
func (p *ChordProcess) TerminateGroup(grace time.Duration) {
	if p == nil {
		return
	}
	p.exitOnce.Do(func() {
		p.markTransferInterrupted()
		// Mark as stopped by gateway to avoid crash restarts.
		p.mu.Lock()
		p.stoppedByGateway = true
		p.abandonPendingInputs()
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
			p.waitAndRecordExit(cmd)
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
	if p.readySignal != nil {
		p.readyOnce.Do(func() { close(p.readySignal) })
	}
	p.state.PendingLocalShells = 0
	p.state.LocalShellStartedAt = time.Time{}
	p.state.setAgentBusy(false)
	p.state.resetTurnWindow()
	// Disconnection leaves Core outcomes unknown. Retain questions for explicit
	// retry after resynchronization rather than inventing answer outcomes.
	p.abandonPendingInputs()
	p.state.RunningJobs = 0
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
	// Waiting here guarantees the process exit is recorded before checking it.
	exitCode := p.waitAndRecordExit(cmd)
	if p.recordTransferExit(exitCode) {
		autoRestart = false
		crashed = exitCode != sessionTransferExitCode
	}

	log.Infof("[%v] chord process exited pid=%v exit_code=%v crashed=%v", processLogContext(key, state), pid, exitCode, crashed)
	if crashed && strings.TrimSpace(stderr) != "" {
		// Keep stderr short; full details are usually in chord.log.
		log.Warnf("[%v] chord process stderr pid=%v stderr=%v", processLogContext(key, state), pid, truncateStderrTail(stderr, 2000))
	}

	if p.onEvent != nil {
		p.onEvent(key, "exit", state)
	}

	if autoRestart {
		go func() {
			log.Infof("[%v] auto-restarting crashed chord process in %v", processLogContext(key, state), defaultAutoRestartDelay)
			if !p.waitAutoRestart(defaultAutoRestartDelay) {
				log.Infof("[%v] auto-restart cancelled: gateway is shutting down", processLogContext(key, state))
				return
			}
			if p.gate != nil {
				p.gate.OpMu.Lock()
				defer p.gate.OpMu.Unlock()
				current, _, phase := p.gate.Snapshot()
				if p.lease == nil || current != p.lease || phase != "active" || p.lease.Context().Err() != nil {
					return
				}
			}
			// Recheck the exact binding after the delay; a replacement owns its lifecycle.
			if p.mgr != nil && p.mgr.GetProcessForKey(key) != p {
				return
			}
			// Use the manager to respawn; it handles the procs map.
			if p.mgr != nil {
				if _, err := p.mgr.getOrSpawnForKeyChatLocked(key); err != nil {
					if errors.Is(err, ErrManagerShuttingDown) {
						log.Debugf("[%v] auto-restart skipped: %v", processLogContext(key, state), err)
					} else {
						log.Errorf("[%v] auto-restart failed error=%v", processLogContext(key, state), err)
					}
				} else {
					log.Infof("[%v] auto-restart succeeded", processLogContext(key, state))
				}
			}
		}()
	}
}

// waitAutoRestart blocks until delay elapses and reports whether the
// restart should proceed. Manager shutdown cancels the wait so a crashed
// process is not respawned and the goroutine does not outlive gateway shutdown
// by the whole delay.
func (p *ChordProcess) waitAutoRestart(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-p.mgr.shutdownSignal():
		return false
	}
}

func (p *ChordProcess) transitionToIdle(updatedAt string, expirePending bool) {
	p.state.setAgentBusy(false)
	p.state.Phase = ""
	p.state.PhaseDetail = ""
	if expirePending {
		p.state.resetTurnWindow()
		p.state.ExpiredConfirm = p.state.PendingConfirm
		p.state.ExpiredHandoff = p.state.PendingHandoff
	} else {
		p.state.ExpiredConfirm = nil
		p.state.ExpiredHandoff = nil
	}
	p.state.PendingConfirm = nil
	p.state.PendingHandoff = nil
	p.state.LastError = ""
	if strings.TrimSpace(updatedAt) == "" {
		p.state.UpdatedAt = time.Now().Format(time.RFC3339)
	} else {
		p.state.UpdatedAt = updatedAt
	}
}

// IdleCheckLoop periodically checks all processes and closes idle ones. It
// exits when the manager begins shutting down.
func (m *ChordManager) IdleCheckLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	shutdown := m.shutdownSignal()

	for {
		select {
		case <-shutdown:
			return
		case <-ticker.C:
		}
		timeout := m.Config().IdleTimeoutDuration()
		idle := m.collectIdleProcesses(timeout)

		// Close stdin for idle processes to let them exit gracefully.
		// Then, if they don't exit quickly, terminate the whole process group.
		for _, p := range idle {
			p.mu.Lock()
			if p.state.PendingConfirm != nil || p.state.pendingQuestionCount() > 0 || p.state.PendingHandoff != nil {
				p.transitionToIdle(time.Now().Format(time.RFC3339), true)
				state := p.state
				p.mu.Unlock()
				if p.onEvent != nil {
					p.onEvent(p.key, "idle_timeout", state)
				}
			} else {
				p.mu.Unlock()
			}
			log.Infof("[%v] idle timeout, stopping chord process", processLogContext(p.key, p.State()))
			p.TerminateGroup(2 * time.Second)
		}
	}
}

func (m *ChordManager) collectIdleProcesses(timeout time.Duration) []*ChordProcess {
	m.mu.RLock()
	procs := make([]*ChordProcess, 0, len(m.procs))
	for _, p := range m.procs {
		procs = append(procs, p)
	}
	m.mu.RUnlock()

	// Bound the entire refresh sweep, including unresponsive children. Status
	// replies retain the existing idle-clock semantics; confirmed empty sessions
	// become eligible after that quiet period without manual /status requests.
	refreshCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	candidates := make([]*ChordProcess, 0)
	for _, p := range procs {
		p.mu.RLock()
		expired := !p.inputBlockedLocked() && time.Since(p.lastActivity) > timeout
		p.mu.RUnlock()
		if expired {
			if err := p.refreshPendingInputStatus(refreshCtx); err != nil {
				continue
			}
		}
		p.mu.RLock()
		if !p.inputBlockedLocked() && !p.hasGuardedInputsLocked() && p.state.RunningJobs == 0 && p.state.PendingLocalShells == 0 && time.Since(p.lastActivity) > timeout {
			candidates = append(candidates, p)
		}
		p.mu.RUnlock()
	}
	// Remove only the exact processes inspected above. A process may have
	// been replaced under the same key while the snapshot was checked.
	idle := make([]*ChordProcess, 0, len(candidates))
	for _, p := range candidates {
		p.mu.Lock()
		m.mu.Lock()
		if !p.inputBlockedLocked() && !p.hasGuardedInputsLocked() && m.procs[p.key] == p && p.state.RunningJobs == 0 && p.state.PendingLocalShells == 0 && time.Since(p.lastActivity) > timeout {
			p.cancelDeliveryLocked()
			delete(m.procs, p.key)
			idle = append(idle, p)
		}
		m.mu.Unlock()
		p.mu.Unlock()
	}
	return idle
}
