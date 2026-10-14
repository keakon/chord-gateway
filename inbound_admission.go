package main

import (
	"sync"
	"time"
)

type AdmissionTicket struct {
	Kind                            string
	Identity                        ChatIdentity
	SenderID                        string
	AccountEpoch, BindingGeneration uint64
	PairingNonceID                  string
	ReceivedAt                      time.Time
	gate                            *ChatGate
}

type Admission struct {
	pairing *PairingService
	mu      sync.Mutex
	gates   map[ChatIdentity]*ChatGate
	closed  bool
}

func NewAdmission(p *PairingService) *Admission {
	return &Admission{pairing: p, gates: map[ChatIdentity]*ChatGate{}}
}
func (a *Admission) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.pairing.mu.Lock()
	for _, state := range a.pairing.platforms {
		if state.cancel != nil {
			state.cancel()
		}
	}
	a.pairing.mu.Unlock()
	for _, g := range a.gates {
		lease, _, _ := g.snapshot()
		if lease != nil {
			lease.Close()
		}
	}
}
func (a *Admission) gate(id ChatIdentity, workspace string, epoch uint64) *ChatGate {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	g := a.gates[id]
	if g == nil {
		g = &ChatGate{generation: 1, phase: "unbound"}
		if workspace != "" {
			g.install(id, workspace, epoch)
		}
		a.gates[id] = g
	}
	return g
}

func (a *Admission) Authorize(msg IncomingMessage) (AdmissionTicket, bool) {
	return a.authorize(msg, true)
}

func (a *Admission) authorize(msg IncomingMessage, chargePairing bool) (AdmissionTicket, bool) {
	p := a.pairing
	cfg := p.store.Snapshot()
	im := cfg.IMConfigByType(msg.IMType)
	ticket := AdmissionTicket{Identity: ChatIdentity{msg.IMType, msg.AccountID, msg.ChatID}, SenderID: msg.SenderID, ReceivedAt: p.now()}
	if im == nil || p.store.faulted.Load() || msg.AccountID == "" || msg.ChatID == "" || msg.SenderID == "" || msg.MessageID == "" || !isValidIngressChatKind(msg) {
		return ticket, false
	}
	p.mu.Lock()
	state := p.platforms[msg.IMType]
	if state == nil || !state.ready || state.changed || state.account != msg.AccountID {
		p.mu.Unlock()
		return ticket, false
	}
	ticket.AccountEpoch = state.epoch
	owner := im.OwnerIdentity()
	if owner != nil {
		if owner.AccountID != msg.AccountID || owner.UserID != msg.SenderID {
			p.mu.Unlock()
			return ticket, false
		}
		switch {
		case !msg.ForceText && isBindCommand(msg.Text):
			ticket.Kind = "paired_bind"
		case im.Bindings()[msg.ChatID] == "":
			ticket.Kind = "owner_unbound"
		default:
			ticket.Kind = "paired_task"
		}
	} else {
		if msg.ChatKind != "private" || msg.InternalAction != nil || msg.ForceText || !isBindCommand(msg.Text) {
			p.mu.Unlock()
			return ticket, false
		}
		if chargePairing {
			replenished := int(p.now().Sub(state.refilled) / (2 * time.Second))
			if replenished > 0 {
				state.tokens = min(30, state.tokens+replenished)
				state.refilled = state.refilled.Add(time.Duration(replenished) * 2 * time.Second)
			}
			if state.tokens == 0 {
				p.mu.Unlock()
				return ticket, false
			}
			state.tokens--
		}
		args, err := parsePairedBindArgs(msg.Text)
		if err != nil || len(args) != 2 || !p.codeValid(state, args[1]) {
			p.mu.Unlock()
			return ticket, false
		}
		ticket.Kind = "paired_bind"
		ticket.PairingNonceID = state.nonceID
	}
	p.mu.Unlock()
	ticket.gate = a.gate(ticket.Identity, im.Bindings()[msg.ChatID], ticket.AccountEpoch)
	if ticket.gate == nil {
		return ticket, false
	}
	lease, generation, phase := ticket.gate.snapshot()
	ticket.BindingGeneration = generation
	if ticket.Kind == "paired_task" && (phase != "active" || lease == nil || lease.WorkspaceID != im.Bindings()[msg.ChatID]) {
		return ticket, false
	}
	return ticket, true
}

// Valid rechecks identity without upgrading the ticket's original input class.
// Transport readiness gates new admission, not already accepted owner work.
func (a *Admission) Valid(t AdmissionTicket) bool {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed || a.pairing.store.faulted.Load() {
		return false
	}
	p := a.pairing
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.platforms[t.Identity.Platform]
	if s == nil || s.changed || s.account != t.Identity.AccountID || s.epoch != t.AccountEpoch {
		return false
	}
	im := p.store.Snapshot().IMConfigByType(t.Identity.Platform)
	if im == nil {
		return false
	}
	owner := im.OwnerIdentity()
	if owner == nil {
		return s.ready && t.Kind == "paired_bind" && t.PairingNonceID != "" && t.PairingNonceID == s.nonceID && !s.consumed && p.now().Before(s.expires)
	}
	if owner.UserID != t.SenderID || owner.AccountID != t.Identity.AccountID {
		return false
	}
	_, generation, phase := t.gate.snapshot()
	return t.Kind != "paired_task" || (generation == t.BindingGeneration && phase == "active")
}

func admittedMessage(router MessageRouter, msg IncomingMessage) (IncomingMessage, bool) {
	if r, ok := router.(*NotificationRouter); ok && r.admission != nil {
		ticket, allowed := r.admission.Authorize(msg)
		if !allowed {
			return msg, false
		}
		msg.ticket = &ticket
		return msg, true
	}
	return msg, false
}
