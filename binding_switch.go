package main

import (
	"context"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// The caller owns g.OpMu. Reserve retirement under the same lifecycle lock as
// outgoing takeover admission. No process or delivery waits may hold that lock:
// the stdout reader needs it to finish processing events and record the exit.
func (m *ChordManager) beginBindingSwitch(g *im.ChatGate, oldKey, newKey string) (*ChordProcess, *im.BindingLease, error) {
	m.lifecycleGate.RLock()
	defer m.lifecycleGate.RUnlock()
	release := m.acquireKeyLock(oldKey)
	defer release()
	if m.shuttingDown.Load() {
		return nil, nil, ErrManagerShuttingDown
	}
	_, _, phase := g.Snapshot()
	if phase == "blocked" || phase == "switching" {
		return nil, nil, errBindBlocked
	}
	if m.transferBlocked(oldKey) || m.transferBlocked(newKey) {
		return nil, nil, errBindBusy
	}
	p := m.GetProcessForKey(oldKey)
	if p != nil {
		p.mu.Lock()
		if p.hasGuardedInputsLocked() {
			p.mu.Unlock()
			return nil, nil, errUnresolvedInputs
		}
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
	return p, g.Switching(), nil
}

// Directory switches drain output and confirm process shutdown before
// publishing new routing. Failure leaves the gate closed until local recovery.
func (r *NotificationRouter) prepareBindingSwitch(ctx context.Context, g *im.ChatGate, oldKey, newKey string) error {
	if p := r.mgr.GetProcessForKey(oldKey); p != nil {
		if err := p.refreshPendingInputStatus(ctx); err != nil {
			return err
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
		g.Block()
		return errBindBlocked
	}
	return nil
}
