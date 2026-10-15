package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTelegramBatchFreezesControlsAcrossAdmissionAndRedelivery(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint(retry), func(t *testing.T) {
			f := newTelegramGatewayFixture(t, 917001)
			p, writer := f.installProcess(t, 123456)
			p.mu.Lock()
			p.state.PendingConfirm = &ConfirmPayload{RequestID: "original"}
			p.mu.Unlock()
			msg := IncomingMessage{IMType: "telegram", AccountID: "917001", ChatID: "123456"}
			id := pauseIngressTestRoute(t, f.service, msg)
			batch := []telegramUpdate{telegramGatewayUpdate(70, 123456, "/allow@GatewayBot"), telegramGatewayUpdate(71, 123456, "/allow@GatewayBot")}
			a, api := f.adapter(t, f.botID, batch...)
			// Redelivery has fresh wire objects, not the adapter's captured fields.
			raw, _ := json.Marshal(batch)
			var redelivery []telegramUpdate
			if err := json.Unmarshal(raw, &redelivery); err != nil {
				t.Fatal(err)
			}
			// Use wire-decoded input for both attempts so canonical fingerprints match.
			api.probe = redelivery
			now := time.Now()
			a.now = func() time.Time { return now }
			firstReceived := now
			receive, first := a.receive, true
			a.receive = func(ctx context.Context, e IngressEnvelope) ReceiveResult {
				if first {
					first = false
					p.mu.Lock()
					p.state.PendingConfirm = &ConfirmPayload{RequestID: "replacement"}
					p.mu.Unlock()
					now = now.Add(time.Second)
					if retry {
						var repeated []telegramUpdate
						if err := json.Unmarshal(raw, &repeated); err != nil {
							t.Error(err)
						}
						api.batches <- repeated
						return ReceiveResult{Disposition: ReceiveRetryable}
					}
				}
				return receive(ctx, e)
			}
			startTelegramGateway(t, a)
			for _, updateID := range []int64{70, 71} {
				rec := telegramGatewayRecord(t, f, 123456, updateID, "received")
				if rec.ControlTarget == nil || rec.ControlTarget.RequestID != "original" || !rec.ReceivedAt.Equal(firstReceived) {
					t.Fatalf("control recaptured after receive: %+v", rec)
				}
			}
			dispatchIngressTestRoute(f.service, id)
			telegramGatewayRecord(t, f, 123456, 71, "rejected")
			if len(writer.snapshot()) != 0 {
				t.Fatal("stale control approved replacement request")
			}
		})
	}
}

func TestTelegramBatchCaptureWaitsForBusySnapshot(t *testing.T) {
	f := newTelegramGatewayFixture(t, 917002)
	p, _ := f.installProcess(t, 123456)
	a, _ := f.adapter(t, f.botID)
	a.accountID, a.username = "917002", "GatewayBot"
	batch := []telegramUpdate{telegramGatewayUpdate(72, 123456, "/allow@GatewayBot")}
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "original"}
	done := make(chan struct{})
	go func() {
		a.captureBatch(batch)
		close(done)
	}()
	select {
	case <-done:
		p.mu.Unlock()
		t.Fatal("capture returned a busy refusal instead of waiting for the snapshot")
	case <-time.After(20 * time.Millisecond):
	}
	p.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("capture did not finish after the snapshot lock was released")
	}
	capture := batch[0].captured.envelope.capture
	if capture == nil || capture.err != nil || capture.target == nil || capture.target.RequestID != "original" {
		t.Fatalf("invalid retained snapshot: %+v", capture)
	}
}

