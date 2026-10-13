package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const maxPendingInputs = 128

const (
	inputHandled  = "handled"
	inputQueued   = "queued"
	inputStarted  = "started"
	inputRejected = "rejected"
)

type InputResultPayload struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
	TurnID    uint64 `json:"turn_id,omitempty"`
	Message   string `json:"message,omitempty"`
}

// SendUserMessage records only correlation IDs. A successful write is not
// evidence that Chord consumed the input or started model work.
func (p *ChordProcess) SendUserMessage(content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inputBlockedLocked() {
		return ErrSessionTransferred
	}
	if p.stdin == nil {
		return io.ErrClosedPipe
	}
	if len(p.pendingInputs) >= maxPendingInputs {
		return fmt.Errorf("too many unconfirmed inputs; wait for Chord to respond")
	}
	// Chord also uses input IDs for durable question lifecycle operations.
	// They must stay unique when this process resumes an existing session.
	id := "input-" + rand.Text()
	data, err := json.Marshal(map[string]string{"type": "send", "content": content, "request_id": id})
	if err != nil {
		return err
	}
	if p.pendingInputs == nil {
		p.pendingInputs = make(map[string]struct{})
	}
	p.pendingInputs[id] = struct{}{}
	err = p.writeCommandLocked(data)
	if err != nil {
		delete(p.pendingInputs, id)
	}
	return err
}

// applyInputResult runs under p.mu. Every input gets one consumption reply.
// Queued inputs start work later via activity, never by a second reply.
func (p *ChordProcess) applyInputResult(env *HeadlessEnvelope, now time.Time) bool {
	var result InputResultPayload
	if json.Unmarshal(env.Payload, &result) != nil {
		return false
	}
	switch result.Status {
	case inputHandled, inputQueued, inputStarted, inputRejected:
	default:
		return false
	}
	if _, ok := p.pendingInputs[result.RequestID]; !ok {
		return false
	}
	delete(p.pendingInputs, result.RequestID)
	if env.Seq < p.state.LastEnvelopeSeq {
		return false
	}
	p.state.LastInputResult = &result
	if result.Status == inputStarted {
		p.state.setAgentBusy(true)
		p.state.LastPushAt = now
		p.state.InternalEventsSinceLastPush = 0
		p.state.ensureTurnWindow(now)
	}
	return true
}

func (p *ChordProcess) abandonPendingInputs() {
	p.state.UnconfirmedInputs += len(p.pendingInputs)
	p.pendingInputs = nil
}
