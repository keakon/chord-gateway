package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestParseAllowArgs(t *testing.T) {
	cases := []struct {
		rest          string
		wantScope     string
		wantRequestID string
		wantInvalid   bool
		name          string
	}{
		{name: "bare", rest: ""},
		{name: "request id", rest: "req-1", wantRequestID: "req-1"},
		{name: "request id at-sigil", rest: "@req-1", wantRequestID: "req-1"},
		{name: "session flag", rest: "--session", wantScope: "session"},
		{name: "project flag", rest: "--project", wantScope: "project"},
		{name: "user flag maps to user_global", rest: "--user", wantScope: "user_global"},
		{name: "short flag", rest: "-p", wantScope: "project"},
		{name: "flag and request id", rest: "req-9 --session", wantRequestID: "req-9", wantScope: "session"},
		{name: "explicit once", rest: "once"},
		{name: "once with project scope", rest: "once --project", wantInvalid: true},
		{name: "project scope with once", rest: "--project once", wantInvalid: true},
		// Misuse must be refused, never guessed at: remembering a wide rule is
		// hard to undo, and matching a request by accident is worse than no-op.
		{name: "two scopes", rest: "--session --project", wantInvalid: true},
		{name: "two request ids", rest: "req-1 req-2", wantInvalid: true},
		{name: "unknown flag", rest: "--always", wantInvalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope, requestID, invalid := parseAllowArgs(tc.rest)
			if scope != tc.wantScope || requestID != tc.wantRequestID || invalid != tc.wantInvalid {
				t.Fatalf("parseAllowArgs(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.rest, scope, requestID, invalid, tc.wantScope, tc.wantRequestID, tc.wantInvalid)
			}
		})
	}
}

func TestFormatConfirmRuleIntentReceiptPartialPersistence(t *testing.T) {
	receipt := ConfirmRuleIntentResult{
		RequestID: "confirm-1",
		Results: []ConfirmRulePersistenceResult{
			{Tool: "Shell", Pattern: "git status"},
			{Tool: "Delete", Pattern: "sample.txt", Error: "permission file is read-only"},
		},
	}
	got := formatConfirmRuleIntentReceipt(receipt, 2, "project")
	for _, want := range []string{"allowed for this call", "1 of 2 rules remembered", "Delete", "sample.txt", "read-only"} {
		if !strings.Contains(got, want) {
			t.Fatalf("receipt %q does not include %q", got, want)
		}
	}
	if got := formatConfirmRuleIntentReceipt(receipt, 1, "project"); !strings.Contains(got, "incomplete") {
		t.Fatalf("incomplete receipt = %q", got)
	}
}

func TestParseIMCommandAllow(t *testing.T) {
	if got := parseIMCommand("/allow --session"); got.Type != "confirm" || got.Action != "allow" || got.RuleScope != "session" {
		t.Fatalf("/allow --session = %+v", got)
	}
	if got := parseIMCommand("/deny breakage"); got.Type != "confirm" || got.Action != "deny" || got.Reason != "breakage" {
		t.Fatalf("/deny breakage = %+v", got)
	}
}

func TestHandleConfirmCommandWaitsForRulePersistenceReceipt(t *testing.T) {
	oldTimeout := confirmRuleIntentWaitTimeout
	confirmRuleIntentWaitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { confirmRuleIntentWaitTimeout = oldTimeout })

	tests := []struct {
		name       string
		sendResult bool
		wantText   string
	}{
		{name: "success", sendResult: true, wantText: "1 rule remembered for the project scope"},
		{name: "timeout", wantText: "could not confirm whether the requested rules were saved"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, proc, sender, stdin, ws := newModelsTestRouter()
			proc.mu.Lock()
			proc.state.PendingConfirm = &ConfirmPayload{
				RequestID: "confirm-1",
				ToolName:  "Shell",
				NeedsApprovalRuleItems: []ConfirmRuleItem{
					{Tool: "Shell", Pattern: "git status"},
				},
			}
			proc.mu.Unlock()

			done := make(chan struct{})
			go func() {
				r.handleConfirmCommand(ws, "chat-1", im.IMCommand{
					Type: "confirm", Action: "allow", RuleScope: "project",
				}, im.IncomingMessage{IMType: "wechat", ChatID: "chat-1", SenderID: "user-1"}, proc.key, proc)
				close(done)
			}()
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				proc.mu.RLock()
				waiting := len(proc.confirmRuleWaiters.waiters) == 1
				proc.mu.RUnlock()
				if waiting {
					break
				}
				time.Sleep(time.Millisecond)
			}
			proc.mu.RLock()
			waiting := len(proc.confirmRuleWaiters.waiters) == 1
			proc.mu.RUnlock()
			if !waiting {
				t.Fatal("confirmation handler did not register its receipt waiter")
			}
			if !strings.Contains(stdin.String(), `"type":"confirm"`) {
				t.Fatalf("confirm command was not sent: %q", stdin.String())
			}
			if tc.sendResult {
				payload, err := json.Marshal(ConfirmRuleIntentResult{
					RequestID: "confirm-1",
					Results:   []ConfirmRulePersistenceResult{{Tool: "Shell", Pattern: "git status"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				proc.processEnvelope(&HeadlessEnvelope{Type: "confirm_rule_intent_result", Payload: payload})
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("confirmation handler did not finish")
			}
			if got := sender.lastMessage().text; !strings.Contains(got, tc.wantText) {
				t.Fatalf("reply = %q, want %q", got, tc.wantText)
			}
		})
	}
}

// TestConfirmPayloadRuleCandidates pins the preference order and the fallback:
// Chord sends structured items when it has them, and older wire data carries
// only bare patterns, which must still be usable as rule items.
func TestConfirmPayloadRuleCandidates(t *testing.T) {
	structured := []ConfirmRuleItem{{Tool: "Shell", Pattern: "git *"}}
	withItems := &ConfirmPayload{NeedsApprovalRuleItems: structured, NeedsApprovalRules: []string{"ignored"}}
	if got := withItems.RuleCandidates(); len(got) != 1 || got[0] != structured[0] {
		t.Fatalf("RuleCandidates() = %#v, want the structured items", got)
	}

	patternsOnly := &ConfirmPayload{NeedsApprovalRules: []string{"rm *", "  ", "git *"}}
	got := patternsOnly.RuleCandidates()
	if len(got) != 2 || got[0].Pattern != "rm *" || got[1].Pattern != "git *" {
		t.Fatalf("RuleCandidates() = %#v, want 2 non-blank patterns", got)
	}

	var empty *ConfirmPayload
	if got := empty.RuleCandidates(); got != nil {
		t.Fatalf("RuleCandidates() on a nil payload = %#v, want nil", got)
	}
	if got := (&ConfirmPayload{}).RuleCandidates(); len(got) != 0 {
		t.Fatalf("RuleCandidates() with no rules = %#v, want empty", got)
	}
}

func TestConfirmRuleScopesCoverChord(t *testing.T) {
	// Chord rejects anything outside session|project|user_global, so this map is
	// the only thing standing between a typo and a refused command.
	seen := map[string]bool{}
	for flag, scope := range confirmRuleScopes {
		switch scope {
		case "session", "project", "user_global":
		default:
			t.Errorf("%s maps to unknown scope %q", flag, scope)
		}
		seen[scope] = true
	}
	if len(seen) != 3 {
		t.Errorf("not every Chord scope is reachable: %#v", seen)
	}
}
