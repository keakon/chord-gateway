package gateway

import (
	"encoding/json"
	"testing"
)

func TestConfirmRuleIntentResultIsCorrelated(t *testing.T) {
	p := &ChordProcess{}
	ch := make(chan ConfirmRuleIntentResult, 1)
	p.confirmRuleWaiters.add("confirm-1", ch)

	payload, err := json.Marshal(ConfirmRuleIntentResult{
		RequestID: "confirm-2",
		Results:   []ConfirmRulePersistenceResult{{Tool: "Shell", Pattern: "git status"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_rule_intent_result", Payload: payload})
	if len(ch) != 0 {
		t.Fatal("unmatched request id settled the waiter")
	}

	payload, err = json.Marshal(ConfirmRuleIntentResult{
		RequestID: "confirm-1",
		Results: []ConfirmRulePersistenceResult{{
			Tool: "Shell", Pattern: "git status", Error: "write failed",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "confirm_rule_intent_result", Payload: payload})
	result := <-ch
	if result.RequestID != "confirm-1" || len(result.Results) != 1 || result.Results[0].Error != "write failed" {
		t.Fatalf("receipt = %+v", result)
	}
}
