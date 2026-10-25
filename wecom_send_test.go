package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/keakon/chord-gateway/internal/im"

	wecom "github.com/keakon/chord-gateway/internal/platform/wecom"

	"github.com/gorilla/websocket"

	"github.com/keakon/golog"
	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
)

func wecomAdapterFixture(t *testing.T) (*wecom.WecomAdapter, im.IncomingMessage, *IngressStore, *NotificationRouter) {
	t.Helper()
	chat, err := wecom.EncodeWecomTarget("private", "owner")
	if err != nil {
		t.Fatal(err)
	}
	store := testConfigStore(t, fmt.Sprintf("ims: {wecom: {bot_id: bot, secret: placeholder, owner: {account_id: bot, user_id: owner}, chat_bindings: {%q: project}}}\nworkspaces: {project: {path: %q}}\n", chat, t.TempDir()))
	paths := &config.Paths{StateDir: t.TempDir()}
	mgr := NewChordManager(store.Snapshot(), paths)
	mgr.store = store
	r := NewNotificationRouter(mgr)
	r.admission = NewAdmission(NewPairingService(store, false, nil))
	mgr.admission = r.admission
	a, err := wecom.NewWecomAdapter(store.Snapshot(), *store.Snapshot().IMConfigByType("wecom"), paths, r)
	if err != nil {
		t.Fatal(err)
	}
	r.SetAdapter(a)
	ingress := NewIngressStore()
	service := NewIngressService(ingress, r)
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = a.AuthenticatedForTest(a.ContextForTest()); err != nil {
		t.Fatal(err)
	}
	if err = a.TransportReadyForTest(a.ContextForTest()); err != nil {
		t.Fatal(err)
	}
	msg := im.IncomingMessage{IMType: "wecom", AccountID: "bot", ChatID: chat, ChatKind: "private", SenderID: "owner", MessageID: "message:fixture", Text: "task"}
	if _, ok := r.admission.Authorize(msg); !ok {
		t.Fatal("fixture admission failed")
	}
	key := (processKey{workspaceID: "project", imType: "wecom", chatID: chat}).String()
	ready := make(chan struct{})
	close(ready)
	p := &ChordProcess{key: key, runtimeID: "runtime", sessionEpoch: 1, stdin: &ingressTestWriter{}, readySignal: ready, state: ControlState{SessionID: "session"}}
	mgr.mu.Lock()
	mgr.procs[key] = p
	mgr.mu.Unlock()
	if err = mgr.pins.Set(pinKeyForBinding(key), "session"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a.Disconnect()
		service.Close()
		r.admission.Close()
		r.Close()
		p.mu.Lock()
		for _, observer := range p.inputObservers {
			if observer.timer != nil {
				observer.timer.Stop()
			}
		}
		p.mu.Unlock()
		if err := ingress.Close(); err != nil {
			t.Error(err)
		}
	})
	return a, msg, ingress, r
}

func startWecomAdapterTestWS(t *testing.T, a *wecom.WecomAdapter, send func(string, string) int) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var frame wecom.WecomWSFrame
			if conn.ReadJSON(&frame) != nil {
				return
			}
			code := 0
			if frame.Cmd == "aibot_send_msg" {
				var body struct {
					ChatID   string `json:"chatid"`
					MsgType  string `json:"msgtype"`
					Markdown struct {
						Content string `json:"content"`
					} `json:"markdown"`
				}
				if json.Unmarshal(frame.Body, &body) != nil || body.MsgType != "markdown" {
					return
				}
				code = send(body.ChatID, body.Markdown.Content)
				if code == -999 {
					continue
				} // Simulate a send whose acceptance ACK never arrives.
			}
			if conn.WriteJSON(map[string]any{"headers": map[string]string{"req_id": frame.Headers.ReqID}, "errcode": code}) != nil {
				return
			}
		}
	}))
	a.SetWSEndpointForTest("ws" + strings.TrimPrefix(server.URL, "http"))
	a.SetRatePacingForTest(time.Millisecond, time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- a.RunWSForTest(a.ContextForTest()) }()
	t.Cleanup(func() { a.Disconnect(); <-done; server.Close() })
	waitIngressCondition(t, func() bool {
		return a.AdapterReadyForTest() && a.WSTransportReadyForTest()
	})
}

