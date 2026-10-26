package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var confirmRuleIntentWaitTimeout = 15 * time.Second

type ConfirmRuleIntentResult struct {
	RequestID string                         `json:"request_id"`
	Results   []ConfirmRulePersistenceResult `json:"results"`
}

type ConfirmRulePersistenceResult struct {
	Tool    string `json:"tool"`
	Pattern string `json:"pattern"`
	Error   string `json:"error,omitempty"`
}

var errConfirmRuleResultAlreadyPending = errors.New("confirmation rule result is already pending")

type confirmRuleIntentWaiters struct {
	waiters map[string]chan ConfirmRuleIntentResult
}

func (w *confirmRuleIntentWaiters) add(id string, ch chan ConfirmRuleIntentResult) bool {
	if w.waiters == nil {
		w.waiters = make(map[string]chan ConfirmRuleIntentResult)
	}
	if _, exists := w.waiters[id]; exists {
		return false
	}
	w.waiters[id] = ch
	return true
}

func (w *confirmRuleIntentWaiters) remove(id string, ch chan ConfirmRuleIntentResult) {
	if w.waiters[id] == ch {
		delete(w.waiters, id)
	}
}

func (w *confirmRuleIntentWaiters) take(id string) chan ConfirmRuleIntentResult {
	ch := w.waiters[id]
	delete(w.waiters, id)
	return ch
}

// notifyConfirmRuleIntentResult runs under p.mu and drops late results for
// requests whose caller has stopped waiting.
func (p *ChordProcess) notifyConfirmRuleIntentResult(result ConfirmRuleIntentResult) {
	ch := p.confirmRuleWaiters.take(result.RequestID)
	if ch == nil {
		return
	}
	select {
	case ch <- result:
	default:
	}
}

// WaitConfirmRuleIntentResult sends an approval carrying remembered rules and
// waits for Chord's correlated receipt after the rule writes finish.
func (p *ChordProcess) WaitConfirmRuleIntentResult(ctx context.Context, requestID string, cmd map[string]any) (ConfirmRuleIntentResult, bool, error) {
	if requestID == "" {
		return ConfirmRuleIntentResult{}, false, errors.New("confirmation request id is required")
	}
	ch := make(chan ConfirmRuleIntentResult, 1)
	p.mu.Lock()
	if !p.confirmRuleWaiters.add(requestID, ch) {
		p.mu.Unlock()
		return ConfirmRuleIntentResult{}, false, errConfirmRuleResultAlreadyPending
	}
	p.mu.Unlock()

	if err := p.SendCommand(cmd); err != nil {
		p.mu.Lock()
		p.confirmRuleWaiters.remove(requestID, ch)
		p.mu.Unlock()
		return ConfirmRuleIntentResult{}, false, err
	}
	select {
	case result := <-ch:
		return result, true, nil
	case <-ctx.Done():
		p.mu.Lock()
		p.confirmRuleWaiters.remove(requestID, ch)
		p.mu.Unlock()
		return ConfirmRuleIntentResult{}, true, ctx.Err()
	}
}

func formatConfirmRuleIntentReceipt(receipt ConfirmRuleIntentResult, expected int, scope string) string {
	if len(receipt.Results) != expected {
		return "✅ allowed for this call; Chord returned an incomplete rule-persistence receipt. Check the rules on the Chord host before relying on them."
	}
	saved := 0
	var failures []ConfirmRulePersistenceResult
	for _, result := range receipt.Results {
		if strings.TrimSpace(result.Error) == "" {
			saved++
		} else {
			failures = append(failures, result)
		}
	}
	if len(failures) == 0 {
		word := "rules"
		if saved == 1 {
			word = "rule"
		}
		return fmt.Sprintf("✅ allowed; %d %s remembered for the %s scope", saved, word, confirmRuleScopeLabel(scope))
	}
	text := fmt.Sprintf("✅ allowed for this call; %d of %d rules remembered for the %s scope.", saved, expected, confirmRuleScopeLabel(scope))
	if saved == 0 {
		text = fmt.Sprintf("✅ allowed for this call; no rules were remembered for the %s scope.", confirmRuleScopeLabel(scope))
	}
	for i, result := range failures {
		if i == 3 {
			text += fmt.Sprintf("\n  • and %d more failure(s)", len(failures)-i)
			break
		}
		detail := strings.Join(strings.Fields(result.Error), " ")
		if detail == "" {
			detail = "persistence failed"
		}
		text += fmt.Sprintf("\n  • %s %q: %s", result.Tool, result.Pattern, detail)
	}
	return truncate(text)
}
