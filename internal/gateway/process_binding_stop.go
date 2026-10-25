package gateway

import (
	"context"
	"time"
)

// StopAndWait confirms both the recorded Wait and the supported process-group
// boundary. A deadline failure never authorizes a replacement binding.
func (p *ChordProcess) StopAndWait(ctx context.Context, grace time.Duration) error {
	p.mu.Lock()
	p.stoppedByGateway = true
	p.autoRestart = false
	cmd, pgid, done := p.cmd, p.pgid, p.waitDone
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if done == nil {
		return errBindBlocked
	}
	p.exitOnce.Do(func() {
		p.mu.Lock()
		if p.stdin != nil {
			_ = p.stdin.Close()
			p.stdin = nil
		}
		p.mu.Unlock()
		if pgid > 0 {
			_ = terminateProcessGroup(pgid)
		} else {
			_ = terminateProcess(cmd.Process)
		}
	})
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		if !bindingGroupAlive(pgid) {
			return nil
		}
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	if pgid > 0 {
		_ = killProcessGroup(pgid)
	} else {
		_ = cmd.Process.Kill()
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			select {
			case <-done:
				if !bindingGroupAlive(pgid) {
					return nil
				}
			default:
			}
		}
	}
}