func TestWecomRenderedSegmentsStayWithinByteBudget(t *testing.T) {
	a, _, _, _ := wecomAdapterFixture(t)
	source := strings.Repeat("\\*中文😀<@owner>@all[link](url)\n", 3000)
	segments, err := a.TextSenderForTest().Segments(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 32 {
		t.Fatalf("got %d segments", len(segments))
	}
	for _, part := range segments {
		if len(part) > 1024 || !utf8.ValidString(part) || strings.Contains(part, "<@") {
			t.Fatalf("unsafe segment: len=%d", len(part))
		}
	}
	if !strings.Contains(segments[31], "Output truncated") {
		t.Fatal("missing final truncation notice")
	}
	got := wecom.RenderTextForTest("\\`*_{}[]()#+-.!|~= <@user> @all")
	if !strings.Contains(got, "＜@user＞") || !strings.Contains(got, "@all") || !strings.Contains(got, `\[`) || !strings.HasPrefix(got, "\\\\\\`") {
		t.Fatalf("render=%q", got)
	}
}

func TestWecomBoundSendAndProbeAuthorization(t *testing.T) {
	a, msg, _, _ := wecomAdapterFixture(t)
	var mu sync.Mutex
	var payloads []string
	startWecomAdapterTestWS(t, a, func(target, text string) int {
		mu.Lock()
		defer mu.Unlock()
		payloads = append(payloads, target+":"+text)
		return 0
	})
	if !errors.Is(a.SendTextContext(t.Context(), msg.ChatID, "bypass"), im.ErrPairingDenied) {
		t.Fatal("ordinary context bypassed binding permit")
	}
	if err := a.SendText(msg.ChatID, "hello"); err != nil {
		t.Fatal(err)
	}
	group, _ := wecom.EncodeWecomTarget("group", "candidate")
	if err := a.SendText(group, "unbound"); !errors.Is(err, im.ErrPairingDenied) {
		t.Fatalf("unbound send=%v", err)
	}
	if err := a.ValidateBindingTarget(t.Context(), im.BindingProbeTarget{Platform: "wecom", AccountID: "other", ChatID: group}); !errors.Is(err, im.ErrPairingDenied) {
		t.Fatal("wrong account probe allowed")
	}
	if err := a.ValidateBindingTarget(t.Context(), im.BindingProbeTarget{Platform: "wecom", AccountID: "bot", ChatID: group}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 2 || payloads[0] != "owner:hello" || payloads[1] != "candidate:"+wecom.BindingCheckMessage {
		t.Fatalf("payloads=%q", payloads)
	}
}

func TestWecomSendReportsAcceptedSegmentsAndDoesNotRetry(t *testing.T) {
	a, msg, _, _ := wecomAdapterFixture(t)
	var mu sync.Mutex
	calls := 0
	startWecomAdapterTestWS(t, a, func(_, text string) int {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if len(text) > 1024 {
			t.Errorf("oversized payload=%d", len(text))
		}
		if calls == 2 {
			return 45001
		}
		return 0
	})
	err := a.SendText(msg.ChatID, strings.Repeat("😀", 900))
	var partial *im.IMTextSendError
	var transport *wecom.WecomTransportError
	if !errors.As(err, &partial) || partial.AcceptedSegments != 1 || partial.Category != "platform_rejected" || !errors.As(err, &transport) || transport.Category != "platform_rejected" || transport.Code != 45001 {
		t.Fatalf("send=%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("retried rejected send: %d", calls)
	}
}

func TestWecomSendThrottleAndCancellation(t *testing.T) {
	a, _, _, _ := wecomAdapterFixture(t)
	accountInterval, targetInterval := a.RatePacingForTest()
	if accountInterval != 500*time.Millisecond || targetInterval != 4*time.Second {
		t.Fatal("production throttle mismatch")
	}
	a.SetRatePacingForTest(10*time.Millisecond, 30*time.Millisecond)
	if err := a.WaitSendSlotForTest(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := a.WaitSendSlotForTest(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 8*time.Millisecond {
		t.Fatal("account throttle bypassed")
	}
	start = time.Now()
	if err := a.WaitSendSlotForTest(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 25*time.Millisecond {
		t.Fatal("target throttle bypassed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.WaitSendSlotForTest(ctx, "third"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWecomUnknownSendReportsPartialWithoutReplay(t *testing.T) {
	a, msg, _, _ := wecomAdapterFixture(t)
	a.SetWSSendTimeoutForTest(20 * time.Millisecond)
	var mu sync.Mutex
	calls := 0
	startWecomAdapterTestWS(t, a, func(_, _ string) int {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 2 {
			return -999
		}
		return 0
	})
	err := a.SendText(msg.ChatID, strings.Repeat("x", 2100))
	var partial *im.IMTextSendError
	if !errors.As(err, &partial) || partial.AcceptedSegments != 1 || partial.Category != "unknown_send" {
		t.Fatalf("send=%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("unknown send replayed: %d", calls)
	}
}

func TestWecomLongResultAndRetiredBinding(t *testing.T) {
	a, msg, _, r := wecomAdapterFixture(t)
	var mu sync.Mutex
	var parts []string
	startWecomAdapterTestWS(t, a, func(_, text string) int { mu.Lock(); defer mu.Unlock(); parts = append(parts, text); return 0 })
	if err := a.SendText(msg.ChatID, strings.Repeat("😀*", 16000)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(parts) != 32 || !strings.Contains(parts[len(parts)-1], "Output truncated") {
		t.Fatalf("invalid long result: segments=%d", len(parts))
	}
	mu.Unlock()
	key := (processKey{workspaceID: "project", imType: "wecom", chatID: msg.ChatID}).String()
	target, ok := r.targetForKey(key)
	if !ok {
		t.Fatal("missing binding")
	}
	ctx, done, err := r.deliveryContext(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	target.Lease.Close()
	if err := a.SendTextContext(ctx, msg.ChatID, "stale output"); !errors.Is(err, im.ErrPairingDenied) {
		t.Fatalf("retired permit allowed: %v", err)
	}
}

func TestWecomProbeSharesTargetACKOrdering(t *testing.T) {
	a, msg, _, _ := wecomAdapterFixture(t)
	entered := make(chan struct{})
	releaseACK := make(chan struct{})
	var releaseOnce sync.Once
	startWecomAdapterTestWS(t, a, func(_, text string) int {
		if text == "body" {
			close(entered)
			<-releaseACK
		}
		return 0
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseACK) }) })
	bodyDone := make(chan error, 1)
	go func() { bodyDone <- a.SendText(msg.ChatID, "body") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("body not sent")
	}
	probeDone := make(chan error, 1)
	go func() {
		probeDone <- a.ValidateBindingTarget(t.Context(), im.BindingProbeTarget{Platform: "wecom", AccountID: "bot", ChatID: msg.ChatID})
	}()
	// While the body ACK is held, probe must wait for the same target's slot.
	time.Sleep(20 * time.Millisecond)
	pending := a.WSPendingSendsForTest()
	if pending != 1 {
		t.Errorf("probe sent ahead of body ACK: pending=%d", pending)
	}
	releaseOnce.Do(func() { close(releaseACK) })
	for _, done := range []<-chan error{bodyDone, probeDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("ordered send did not finish")
		}
	}
}

func TestWecomRejectedCallbackIsCountedAndLoggedOnce(t *testing.T) {
	a, _, store, _ := wecomAdapterFixture(t)
	var logged strings.Builder
	log.SetLogfFunc(func(msg string, args ...interface{}) {
		fmt.Fprintf(&logged, msg, args...)
		logged.WriteByte('\n')
	}, golog.WarnLevel)
	t.Cleanup(func() { log.SetDefaultLogger(nil) })
	// Every rejection is counted, but the warning is bounded to one line per
	// interval so a malformed flood cannot overwhelm the log.
	a.CallbackRejectedForTest("invalid_callback")
	a.CallbackRejectedForTest("source_time_out_of_range")
	counts := map[string]uint64{}
	for _, counter := range store.MetricsSnapshot().Counters {
		if counter.Platform == "wecom" && counter.Event == "callback_dropped" {
			counts[counter.Reason] = counter.Count
		}
	}
	if counts["invalid_callback"] != 1 || counts["source_time_out_of_range"] != 1 {
		t.Fatalf("callback rejection metrics: %v", counts)
	}
	if got := strings.Count(logged.String(), "wecom: rejected an invalid text callback"); got != 1 {
		t.Fatalf("rejection warnings are not bounded: %d (%q)", got, logged.String())
	}
	if !strings.Contains(logged.String(), "reason=invalid_callback") {
		t.Fatalf("rejection warning lost its reason: %q", logged.String())
	}
}

func TestWecomUnavailableIsLoggedUnlessStopping(t *testing.T) {
	a, _, _, _ := wecomAdapterFixture(t)
	var logged strings.Builder
	log.SetLogfFunc(func(msg string, args ...interface{}) {
		fmt.Fprintf(&logged, msg, args...)
		logged.WriteByte('\n')
	}, golog.WarnLevel)
	t.Cleanup(func() { log.SetDefaultLogger(nil) })
	a.UnavailableForTest(&wecom.WecomTransportError{Category: "connection_unavailable"})
	if !strings.Contains(logged.String(), "connection unavailable category=connection_unavailable") {
		t.Fatalf("transport failure was silent: %q", logged.String())
	}
	logged.Reset()
	a.SetStoppingForTest(true)
	a.UnavailableForTest(errors.New("dial failed"))
	if logged.Len() != 0 {
		t.Fatalf("shutdown reported as failure: %q", logged.String())
	}
}

func TestWecomQuestionHintRoundTrip(t *testing.T) {
	router := &NotificationRouter{}
	for _, multiple := range []bool{false, true} {
		message := router.formatQuestionNotification(ControlState{Questions: map[string]*QuestionPayload{"q1": {RequestID: "q1", Version: 1, Question: "Choose", Options: []string{"First", "Second"}, Multiple: multiple}}})
		rendered := wecom.RenderTextForTest(message)
		_, hint, ok := strings.Cut(rendered, "Reply ")
		if !ok {
			t.Fatalf("missing hint: %q", rendered)
		}
		cmd := parseIMCommand(hint)
		want := "1"
		if multiple {
			want = "1,2"
		}
		if cmd.RequestID != "q1" || len(cmd.Answers) != 1 || cmd.Answers[0] != want {
			t.Fatalf("hint=%q command=%+v", hint, cmd)
		}
	}
}
