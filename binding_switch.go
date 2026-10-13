package main

import (
	"context"
	"time"
)

// The caller owns g.opMu. Reserve retirement under the same lifecycle lock as
// outgoing takeover admission. No process or delivery waits may hold that lock:
// the stdout reader needs it to finish processing events and record the exit.
func (m *ChordManager) beginBindingSwitch(g *ChatGate, oldKey, newKey string) (*ChordProcess, *BindingLease, error) {
	m.lifecycleGate.RLock()
	defer m.lifecycleGate.RUnlock()
	release := m.acquireKeyLock(oldKey)
	defer release()
	if m.shuttingDown.Load() {
		return nil, nil, ErrManagerShuttingDown
	}
	_, _, phase := g.snapshot()
	if phase == "blocked" || phase == "switching" {
		return nil, nil, errBindBlocked
	}
	if m.transferBlocked(oldKey) || m.transferBlocked(newKey) {
		return nil, nil, errBindBusy
	}
	p := m.GetProcessForKey(oldKey)
	if p != nil {
		p.mu.Lock()
		if !p.canRebindLocked() {
			p.mu.Unlock()
			return nil, nil, errBindBusy
		}
		// This stop intent also prevents accepting a new transfer while old
		// outbound sends drain. It never creates a durable transfer guard.
		p.stoppedByGateway = true
		p.autoRestart = false
		p.mu.Unlock()
	}
	return p, g.switching(), nil
}

// Directory switches drain output and confirm process shutdown before
// publishing new routing. Failure leaves the gate closed until local recovery.
func (r *NotificationRouter) prepareBindingSwitch(ctx context.Context, g *ChatGate, oldKey, newKey string) error {
	// The paused projection covers every owner, while InputQueue covers main
	// only. Refresh it before retirement rather than inferring completion from
	// main's empty queue or the aggregate idle event.
	if p := r.mgr.GetProcessForKey(oldKey); p != nil {
		state := p.State()
		if !state.Busy && state.PausedInputs != nil && state.PausedInputs.PendingInputCount > 0 {
			wait, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := p.WaitStatus(wait)
			cancel()
			if err != nil {
				return errBindStatusUnknown
			}
		}
	}
	p, lease, err := r.mgr.beginBindingSwitch(g, oldKey, newKey)
	if err != nil {
		return err
	}
	if oldKey != "" {
		r.clearTransferCards(oldKey)
	}
	if lease != nil {
		wait, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = lease.Wait(wait)
		cancel()
	}
	if err == nil && p != nil {
		wait, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = p.StopAndWait(wait, 2*time.Second)
		cancel()
		if err == nil {
			r.mgr.removeProcessIfCurrent(oldKey, p)
		}
	}
	if err != nil {
		g.mu.Lock()
		g.phase = "blocked"
		g.mu.Unlock()
		return errBindBlocked
	}
	return nil
}
