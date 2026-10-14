package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/slack-go/slack"
)

func slackTestInbound() *slackInbound {
	return newSlackInbound(nil, slackIdentity{TeamID: "T1", AppID: "A1", BotUserID: "UBOT", BotID: "B1", AccountID: "account"}, nil)
}

func slackTestEvent(event slackInboundMessage, eventID string) json.RawMessage {
	payload, _ := json.Marshal(slackEventsPayload{Type: "event_callback", TeamID: "T1", AppID: "A1", EventID: eventID, Event: event})
	return payload
}

func slackTestSlash(text, trigger string) json.RawMessage {
	payload, _ := json.Marshal(slackSlashPayload{TeamID: "T1", AppID: "A1", ChannelID: "D1", UserID: "U1", Command: "/chord", Text: text, TriggerID: trigger})
	return payload
}

func TestSlackInboundSemanticAndDeliveryIdentity(t *testing.T) {
	s := slackTestInbound()
	event := slackInboundMessage{Type: "app_mention", Channel: "C1", User: "U1", Text: "<@UBOT> task <@UBOT>", TS: "1760000000.000001", EventTS: "1760000000.000002", ThreadTS: "1760000000.000000"}
	first, _, _, err := s.decode("events_api", slackTestEvent(event, "E1"), "ENV1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	second, _, _, err := s.decode("events_api", slackTestEvent(event, "E2"), "ENV2", time.Now())
	if err != nil || first.SemanticID != second.SemanticID || first.Message.MessageID == second.Message.MessageID || first.DeliveryID == second.DeliveryID {
		t.Fatal("business/semantic/delivery scopes are incorrect", err)
	}
	if first.Message.Text != "task <@UBOT>" || first.SourceTime.Nanosecond() != 2000 {
		t.Fatal(first)
	}
	_, thread, err := decodeSlackTarget(first.Message.ChatID)
	if err != nil || thread != event.ThreadTS {
		t.Fatal("thread timestamp changed", err)
	}
	event.TS = "1760000000.000003"
	third, _, _, _ := s.decode("events_api", slackTestEvent(event, "E3"), "ENV3", time.Now())
	if third.SemanticID == first.SemanticID {
		t.Fatal("different messages in one thread were deduplicated")
	}
}

func TestSlackInboundRejectsUnsupportedEventsAndScope(t *testing.T) {
	base := slackInboundMessage{Type: "message", Channel: "D1", ChannelType: "im", User: "U1", Text: "task", TS: "1760000000.1"}
	for _, test := range []struct {
		name string
		edit func(*slackInboundMessage)
	}{
		{"bot", func(e *slackInboundMessage) { e.BotID = "B2" }},
		{"self", func(e *slackInboundMessage) { e.User = "UBOT" }},
		{"edited", func(e *slackInboundMessage) { e.Subtype = "message_changed" }},
		{"deleted", func(e *slackInboundMessage) { e.Subtype = "message_deleted" }},
		{"external user", func(e *slackInboundMessage) { e.UserTeam = "T2" }},
		{"external event", func(e *slackInboundMessage) { e.Team = "T2" }},
		{"channel message", func(e *slackInboundMessage) { e.Channel = "C1"; e.ChannelType = "channel" }},
		{"thread without mention", func(e *slackInboundMessage) { e.ThreadTS = "1760000000.0" }},
		{"mention of other bot", func(e *slackInboundMessage) { e.Type = "app_mention"; e.Channel = "C1"; e.Text = "<@UOTHER> task" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := base
			test.edit(&event)
			if _, _, _, err := slackTestInbound().decode("events_api", slackTestEvent(event, "E1"), "ENV1", time.Now()); !errors.Is(err, errSlackIgnored) {
				t.Fatal(err)
			}
		})
	}
	for _, field := range []string{"team_id", "api_app_id", "event_id"} {
		var payload map[string]any
		_ = json.Unmarshal(slackTestEvent(base, "E1"), &payload)
		payload[field] = ""
		data, _ := json.Marshal(payload)
		if _, _, _, err := slackTestInbound().decode("events_api", data, "ENV1", time.Now()); !errors.Is(err, errSlackIgnored) {
			t.Fatalf("missing %s accepted", field)
		}
	}
}

func TestSlackSlashRootTargetAndArguments(t *testing.T) {
	s := slackTestInbound()
	received := time.Now()
	envelope, _, force, err := s.decode("slash_commands", slackTestSlash(` bind "/project folder" code `, "TR1"), "ENV1", received)
	if err != nil || envelope.Message.Text != `/bind "/project folder" code` || envelope.Message.ChatID != "D1" || envelope.Message.MessageID != "slash:TR1" || envelope.SemanticID != "" || !envelope.SourceTime.Equal(received) || !force {
		t.Fatal(envelope, force, err)
	}
	if text := slackSlashText("  "); text != "/help" {
		t.Fatal(text)
	}
	if _, _, _, err := s.decode("slash_commands", slackTestSlash("status", ""), "ENV2", received); !errors.Is(err, errSlackIgnored) {
		t.Fatal("missing trigger accepted", err)
	}
	if got := stripSlackMentionPrefix("before <@UBOT> after", "UBOT"); got != "before <@UBOT> after" {
		t.Fatal("non-prefix mention removed", got)
	}
}

func TestSlackInboundRejectsEnterpriseInstallations(t *testing.T) {
	for _, value := range []string{`true`, `"true"`} {
		var slash slackSlashPayload
		_ = json.Unmarshal(slackTestSlash("status", "TR1"), &slash)
		slash.IsEnterpriseInstall = json.RawMessage(value)
		payload, _ := json.Marshal(slash)
		if _, _, _, err := slackTestInbound().decode("slash_commands", payload, "ENV1", time.Now()); err != errSlackIgnored {
			t.Fatal("enterprise installation slash accepted", err)
		}
	}
	for _, value := range []string{`false`, `"false"`} {
		if !slackWorkspaceInstallation(json.RawMessage(value)) {
			t.Fatal("workspace installation rejected")
		}
	}
	event := slackInboundMessage{Type: "message", Channel: "D1", ChannelType: "im", User: "U1", Text: "task", TS: "1760000000.1"}
	var payload map[string]any
	_ = json.Unmarshal(slackTestEvent(event, "E1"), &payload)
	payload["authorizations"] = []map[string]any{{"is_enterprise_install": true}}
	data, _ := json.Marshal(payload)
	if _, _, _, err := slackTestInbound().decode("events_api", data, "ENV1", time.Now()); err != errSlackIgnored {
		t.Fatal("enterprise installation event accepted", err)
	}
}

type slackCaptureReceiver struct{ envelopes chan IngressEnvelope }

func (s slackCaptureReceiver) Accept(_ context.Context, envelope IngressEnvelope) ReceiveResult {
	s.envelopes <- envelope
	return ReceiveResult{Disposition: ReceiveAccepted}
}

func TestSlackInboundConversationClassificationAndCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"id": "D1", "is_im": true, "user": "U1"}})
	}))
	defer server.Close()
	s := slackTestInbound()
	s.client = slack.New("test-token", slack.OptionAPIURL(server.URL+"/"))
	channel, err := s.conversation(t.Context(), "D1", false)
	if err != nil || slackConversationKind(channel, "D1", "U1") != "private" {
		t.Fatal(channel, err)
	}
	if _, err = s.conversation(t.Context(), "D1", false); err != nil || calls.Load() != 1 {
		t.Fatal("cache miss", err, calls.Load())
	}
	if _, err = s.conversation(t.Context(), "D1", true); err != nil || calls.Load() != 2 {
		t.Fatal("binding did not force query", err, calls.Load())
	}
	if slackConversationKind(channel, "D1", "UOTHER") != "unsupported" {
		t.Fatal("DM sender was not checked")
	}
	channel.IsShared = true
	if slackConversationKind(channel, "D1", "U1") != "unsupported" {
		t.Fatal("shared conversation accepted")
	}
	channel.IsShared, channel.IsMpIM = false, true
	if slackConversationKind(channel, "D1", "U1") != "unsupported" {
		t.Fatal("MPIM accepted")
	}
}

