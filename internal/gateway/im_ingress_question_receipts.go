package gateway

import "github.com/keakon/chord-gateway/internal/im"

type ingressQuestionReceipt struct {
	process *ChordProcess
	receipt QuestionReceipt
}

// enqueueIngressQuestionReceipt runs after stdout releases the process lock.
// Backpressure preserves the unresolved control and stops further delivery;
// stdout never waits for receipt settlement or creates unbounded receipt goroutines.
func (r *NotificationRouter) enqueueIngressQuestionReceipt(p *ChordProcess, receipt QuestionReceipt) bool {
	if r.ingress == nil {
		return true
	}
	s := r.ingress
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.stopping || s.ctx.Err() != nil {
		return false
	}
	select {
	case s.questionReceipts <- ingressQuestionReceipt{p, receipt}:
		return true
	default:
		return false
	}
}

func (s *IngressService) runQuestionReceipts() {
	defer s.wg.Done()
	for {
		select {
		case item := <-s.questionReceipts:
			s.applyQuestionReceipt(item)
			s.signal()
		case <-s.ctx.Done():
			// Serialize parent cancellation with producers as Close does.
			// A producer that passed its check must finish enqueueing before
			// the final drain; later producers cannot orphan a receipt.
			s.mu.Lock()
			s.stopping = true
			s.mu.Unlock()
			// Settle queued receipts before ingress closes.
			for {
				select {
				case item := <-s.questionReceipts:
					s.applyQuestionReceipt(item)
				default:
					return
				}
			}
		}
	}
}

func (s *IngressService) applyQuestionReceipt(item ingressQuestionReceipt) {
	if err := s.router.ObserveIngressQuestionReceipt(item.process, item.receipt); err != nil {
		item.process.mu.Lock()
		item.process.inputReceiptBlocked = true
		item.process.state.LastError = "ingress_unavailable: question receipt update unavailable; check ingress and the original session"
		key := item.process.key
		item.process.mu.Unlock()
		_, platform, _ := parseProcessKey(key)
		s.store.metrics.record(platform, "receipt_update_failed", "ingress_unavailable")
	}
}

func (s *IngressService) canRetryIngressQuestion(control ingressControl, records []IngressRecord) bool {
	if control.message.AdmissionTicket() == nil {
		return false
	}
	s.mu.Lock()
	blocked := s.routeBlockedLocked(control.message.AdmissionTicket().Identity)
	s.mu.Unlock()
	if blocked {
		return false
	}
	ids := s.router.IngressQuestionRetryRecords(control.message, control.target)
	if len(ids) == 0 {
		return false
	}
	matched := make(map[string]bool, len(ids))
	for _, id := range ids {
		matched[id] = true
	}
	found := false
	for _, rec := range records {
		if rec.State == im.IngressUnknown || rec.State == im.IngressDispatching {
			if rec.Kind != "control" || !matched[rec.RecordID] {
				return false
			}
			found = true
		}
	}
	return found
}

// currentIngressBlockers rechecks late/explicit Question receipts before
// advancing ordinary input. It never guesses success from the runtime state.
func (s *IngressService) currentIngressBlockers(id im.ChatIdentity) (blocked, unknown bool) {
	s.mu.Lock()
	blocked = s.routeBlockedLocked(id)
	s.mu.Unlock()
	unknown = blocked
	records := s.store.ListRoutePendingMetadata(id)
	for _, rec := range records {
		if rec.State == im.IngressUnknown {
			unknown = true
		}
		if rec.State == im.IngressUnknown || rec.State == im.IngressDispatching {
			blocked = true
		}
	}
	return blocked, unknown
}
