package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestDiscordBodyCommandsRemainDurableTasksAfterNormalization(t *testing.T) {
	for _, text := range []string{
		" /bind /project <@123> \n",
		"!echo sample <@123>",
		"！echo sample <@123>",
		"/allow <@123>",
		"/answer @question value <@123>",
		"/cancel <@123>",
		"/help <@123>",
	} {
		t.Run(text, func(t *testing.T) {
			s, _, w, msg := ingressTestService(t)
			pauseIngressTestRoute(t, s, msg)
			msg.MessageID, msg.Text, msg.ForceText = "body-command", text, true
			if command := commandForIncomingMessage(msg); command.Type != "send" || command.Content != text {
				t.Fatal(command)
			}
			if ticket, ok := s.router.admission.Authorize(msg); !ok || ticket.Kind != "paired_task" {
				t.Fatalf("body command admitted as %q, allowed=%t", ticket.Kind, ok)
			}
			future, err := s.router.SubmitIngressNormalization(t.Context(), msg.ChatID, im.IngressEnvelope{Message: msg}, func(_ context.Context, e im.IngressEnvelope) (im.IngressEnvelope, error) {
				// Lookup is allowed to classify a channel, never replace the
				// operation or turn a body mention into an invocation prefix.
				e.Message.Text, e.Message.ForceText = "/allow", false
				e.Message.InternalAction = &im.InternalAction{Type: "confirm", Action: "allow", RequestID: "replacement"}
				return e, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			result := im.WaitIngressNormalization(t.Context(), future)
			if result.Disposition != im.ReceiveAccepted {
				t.Fatal(result)
			}
			record, err := s.store.Get(result.RecordID)
			if err != nil || record.Kind != "task" || record.Text != text || record.ControlTarget != nil || record.State != "received" {
				t.Fatal(record, err)
			}
			if len(w.snapshot()) != 0 {
				t.Fatal("normalization bypassed the paused route")
			}
		})
	}
}

func TestDiscordBodyCommandRecoverySendsOriginalText(t *testing.T) {
	s, _, w, msg := ingressTestService(t)
	pauseIngressTestRoute(t, s, msg)
	const text = " \t!echo sample <@123>\n"
	msg.MessageID, msg.Text, msg.ForceText = "recover-body-command", text, true
	result := s.router.ReceiveIngress(t.Context(), im.IngressEnvelope{Message: msg})
	if result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	s.Close()
	recovered := NewIngressService(s.store, s.router)
	if err := recovered.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.PrepareAccount(t.Context(), msg.IMType, msg.AccountID); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool { return len(w.snapshot()) == 1 })
	commands := w.snapshot()
	if commands[0]["type"] != "send" || commands[0]["content"] != text || commands[0]["request_id"] != "im-"+result.RecordID {
		t.Fatal("recovery reinterpreted the persisted task", commands)
	}
}

func TestDiscordForcedHelpDoesNotBecomeUrgentOrBypassUnknownOutcome(t *testing.T) {
	s, p, w, msg := ingressTestService(t)
	id := pauseIngressTestRoute(t, s, msg)
	msg.MessageID, msg.Text = "unknown-task", "original task"
	unknown := s.Accept(t.Context(), im.IngressEnvelope{Message: msg})
	if unknown.Disposition != im.ReceiveAccepted {
		t.Fatal(unknown)
	}
	if err := s.store.UpdateRecord(t.Context(), unknown.RecordID, []im.IngressState{"received"}, func(r *IngressRecord) error {
		r.State = "unknown"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{ToolName: "Done", RequestID: "done-request"}
	p.mu.Unlock()
	msg.MessageID, msg.Text, msg.ForceText = "forced-help", "/help", true
	forced := s.Accept(t.Context(), im.IngressEnvelope{Message: msg})
	if forced.Disposition != im.ReceiveAccepted {
		t.Fatal(forced)
	}
	record, err := s.store.Get(forced.RecordID)
	if err != nil || record.Kind != "control" || record.ControlTarget.RequestID != "done-request" {
		t.Fatal(record, err)
	}
	// Ordinary Done feedback cannot jump ahead of earlier side effects merely
	// because its text happens to spell /help.
	older := IngressRecord{RecordID: "earlier-side-effect", Kind: "control", State: "received"}
	ordered := s.prioritizeControls([]IngressRecord{older, record})
	if ordered[0].RecordID != older.RecordID {
		t.Fatal("forced /help overtook an earlier side effect")
	}
	dispatchIngressTestRoute(s, id)
	after, err := s.store.Get(forced.RecordID)
	if err != nil || after.State != "rejected" || after.Reason != "route_outcome_unknown" || len(w.snapshot()) != 0 {
		t.Fatal(after, err, w.snapshot())
	}
}

func TestDiscordLeadingInvocationRetainsCommandSemantics(t *testing.T) {
	for _, text := range []string{"/bind /project", "!echo sample", "/cancel", "/allow", "/answer @question value"} {
		msg := im.IncomingMessage{Text: text}
		if command := commandForIncomingMessage(msg); command.Type == "send" {
			t.Fatalf("invoked command %q became ordinary text: %s", text, fmt.Sprint(command))
		}
	}
}
