package main

import (
	"sync"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

type Admission struct {
	pairing *PairingService
	mu      sync.Mutex
	gates   map[im.ChatIdentity]*im.ChatGate
	closed  bool
}

func NewAdmission(p *PairingService) *Admission {
	return &Admission{pairing: p, gates: map[im.ChatIdentity]*im.ChatGate{}}
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
		lease, _, _ := g.Snapshot()
		if lease != nil {
			lease.Close()
		}
	}
}
func (a *Admission) gate(id im.ChatIdentity, workspace string, epoch uint64) *im.ChatGate {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	g := a.gates[id]
	if g == nil {
		g = im.NewChatGate()
		if workspace != "" {
			g.Install(id, workspace, epoch)
		}
		a.gates[id] = g
	}
	return g
}

func (a *Admission) Authorize(msg im.IncomingMessage) (im.AdmissionTicket, bool) {
	return a.authorize(msg, true)
}

func (a *Admission) authorize(msg im.IncomingMessage, chargePairing bool) (im.AdmissionTicket, bool) {
	p := a.pairing
	cfg := p.store.Snapshot()
	imCfg := cfg.IMConfigByType(msg.IMType)
	ticket := im.AdmissionTicket{Identity: im.ChatIdentity{Platform: msg.IMType, AccountID: msg.AccountID, ChatID: msg.ChatID}, SenderID: msg.SenderID, ReceivedAt: p.now()}
	if imCfg == nil || p.store.faulted.Load() || msg.AccountID == "" || msg.ChatID == "" || msg.SenderID == "" || msg.MessageID == "" || !isValidIngressChatKind(msg) {
		return ticket, false
	}
	p.mu.Lock()
	state := p.platforms[msg.IMType]
	if state == nil || !state.ready || state.changed || state.account != msg.AccountID {
		p.mu.Unlock()
		return ticket, false
	}
	ticket.AccountEpoch = state.epoch
	owner := imCfg.OwnerIdentity()
	if owner != nil {
		if owner.AccountID != msg.AccountID || owner.UserID != msg.SenderID {
			p.mu.Unlock()
			return ticket, false
		}
		switch {
		case !msg.ForceText && isBindCommand(msg.Text):
			ticket.Kind = "paired_bind"
		case imCfg.Bindings()[msg.ChatID] == "":
			ticket.Kind = "owner_unbound"
		default:
			ticket.Kind = "paired_task"
		}
	} else {
		if msg.ChatKind != "private" || msg.InternalAction != nil || msg.ForceText {
			p.mu.Unlock()
			return ticket, false
		}
		if !isBindCommand(msg.Text) {
			// An unpaired private sender learns why the bot is silent instead
			// of being dropped without a trace. Group chats stay silent.
			if chargePairing && p.hintAllowed(state, msg.ChatID) {
				ticket.Kind = "pairing_hint_text"
			}
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
		if err != nil || len(args) != 2 {
			if chargePairing && p.hintAllowed(state, msg.ChatID) {
				ticket.Kind = "pairing_hint_usage"
			}
			p.mu.Unlock()
			return ticket, false
		}
		if !p.codeValid(args[1]) {
			if chargePairing && p.hintAllowed(state, msg.ChatID) {
				ticket.Kind = "pairing_hint_code"
			}
			p.mu.Unlock()
			return ticket, false
		}
		ticket.Kind = "paired_bind"
		ticket.PairingNonceID = p.codeID
	}
	p.mu.Unlock()
	ticket.Gate = a.gate(ticket.Identity, imCfg.Bindings()[msg.ChatID], ticket.AccountEpoch)
	if ticket.Gate == nil {
		return ticket, false
	}
	lease, generation, phase := ticket.Gate.Snapshot()
	ticket.BindingGeneration = generation
	if ticket.Kind == "paired_task" && (phase != "active" || lease == nil || lease.WorkspaceID != imCfg.Bindings()[msg.ChatID]) {
		return ticket, false
	}
	return ticket, true
}

// Valid rechecks identity without upgrading the ticket's original input class.
// Transport readiness gates new admission, not already accepted owner work.
func (a *Admission) Valid(t im.AdmissionTicket) bool {
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
	imCfg := p.store.Snapshot().IMConfigByType(t.Identity.Platform)
	if imCfg == nil {
		return false
	}
	if t.IsPairingHint() {
		// A hint carries no authority beyond one bounded reply text and has no
		// gate, so it is valid only while the platform is still unpaired. Once
		// an owner appears the ticket must expire rather than be upgraded to
		// owner authority or reach the nil gate below.
		if imCfg.OwnerIdentity() != nil {
			return false
		}
		return s.ready
	}
	owner := imCfg.OwnerIdentity()
	if owner == nil {
		// Re-validate the binding ticket against the live pairing code: a code
		// replaced while the probe is in flight must not authorize a bind.
		return s.ready && t.Kind == "paired_bind" && t.PairingNonceID != "" && t.PairingNonceID == p.codeID && p.now().Before(p.codeExpires)
	}
	if owner.UserID != t.SenderID || owner.AccountID != t.Identity.AccountID {
		return false
	}
	_, generation, phase := t.Gate.Snapshot()
	return t.Kind != "paired_task" || (generation == t.BindingGeneration && phase == "active")
}

func admittedMessage(router im.MessageRouter, msg im.IncomingMessage) (im.IncomingMessage, bool) {
	if r, ok := router.(*NotificationRouter); ok && r.admission != nil {
		ticket, allowed := r.admission.Authorize(msg)
		if !allowed {
			return msg, false
		}
		msg.SetAdmissionTicket(&ticket)
		return msg, true
	}
	return msg, false
}
