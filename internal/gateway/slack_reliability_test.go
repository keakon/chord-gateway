package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	slackapi "github.com/slack-go/slack"

	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/slack"
)

func TestSlackInboundDecodesOnlyEventsTextAfterMentionCheck(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{`<@UBOT> /answer @question A &amp; B &lt; C`, `/answer @question A & B < C`},
		{`<@UBOT> !printf 'A &amp; B' &gt; output`, `!printf 'A & B' > output`},
		{`<@UBOT> /bind "/project &amp; samples"`, `/bind "/project & samples"`},
		{`<@UBOT> &amp;lt;literal&amp;gt; &quot; <@UOTHER>`, `&lt;literal&gt; &quot; <@UOTHER>`},
	} {
		event := slackIntegrationEvent("C1", "", tc.text, "000001")
		e, _, _, err := slackTestInbound().DecodeForTest("events_api", slackTestEvent(event, "EVENT"), "ENV", time.Now())
		if err != nil || e.Message.Text != tc.want {
			t.Fatalf("got %q, %v; want %q", e.Message.Text, err, tc.want)
		}
	}
	event := slackIntegrationEvent("C1", "", `&lt;@UBOT&gt; /allow`, "000001")
	if _, _, _, err := slackTestInbound().DecodeForTest("events_api", slackTestEvent(event, "EVENT"), "ENV", time.Now()); !errors.Is(err, slack.ErrIgnored) {
		t.Fatal("escaped text became a real invocation", err)
	}
	slash, _, _, err := slackTestInbound().DecodeForTest("slash_commands", slackTestSlash(`answer @question &amp;lt; &lt; &gt; &amp;`, "TRIGGER"), "ENV", time.Now())
	if err != nil || slash.Message.Text != `/answer @question &amp;lt; &lt; &gt; &amp;` {
		t.Fatal("unescaped slash parameters changed", slash.Message.Text, err)
	}
}

func TestSlackCaptureBusyNeverQueuesOrRecaptures(t *testing.T) {
	for _, name := range []string{"process", "manager", "transfer", "pin"} {
		t.Run(name, func(t *testing.T) {
			h := newSlackIntegrationHarness(t)
			h.bindDM(t)
			process, _ := h.attachProcess(t, "D1")
			text := "allow"
			var unlock func()
			switch name {
			case "process":
				process.mu.Lock()
				unlock = process.mu.Unlock
			case "manager":
				h.router.mgr.mu.Lock()
				unlock = h.router.mgr.mu.Unlock
			case "transfer":
				text = "takeover status"
				h.router.mgr.transfers.mu.Lock()
				unlock = h.router.mgr.transfers.mu.Unlock
			case "pin":
				h.router.mgr.mu.Lock()
				h.router.mgr.procs = make(map[string]*ChordProcess)
				h.router.mgr.mu.Unlock()
				h.router.mgr.pins.mu.Lock()
				unlock = h.router.mgr.pins.mu.Unlock
			}
			defer unlock()
			done := make(chan error, 1)
			go func() {
				future, cancel, err := h.inbound.SubmitForTest(t.Context(), "slash_commands", slackTestSlash(text, "BUSY"), "ENV", time.Now())
				if cancel != nil {
					cancel()
				}
				if future != nil {
					done <- errors.New("busy snapshot was queued")
					return
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, slack.ErrCaptureBusy) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("reader waited on snapshot lock")
			}
		})
	}
}