func slackConversationCacheFixture(t *testing.T) *slackInbound {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"id": r.Form.Get("channel"), "is_im": true, "user": "U1"}})
	}))
	t.Cleanup(server.Close)
	s := slackTestInbound()
	s.client = slack.New("test-token", slack.OptionAPIURL(server.URL+"/"))
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache["DEXPIRED"] = slackCachedConversation{expires: now.Add(-time.Minute)}
	s.cache["DOLDEST"] = slackCachedConversation{expires: now.Add(time.Minute)}
	for i := 0; i < slackConversationCacheMax-1; i++ {
		s.cache["D"+strconv.Itoa(i)] = slackCachedConversation{expires: now.Add(time.Hour)}
	}
	return s
}

func TestSlackConversationCachePurgesExpiredAndEvictsEarliestExpiry(t *testing.T) {
	s := slackConversationCacheFixture(t)
	if _, err := s.conversation(t.Context(), "DNEW", false); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) != slackConversationCacheMax {
		t.Fatalf("cache size=%d, want %d", len(s.cache), slackConversationCacheMax)
	}
	if _, ok := s.cache["DEXPIRED"]; ok {
		t.Fatal("expired entry survived past the cache bound")
	}
	if _, ok := s.cache["DOLDEST"]; ok {
		t.Fatal("earliest-expiry entry was not evicted")
	}
	if _, ok := s.cache["DNEW"]; !ok {
		t.Fatal("new conversation was not cached")
	}
}

