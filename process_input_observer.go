package main

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

var errIngressDonePending = errors.New("new Done confirmation requires explicit handling")

var ErrInputReceiptBackpressure = errors.New("input_receipt_backpressure: receipt application unavailable")

// ProcessInputReceipt identifies the actual process/session that accepted an
// input. The callback must apply this result to process-local intake before returning nil.
type ProcessInputReceipt struct {
	RequestID    string
	RuntimeID    string
	SessionID    string
	SessionEpoch uint64
	Status       string
	Message      string
}

type processInputObserver struct {
	receipt     ProcessInputReceipt
	commit      func(ProcessInputReceipt) error
	timer       *time.Timer
	version     uint64
	queued      bool
	unconfirmed bool
}

// InputReceiptIdentity snapshots the identity used when recording dispatching.
func (p *ChordProcess) InputReceiptIdentity() ProcessInputReceipt {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return ProcessInputReceipt{RuntimeID: p.runtimeID, SessionID: p.state.SessionID, SessionEpoch: p.sessionEpoch}
}

// ObserveInputReceipt must be called after dispatching is recorded in memory and
// before writing stdin. At most 128 observers and queued receipts exist per
// process, including uncertain results retained for late authoritative replies.
func (p *ChordProcess) ObserveInputReceipt(requestID string, commit func(ProcessInputReceipt) error, expected ...ProcessInputReceipt) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(expected) > 0 {
		r := expected[0]
		if r.RuntimeID != p.runtimeID || r.SessionID != p.state.SessionID || r.SessionEpoch != p.sessionEpoch {
			return fmt.Errorf("input receipt identity changed")
		}
	}
	if requestID == "" || commit == nil {
		return fmt.Errorf("invalid input receipt observer")
	}
	if p.mgr != nil && p.mgr.shuttingDown.Load() {
		return ErrManagerShuttingDown
	}
	if p.inputReceiptShutdown || p.inputReceiptBlocked || len(p.inputObservers) >= maxPendingInputs {
		return ErrInputReceiptBackpressure
	}
	if p.inputObservers[requestID] != nil {
		return fmt.Errorf("input receipt observer already registered")
	}
	if p.inputObservers == nil {
		p.inputObservers = make(map[string]*processInputObserver)
	}
	observer := &processInputObserver{receipt: ProcessInputReceipt{RequestID: requestID, RuntimeID: p.runtimeID, SessionID: p.state.SessionID, SessionEpoch: p.sessionEpoch}, commit: commit}
	p.inputObservers[requestID] = observer
	if p.mgr != nil {
		p.mgr.inputReceiptProcesses.Store(p, struct{}{})
	}
	observer.timer = time.AfterFunc(30*time.Second, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.inputObservers[requestID] == observer {
			p.queueInputReceiptLocked(requestID, inputUnknown, "receipt_timeout")
		}
	})
	return nil
}

func (p *ChordProcess) inputObserverMatchesLocked(observer *processInputObserver) bool {
	r := observer.receipt
	return r.RuntimeID == p.runtimeID && r.SessionID == p.state.SessionID && r.SessionEpoch == p.sessionEpoch
}

func (p *ChordProcess) observeInputResultLocked(result *InputResultPayload) bool {
	observer := p.inputObservers[result.RequestID]
	if observer == nil || !p.inputObserverMatchesLocked(observer) {
		return false
	}
	p.queueInputReceiptLocked(result.RequestID, result.Status, result.Message)
	return true
}

// queueInputReceiptLocked only copies small metadata; no receipt callback
// runs while holding the process lock. Duplicate receipts occupy one slot.
func (p *ChordProcess) queueInputReceiptLocked(id, status, message string) {
	observer := p.inputObservers[id]
	if observer == nil || p.inputReceiptShutdown {
		return
	}
	if observer.receipt.Status != "" && observer.receipt.Status != inputUnknown {
		return
	}
	if status == inputUnknown && !observer.unconfirmed {
		observer.unconfirmed = true
		p.state.UnconfirmedInputs++
	}
	observer.receipt.Status = status
	if len(message) > 512 {
		message = message[:512]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	observer.receipt.Message = message
	observer.version++
	if !observer.queued {
		if len(p.inputReceiptQueue) >= maxPendingInputs {
			p.inputReceiptBlocked = true
			p.state.LastError = ErrInputReceiptBackpressure.Error()
			return
		}
		observer.queued = true
		p.inputReceiptQueue = append(p.inputReceiptQueue, id)
	}
	p.startInputReceiptWorkerLocked()
}

func (p *ChordProcess) startInputReceiptWorkerLocked() {
	if p.inputReceiptWorker || len(p.inputReceiptQueue) == 0 {
		return
	}
	p.inputReceiptWorker = true
	p.inputReceiptDone = make(chan struct{})
	go p.flushInputReceipts()
}

func (p *ChordProcess) flushInputReceipts() {
	p.mu.RLock()
	done := p.inputReceiptDone
	p.mu.RUnlock()
	defer close(done)
	for {
		p.mu.Lock()
		if len(p.inputReceiptQueue) == 0 {
			p.inputReceiptWorker = false
			p.inputReceiptBlocked = false
			p.mu.Unlock()
			return
		}
		id := p.inputReceiptQueue[0]
		observer := p.inputObservers[id]
		receipt, version, commit := observer.receipt, observer.version, observer.commit
		p.mu.Unlock()
		err := commit(receipt)
		p.mu.Lock()
		if err != nil {
			p.inputReceiptBlocked = true
			p.inputReceiptWorker = false
			p.state.LastError = ErrInputReceiptBackpressure.Error()
			p.mu.Unlock()
			return
		}
		if observer.version != version {
			p.mu.Unlock()
			continue
		}
		p.inputReceiptQueue = p.inputReceiptQueue[1:]
		observer.queued = false
		if observer.timer != nil {
			observer.timer.Stop()
		}
		if receipt.Status != inputUnknown {
			if observer.unconfirmed && p.state.UnconfirmedInputs > 0 {
				p.state.UnconfirmedInputs--
			}
			delete(p.pendingInputs, id)
			delete(p.inputObservers, id)
			if len(p.inputObservers) == 0 && p.mgr != nil {
				p.mgr.inputReceiptProcesses.Delete(p)
			}
		}
		p.mu.Unlock()
	}
}

// WaitInputReceiptsShutdown seals receipt delivery and waits for all current
// receipt callbacks before the in-memory store is closed. Failed callbacks
// block new dispatch in this process; inputs are never replayed.
func (m *ChordManager) WaitInputReceiptsShutdown() {
	m.inputReceiptProcesses.Range(func(key, _ any) bool {
		p := key.(*ChordProcess)
		p.mu.Lock()
		for id, observer := range p.inputObservers {
			if observer.timer != nil {
				observer.timer.Stop()
			}
			p.queueInputReceiptLocked(id, inputUnknown, "gateway_shutdown")
		}
		p.inputReceiptShutdown = true
		p.startInputReceiptWorkerLocked()
		done := p.inputReceiptDone
		p.mu.Unlock()
		if done != nil {
			<-done
		}
		return true
	})
}
