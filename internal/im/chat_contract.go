package im

import (
	"context"
	"errors"
	"sync"
)

// ErrPairingDenied reports that pairing authorization is no longer valid.
var ErrPairingDenied = errors.New("pairing authorization is no longer valid")

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

func NewBindingLease(id ChatIdentity, workspace string, epoch, generation uint64) *BindingLease {
	ctx, cancel := context.WithCancel(context.Background())
	return &BindingLease{Identity: id, WorkspaceID: workspace, AccountEpoch: epoch, Generation: generation, ctx: ctx, cancel: cancel, drained: make(chan struct{})}
}

// Context returns the lease's lifecycle context; it is cancelled by Close.
func (l *BindingLease) Context() context.Context {
	return l.ctx
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
	OpMu       sync.Mutex
	mu         sync.Mutex
	generation uint64
	phase      string
	lease      *BindingLease
}

// NewChatGate returns a gate in the "unbound" phase with generation 1.
func NewChatGate() *ChatGate {
	return &ChatGate{generation: 1, phase: "unbound"}
}

func (g *ChatGate) Snapshot() (*BindingLease, uint64, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lease, g.generation, g.phase
}
func (g *ChatGate) Install(id ChatIdentity, workspace string, epoch uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.generation == 0 {
		g.generation = 1
	}
	g.lease = NewBindingLease(id, workspace, epoch, g.generation)
	g.phase = "active"
}
func (g *ChatGate) Switching() *BindingLease {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.generation++
	g.phase = "switching"
	if g.lease != nil {
		g.lease.Close()
	}
	return g.lease
}

// Block marks the gate as permanently blocked after a failed switch. A blocked
// gate rejects all subsequent operations until the process restarts it.
func (g *ChatGate) Block() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.phase = "blocked"
}

// SetUnbound resets the gate to the "unbound" phase with no lease.
func (g *ChatGate) SetUnbound() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.phase = "unbound"
}