func TestSlackInboundAsyncOrderingKindAndFreeze(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"id": "D1", "is_im": true, "user": "U1"}})
	}))
	defer server.Close()
	receiver := slackCaptureReceiver{make(chan IngressEnvelope, 4)}
	n := NewIngressNormalizer(t.Context(), receiver)
	defer n.Close()
	router := &NotificationRouter{ingress: &IngressService{started: true, normalizer: n}}
	s := slackTestInbound()
	s.client, s.router = slack.New("test-token", slack.OptionAPIURL(server.URL+"/")), router
	first, cancelFirst, err := s.submit(t.Context(), "slash_commands", slackTestSlash("allow", "TR1"), "ENV1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer cancelFirst()
	<-started
	event := slackInboundMessage{Type: "message", Channel: "D1", ChannelType: "im", User: "U1", Text: "<@UBOT> task", TS: "1760000000.1", ThreadTS: "1760000000.0"}
	second, cancelSecond, err := s.submit(t.Context(), "events_api", slackTestEvent(event, "E2"), "ENV2", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer cancelSecond()
	select {
	case <-receiver.envelopes:
		t.Fatal("lookup was bypassed")
	default:
	}
	close(release)
	if WaitIngressNormalization(t.Context(), first).Disposition != ReceiveAccepted || WaitIngressNormalization(t.Context(), second).Disposition != ReceiveAccepted {
		t.Fatal("normalization failed")
	}
	a, b := <-receiver.envelopes, <-receiver.envelopes
	if a.Message.ChatKind != "private" || b.Message.ChatKind != "group" || a.capture == nil || a.capture.kind != "control" || b.capture == nil || b.capture.kind != "task" {
		t.Fatal(a, b)
	}
}

func TestSlackNormalizationDeadlineIncludesQueueWait(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()
	receiver := slackCaptureReceiver{make(chan IngressEnvelope, 1)}
	n := NewIngressNormalizer(t.Context(), receiver)
	defer n.Close()
	s := slackTestInbound()
	s.router = &NotificationRouter{ingress: &IngressService{started: true, normalizer: n}}
	s.client = slack.New("test-token", slack.OptionAPIURL(server.URL+"/"))
	_, _, err := s.submit(t.Context(), "slash_commands", slackTestSlash("status", "TR1"), "ENV1", time.Now().Add(-31*time.Second))
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
		t.Fatal("expired slash started normalization", err, calls.Load())
	}
}

func TestSlackInboundSharedConversationProducesAdmissionRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"id": "C1", "is_channel": true, "is_shared": true}})
	}))
	defer server.Close()
	receiver := slackCaptureReceiver{make(chan IngressEnvelope, 1)}
	n := NewIngressNormalizer(t.Context(), receiver)
	defer n.Close()
	s := slackTestInbound()
	s.router = &NotificationRouter{ingress: &IngressService{started: true, normalizer: n}}
	s.client = slack.New("test-token", slack.OptionAPIURL(server.URL+"/"))
	event := slackInboundMessage{Type: "app_mention", Channel: "C1", User: "U1", Text: "<@UBOT> task", TS: "1760000000.1", ThreadTS: "1760000000.0"}
	future, cancel, err := s.submit(t.Context(), "events_api", slackTestEvent(event, "E1"), "ENV1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if result := WaitIngressNormalization(t.Context(), future); result.Disposition != ReceiveAccepted {
		t.Fatal(result)
	}
	envelope := <-receiver.envelopes
	if envelope.Message.ChatKind != "unsupported" || !strings.HasPrefix(envelope.Message.ChatID, "target-v1:") {
		t.Fatal("shared conversation did not reach rejecting admission kind", envelope)
	}
}

func TestSlackNormalizationOutlivesACKWait(t *testing.T) {
	for _, stop := range []string{"complete", "disconnect", "deadline"} {
		t.Run(stop, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				receiver := slackCaptureReceiver{make(chan IngressEnvelope, 1)}
				n := NewIngressNormalizer(ctx, receiver)
				defer n.Close()
				s := slackTestInbound()
				s.router = &NotificationRouter{ingress: &IngressService{started: true, normalizer: n}}
				entered := make(chan struct{})
				s.client = slack.New("test-token", slack.OptionHTTPClient(&http.Client{Transport: imageRoundTripper(func(r *http.Request) (*http.Response, error) {
					close(entered)
					if stop != "complete" {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					select {
					case <-time.After(3 * time.Second):
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"channel":{"id":"D1","is_im":true,"user":"U1"}}`))}, nil
				})}))
				start := time.Now()
				future, cleanup, err := s.submit(ctx, "slash_commands", slackTestSlash("status", "slow-command"), "slow-envelope", start)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				<-entered
				acks := make(chan slackACK, 1)
				(&SlackAdapter{}).slackReceiptACK(ctx, 1, slackReceipt{Frame: slackSocketFrame{Type: "slash_commands", EnvelopeID: "slow-envelope"}, ReceivedAt: start, Future: future}, acks)
				if time.Since(start) != 2*time.Second || len(acks) != 0 {
					t.Fatal("ACK deadline changed or unconfirmed work acknowledged")
				}
				if stop == "disconnect" {
					cancel()
				}
				result := <-future
				if stop == "complete" {
					if result.Disposition != ReceiveAccepted || len(receiver.envelopes) != 1 {
						t.Fatal("ACK wait cancelled admission", result)
					}
				} else if result.Disposition == ReceiveAccepted || len(receiver.envelopes) != 0 {
					t.Fatal("cancelled lookup was accepted", result)
				}
				if stop == "deadline" && time.Since(start) != 30*time.Second {
					t.Fatal("normalization was not bounded", time.Since(start))
				}
			})
		})
	}
}
