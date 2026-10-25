package main

import (
	"context"
	"testing"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func TestQuestionFormInvalidShapeDoesNotConsumeSubmission(t *testing.T) {
	fc := &config.FeishuConfig{AppID: "cli_test", AppSecret: "secret", Owner: &config.OwnerIdentity{AccountID: "cli_test", UserID: "ou_owner"}}
	a := testFeishuAdapter(t, fc)

	event := makeFeishuQuestionFormEvent("ou_owner", "oc_chat1", "q-1", map[string]interface{}{
		"option_1": true, "option_2": map[string]any{"checked": false},
	})
	response, err := a.handleCardActionEvent(context.Background(), event)
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "error" || response.Card != nil {
		t.Fatalf("invalid shape response=%+v err=%v", response, err)
	}
	queue := feishuTestQueue(a, im.IncomingMessage{AppID: fc.AppID, ChatID: "oc_chat1"})
	if len(queue) != 0 {
		t.Fatal("invalid form dispatched a partial answer")
	}
	event.Event.Action.FormValue["option_2"] = false
	response, err = a.handleCardActionEvent(context.Background(), event)
	if err != nil || response == nil || len(queue) != 1 {
		t.Fatalf("valid retry rejected: response=%+v err=%v queued=%d", response, err, len(queue))
	}
}
