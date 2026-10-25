package main

import (
	"context"

	"github.com/keakon/chord-gateway/internal/im"
)

// chatOperation captures the binding before asynchronous work starts. A later
// operation may acquire the gate only while that exact lease is still current.
type chatOperation struct {
	manager *ChordManager
	key     string
	gate    *im.ChatGate
	lease   *im.BindingLease
}

func (m *ChordManager) captureChatOperation(key string) (*chatOperation, error) {
	operation := &chatOperation{manager: m, key: key}
	if m.admission == nil {
		return operation, nil
	}
	workspace, platform, chat := parseProcessKey(key)
	cfg := m.Config()
	ws, err := cfg.ResolveWorkspace(platform, chat)
	if err != nil || ws.ID != workspace {
		return nil, im.ErrPairingDenied
	}
	identity := im.ChatIdentity{Platform: platform, ChatID: chat}
	epoch := uint64(0)
	if platform != "console" {
		owner := cfg.IMConfigByType(platform).OwnerIdentity()
		if owner == nil {
			return nil, im.ErrPairingDenied
		}
		identity.AccountID = owner.AccountID
		p := m.admission.pairing
		p.mu.Lock()
		state := p.platforms[platform]
		if state == nil || state.changed || state.account != owner.AccountID {
			p.mu.Unlock()
			return nil, im.ErrPairingDenied
		}
		epoch = state.epoch
		p.mu.Unlock()
	}
	operation.gate = m.admission.gate(identity, workspace, epoch)
	if operation.gate == nil {
		return nil, ErrManagerShuttingDown
	}
	operation.lease, _, _ = operation.gate.Snapshot()
	if operation.lease == nil {
		return nil, errBindBlocked
	}
	return operation, nil
}

func (o *chatOperation) context() context.Context {
	if o.lease != nil {
		return o.lease.Context()
	}
	return context.Background()
}

func (o *chatOperation) acquire() (func(), error) {
	m := o.manager
	if m.shuttingDown.Load() {
		return nil, ErrManagerShuttingDown
	}
	if o.gate == nil {
		return func() {}, nil
	}
	o.gate.OpMu.Lock()
	if !o.validLocked() {
		o.gate.OpMu.Unlock()
		return nil, im.ErrPairingDenied
	}
	return o.gate.OpMu.Unlock, nil
}

// validLocked is also checked after external work: account invalidation can
// cancel a lease without waiting for the chat operation mutex.
func (o *chatOperation) validLocked() bool {
	m := o.manager
	if m.shuttingDown.Load() {
		return false
	}
	if o.gate == nil {
		return true
	}
	lease, generation, phase := o.gate.Snapshot()
	if phase != "active" || lease != o.lease || lease.Context().Err() != nil {
		return false
	}
	cfg := m.Config()
	workspace, platform, chat := parseProcessKey(o.key)
	ws, err := cfg.ResolveWorkspace(platform, chat)
	if err != nil || ws.ID != workspace || lease.WorkspaceID != workspace {
		return false
	}
	if platform != "console" {
		owner := cfg.IMConfigByType(platform).OwnerIdentity()
		return owner != nil && m.admission.Valid(im.AdmissionTicket{Kind: "paired_task", Identity: lease.Identity, SenderID: owner.UserID, AccountEpoch: lease.AccountEpoch, BindingGeneration: generation, Gate: o.gate})
	}
	return true
}

// Router dispatch already owns the chat gate and uses internal locked helpers.
func (m *ChordManager) acquireChatOperation(key string) (func(), error) {
	operation, err := m.captureChatOperation(key)
	if err != nil {
		return nil, err
	}
	return operation.acquire()
}