func TestTelegramCommandAliasesRoundTripAndRejectLegacySuffix(t *testing.T) {
	for _, canonical := range []string{"/handoff-deny", "/question-interact", "/question-withdraw", "/question-replace"} {
		for _, kind := range []string{"private", "group"} {
			command := canonical + " @request unchanged/path@value"
			hint := FormatCommandHint(PlatformPresentation{Platform: "telegram", BotUsername: "GatewayBot"}, CommandTarget{ChatKind: kind}, command)
			u := telegramGatewayUpdate(1, 123456, hint)
			text, reason := telegramNormalizeCommand(hint, u.Message.Entities, "GatewayBot")
			if reason != "" || text != command || parseIMCommand(text).Type != parseIMCommand(command).Type {
				t.Fatalf("%s: hint=%q normalized=%q reason=%q", kind, hint, text, reason)
			}
		}
		legacy := canonical + "@GatewayBot argument"
		alias := strings.ReplaceAll(canonical, "-", "_")
		// A prefix-only entity and the full command token are the two shapes
		// Telegram can produce; both must reject the legacy hyphenated form.
		for _, token := range []string{canonical[:strings.IndexByte(canonical, '-')], canonical + "@GatewayBot"} {
			u := telegramGatewayUpdate(1, 123456, legacy)
			u.Message.Entities = []telegramEntity{{Type: "bot_command", Offset: 0, Length: imUTF16Units(token)}}
			if _, reason := telegramNormalizeCommand(legacy, u.Message.Entities, "GatewayBot"); reason != "invalid_command_entity" {
				t.Fatalf("legacy suffix forwarded as model input: token=%q reason=%q", token, reason)
			}
		}
		// Telegram marks the whole command, suffix included, as one entity. The
		// supported alias must still reach the canonical command through it.
		supported := alias + "@GatewayBot argument"
		v := telegramGatewayUpdate(1, 123456, supported)
		v.Message.Entities = []telegramEntity{{Type: "bot_command", Offset: 0, Length: imUTF16Units(alias + "@GatewayBot")}}
		text, reason := telegramNormalizeCommand(supported, v.Message.Entities, "GatewayBot")
		if reason != "" || text != canonical+" argument" || parseIMCommand(text).Type != parseIMCommand(canonical+" argument").Type {
			t.Fatalf("supported alias rejected: text=%q reason=%q", text, reason)
		}
	}
	input := "/question_interact@OtherBot q"
	u := telegramGatewayUpdate(1, 123456, input)
	if _, reason := telegramNormalizeCommand(input, u.Message.Entities, "GatewayBot"); reason != "other_bot_command" {
		t.Fatal(reason)
	}
}

func TestTelegramPartialFailureVisibleInStatusAndMetrics(t *testing.T) {
	f := newTelegramGatewayFixture(t, 917002)
	a, _ := f.adapter(t, f.botID)
	calls := 0
	var err error
	a.sender, err = NewIMTextSender(IMTextSendOptions{Platform: "telegram", Budget: 3500, Measure: imUTF16Units, SendSegment: func(context.Context, string, string) error {
		calls++
		if calls == 2 {
			return &telegramAPIError{Category: "chat_migrated"}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	startTelegramGateway(t, a)
	waitIngressCondition(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.ready })
	if err = a.SendTextContext(t.Context(), "123456", strings.Repeat("x", 3501)); err == nil {
		t.Fatal("expected failure")
	}
	status := f.router.ingressStatus(IncomingMessage{IMType: "telegram", AccountID: "917002", ChatID: "123456"})
	if !strings.Contains(status, "Last outbound failure: chat_migrated after 1 accepted segments.") {
		t.Fatal(status)
	}
	var logs strings.Builder
	logIMMetrics(nil, a, func(format string, args ...any) { fmt.Fprintf(&logs, format, args...) })
	if !strings.Contains(logs.String(), "partial_failures=1") {
		t.Fatal(logs.String())
	}
}

func TestTelegramSensitiveInputsKeepNoDurableDigest(t *testing.T) {
	a := &TelegramAdapter{accountID: "99", username: "GatewayBot", now: realTelegramClock}
	for _, input := range []string{"/bind /workspace pairing-secret", "/answer @q private-answer", "! private-command", "！ private-command"} {
		u := telegramTestUpdate(10)
		u.Message.Text = input
		for _, unsupported := range []bool{false, true} {
			u.Message.IsTopicMessage = unsupported
			e, _ := a.normalizeUpdate(u)
			if e.InputDigest != "" {
				t.Fatal("sensitive input digest persisted", input)
			}
		}
	}
	var unsupported telegramUpdate
	if err := json.Unmarshal([]byte(`{"update_id":11,"edited_message":{"text":"/bind /workspace pairing-secret"}}`), &unsupported); err != nil {
		t.Fatal(err)
	}
	if e, _ := a.normalizeUpdate(unsupported); e.InputDigest != "" {
		t.Fatal("unsupported payload digest persisted")
	}
	u := telegramTestUpdate(12)
	if e, reason := a.normalizeUpdate(u); reason != "" || e.InputDigest != telegramDigest(u.Message.Text) {
		t.Fatal("ordinary text lost conflict digest")
	}
}