func TestSlackSocketBusyCapturePreservesPingAndShutdown(t *testing.T) {
	h := newSlackIntegrationHarness(t)
	h.bindDM(t)
	process, _ := h.attachProcess(t, "D1")
	cfg := h.router.getConfig()
	a, err := slack.NewSlackAdapter(cfg, cfg.IMs[0], testPaths(t), h.router)
	if err != nil {
		t.Fatal(err)
	}
	identity := h.inbound.IdentityForTest()
	a.SetIdentityForTest(identity, true)
	a.SetVerifyIdentityForTest(func(context.Context) (slack.Identity, error) { return identity, nil })
	t.Cleanup(a.CancelForTest)
	conn, done, cancel := startSlackSocketTest(t, a, identity)
	if err := conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": identity.AppID}}); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool { return a.ReadyForTest() })
	process.mu.Lock()
	defer process.mu.Unlock()
	event := slackIntegrationEvent("D1", "", "sample task", "000001")
	if err := conn.WriteJSON(slack.SocketFrame{Type: "events_api", EnvelopeID: "BUSY", Payload: slackTestEvent(event, "BUSY")}); err != nil {
		t.Fatal(err)
	}
	pong := make(chan string, 1)
	conn.SetPongHandler(func(s string) error { pong <- s; return nil })
	if err := conn.WriteControl(websocket.PingMessage, []byte("busy-capture"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]string{"type": "interactive", "envelope_id": "MARKER"}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if json.Unmarshal(data, &ack) != nil || ack["envelope_id"] != "MARKER" {
		t.Fatal("busy input was acknowledged", string(data))
	}
	select {
	case got := <-pong:
		if got != "busy-capture" {
			t.Fatal(got)
		}
	default:
		t.Fatal("snapshot lock blocked ping")
	}
	cancel()
	_ = waitSlackSocketDone(t, done)
}

func TestSlackDuplicateACKReportsOriginalTerminalState(t *testing.T) {
	for _, tc := range []struct {
		state        im.IngressState
		reason, want string
	}{
		{"expired", "acceptance_outcome_uncertain", "Expired"},
		{"expired", "execution_outcome_unknown", "Outcome unknown"},
		{"unknown", "", "Outcome unconfirmed"},
		{"quarantined", "", "Quarantined"},
	} {
		t.Run(string(tc.state)+tc.reason, func(t *testing.T) {
			future := make(chan im.ReceiveResult, 1)
			future <- im.ReceiveResult{Disposition: im.ReceiveDuplicate, State: tc.state, Reason: tc.reason}
			queue := make(chan slack.ACK, 1)
			done := make(chan struct{})
			receipt := slack.Receipt{Frame: slack.SocketFrame{Type: "slash_commands", EnvelopeID: "DUPLICATE", AcceptsResponsePayload: true}, ReceivedAt: time.Now(), Future: future}
			go func() { defer close(done); (&slack.SlackAdapter{}).ReceiptACKForTest(t.Context(), 1, receipt, queue) }()
			ack := <-queue
			ack.Done <- nil
			<-done
			if !strings.Contains(string(ack.Data), tc.want) || strings.Contains(string(ack.Data), "Accepted") {
				t.Fatal(string(ack.Data))
			}
		})
	}
}

func TestSlackMissingScopeIsFatalAndDiagnosable(t *testing.T) {
	client := testSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth.test" {
			_, _ = w.Write([]byte(`{"ok":true,"team_id":"T1","user_id":"UBOT","bot_id":"B1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope","needed":"users:read"}`))
	})
	_, err := slack.VerifyIdentity(t.Context(), client, "T1", "A1")
	if !slack.ConnectionFatal(err) || slack.ConnectionCategory(err) != "missing_scope" {
		t.Fatal("permanent configuration failure lost category", err)
	}
	if slack.ConnectionCategory(slack.APIError("auth.test", errors.New("sensitive response"))) != "api_unavailable" {
		t.Fatal("transient API failure lost category")
	}
}

func TestSlackRenamedBotKeepsInstallationAndReadableHints(t *testing.T) {
	a, id := slackSocketTestAdapter(t)
	a.SetVerifyIdentityForTest(func(context.Context) (slack.Identity, error) {
		updated := id
		updated.BotName = "RenamedBot"
		return updated, nil
	})
	conn, done, cancel := startSlackSocketTest(t, a, id)
	_ = conn.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": id.AppID}})
	waitIngressCondition(t, func() bool { return a.ReadyForTest() })
	hint := a.HostForTest().(*NotificationRouter).commandHint("slack", im.CommandTarget{Thread: true}, "/allow")
	if !strings.Contains(hint, "@RenamedBot /allow") || !strings.Contains(hint, "@ menu") || strings.Contains(hint, id.BotUserID) {
		t.Fatal(hint)
	}
	client := testSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		text := r.Form.Get("text")
		if !strings.Contains(text, "@RenamedBot /allow") || strings.Contains(text, "&lt;@") {
			t.Error("transport made generated instructions unreadable", text)
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1234567890.000001"}`))
	})
	target, _ := slack.Target("C1", "1234567890.000000")
	if err := testSlackSender(t, client).Send(t.Context(), nil, target, hint); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = waitSlackSocketDone(t, done)
	// Errors retain their safe category through callers' wrapping.
	if slack.ConnectionCategory(slack.APIError("bots.info", slackapi.SlackErrorResponse{Err: "missing_scope"})) != "missing_scope" {
		t.Fatal("scope diagnostic lost")
	}
}
