package main

import (
	"context"
	"sync"
)

type ChatIdentity struct{ Platform, AccountID, ChatID string }

type BindingLease struct {
	Identity                 ChatIdentity
	WorkspaceID              string
	AccountEpoch, Generation uint64
	ctx                      context.Context
	cancel                   context.CancelFunc
	mu                       sync.Mutex
	closing                  bool
	sends                    int
	drained                  chan struct{}
}

func newBindingLease(id ChatIdentity, workspace string, epoch, generation uint64) *BindingLease {
	ctx, cancel := context.WithCancel(context.Background())
	return &BindingLease{Identity: id, WorkspaceID: workspace, AccountEpoch: epoch, Generation: generation, ctx: ctx, cancel: cancel, drained: make(chan struct{})}
}
func (l *BindingLease) BeginSend() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing {
		return false
	}
	l.sends++
	return true
}
func (l *BindingLease) EndSend() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sends--
	if l.closing && l.sends == 0 {
		close(l.drained)
	}
}
func (l *BindingLease) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing {
		return
	}
	l.closing = true
	l.cancel()
	if l.sends == 0 {
		close(l.drained)
	}
}
func (l *BindingLease) Wait(ctx context.Context) error {
	select {
	case <-l.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type ChatGate struct {
	opMu       sync.Mutex
	mu         sync.Mutex
	generation uint64
	phase      string
	lease      *BindingLease
}

func (g *ChatGate) snapshot() (*BindingLease, uint64, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lease, g.generation, g.phase
}
func (g *ChatGate) install(id ChatIdentity, workspace string, epoch uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.generation == 0 {
		g.generation = 1
	}
	g.lease = newBindingLease(id, workspace, epoch, g.generation)
	g.phase = "active"
}
func (g *ChatGate) switching() *BindingLease {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.generation++
	g.phase = "switching"
	if g.lease != nil {
		g.lease.Close()
	}
	return g.lease
}

// chatOperation captures the binding before asynchronous work starts. A later
// operation may acquire the gate only while that exact lease is still current.
type chatOperation struct {
	manager *ChordManager
	key     string
	gate    *ChatGate
	lease   *BindingLease
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
		return nil, errPairingDenied
	}
	identity := ChatIdentity{Platform: platform, ChatID: chat}
	epoch := uint64(0)
	if platform != "console" {
		owner := cfg.IMConfigByType(platform).OwnerIdentity()
		if owner == nil {
			return nil, errPairingDenied
		}
		identity.AccountID = owner.AccountID
		p := m.admission.pairing
		p.mu.Lock()
		state := p.platforms[platform]
		if state == nil || state.changed || state.account != owner.AccountID {
			p.mu.Unlock()
			return nil, errPairingDenied
		}
		epoch = state.epoch
		p.mu.Unlock()
	}
	operation.gate = m.admission.gate(identity, workspace, epoch)
	if operation.gate == nil {
		return nil, ErrManagerShuttingDown
	}
	operation.lease, _, _ = operation.gate.snapshot()
	if operation.lease == nil {
		return nil, errBindBlocked
	}
	return operation, nil
}

func (o *chatOperation) context() context.Context {
	if o.lease != nil {
		return o.lease.ctx
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
	o.gate.opMu.Lock()
	if !o.validLocked() {
		o.gate.opMu.Unlock()
		return nil, errPairingDenied
	}
	return o.gate.opMu.Unlock, nil
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
	lease, generation, phase := o.gate.snapshot()
	if phase != "active" || lease != o.lease || lease.ctx.Err() != nil {
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
		return owner != nil && m.admission.Valid(AdmissionTicket{Kind: "paired_task", Identity: lease.Identity, SenderID: owner.UserID, AccountEpoch: lease.AccountEpoch, BindingGeneration: generation, gate: o.gate})
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
