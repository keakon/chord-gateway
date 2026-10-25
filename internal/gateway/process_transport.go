package gateway

import (
	"time"

	"github.com/keakon/golog/log"
)

// TransportErrorPayload describes a gateway-side JSONL failure, separate from
// Chord error envelopes that may already have a canonical notification.
type TransportErrorPayload struct {
	Message string
	Fatal   bool
}

func (p *ChordProcess) reportTransportError(err error, fatal bool) {
	now := time.Now()
	p.mu.Lock()
	p.state.LastError = err.Error()
	p.state.LastTransportError = &TransportErrorPayload{Message: err.Error(), Fatal: fatal}
	p.state.UpdatedAt = now.Format(time.RFC3339)
	p.lastActivity = now
	key, state, onEvent := p.key, p.state, p.onEvent
	p.mu.Unlock()

	// Do not log the raw frame: it may contain user input or tool output.
	log.Warnf("[%v] headless transport failed fatal=%v error=%v", processLogContext(key, state), fatal, err)
	if onEvent != nil {
		onEvent(key, "transport_error", state)
	}
}
