package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func newWechatIngressTest(t *testing.T) (*WechatAdapter, *IngressService, *ChordProcess, *ingressTestWriter) {
	t.Helper()
	s, p, writer, original := ingressTestService(t)
	r := s.router
	bind := original
	bind.ChatID, bind.MessageID, bind.Text = "owner", "owner-bind", fmt.Sprintf("/bind %q", t.TempDir())
	if !dispatchPairingTest(r, bind) {
		t.Fatal("owner chat binding rejected")
	}
	workspace, err := r.getConfig().ResolveWorkspace("wechat", "owner")
	if err != nil {
		t.Fatal(err)
	}
	key := (processKey{workspaceID: workspace.ID, imType: "wechat", chatID: "owner"}).String()
	r.mgr.mu.Lock()
	delete(r.mgr.procs, p.key)
	p.key = key
	r.mgr.procs[key] = p
	r.mgr.mu.Unlock()
	if err := r.mgr.pins.Set(pinKeyForBinding(key), "session"); err != nil {
		t.Fatal(err)
	}
	a := newTestWechatAdapter(t)
	a.msgRouter = r
	a.token.Store(&TokenData{AccountID: "bot", Token: "sample-token", BaseURL: "https://example.invalid"})
	a.imCfg = *r.getConfig().IMConfigByType("wechat")
	a.httpClient.Transport = imageRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ret":0}`)), Header: make(http.Header)}, nil
	})
	p.stdin = &wechatIngressStatusWriter{process: p, writer: writer}
	r.SetAdapter(a)
	return a, s, p, writer
}

// ChordProcess serializes writes under p.mu, so the synthetic status response
// can notify the registered waiter synchronously without another goroutine.
type wechatIngressStatusWriter struct {
	process *ChordProcess
	writer  *ingressTestWriter
}

func (w *wechatIngressStatusWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	if err != nil {
		return n, err
	}
	var command map[string]any
	if json.Unmarshal(data, &command) == nil && command["type"] == "status" {
		w.process.statusWaiters.notify(w.process.state)
	}
	return n, nil
}
func (w *wechatIngressStatusWriter) Close() error { return nil }

func wechatTextMessage(id, sender, text, reply string) ilinkMessage {
	return ilinkMessage{MessageID: json.Number(id), MessageType: ilinkMessageTypeUser, FromUserID: sender, ContextToken: reply, ItemList: []ilinkItem{{Type: ilinkItemTypeText, TextItem: &ilinkTextItem{Text: text}}}}
}

func wechatTaskWrites(writer *ingressTestWriter) []map[string]any {
	var tasks []map[string]any
	for _, command := range writer.snapshot() {
		if command["type"] == "send" {
			tasks = append(tasks, command)
		}
	}
	return tasks
}

func TestWechatIngressReplyContextWaitingAndFreshStatusWakesFIFO(t *testing.T) {
	a, s, p, writer := newWechatIngressTest(t)
	var accepted []im.ReceiveResult
	for i, text := range []string{"first task", "second task"} {
		result := a.receiveIncomingMessage(t.Context(), wechatTextMessage(fmt.Sprint(i+1), "owner", text, ""), a.token.Load())
		if result.Disposition != im.ReceiveAccepted {
			t.Fatal(result)
		}
		accepted = append(accepted, result)
	}
	for _, result := range accepted {
		record, err := s.store.Get(result.RecordID)
		if err != nil {
			t.Fatal(err)
		}
		s.dispatchTask(record)
		still, err := s.store.Get(result.RecordID)
		if err != nil || still.State != "received" || !still.ReceivedAt.Equal(record.ReceivedAt) || still.Sequence != record.Sequence {
			t.Fatal(still, err)
		}
	}
	if len(wechatTaskWrites(writer)) != 0 {
		t.Fatal("task dispatched without reply context")
	}
	status := a.receiveIncomingMessage(t.Context(), wechatTextMessage("3", "owner", "/status", "fresh-context"), a.token.Load())
	if status.Disposition != im.ReceiveAccepted {
		t.Fatal(status)
	}
	waitIngressCondition(t, func() bool { return len(wechatTaskWrites(writer)) == 1 })
	if writes := wechatTaskWrites(writer); writes[0]["content"] != "first task" {
		t.Fatal(writes)
	}
	// A new ordinary task is accepted behind the received tasks already waiting.
	third := a.receiveIncomingMessage(t.Context(), wechatTextMessage("4", "owner", "third task", "fresh-context"), a.token.Load())
	if third.Disposition != im.ReceiveAccepted {
		t.Fatal(third)
	}
	for i, result := range accepted {
		payload, _ := json.Marshal(InputResultPayload{RequestID: "im-" + result.RecordID, Status: inputQueued})
		p.mu.Lock()
		p.applyInputResult(&HeadlessEnvelope{Payload: payload, Seq: uint64(i + 1)}, time.Now())
		p.mu.Unlock()
		waitIngressCondition(t, func() bool { return len(wechatTaskWrites(writer)) == i+2 })
	}
	writes := wechatTaskWrites(writer)
	if writes[1]["content"] != "second task" || writes[2]["content"] != "third task" {
		t.Fatal(writes)
	}
}

func TestWechatIngressUnauthorizedDoesNotRefreshContext(t *testing.T) {
	a, s, _, writer := newWechatIngressTest(t)
	for i, sender := range []string{"stranger", "other-sender"} {
		result := a.receiveIncomingMessage(t.Context(), wechatTextMessage(fmt.Sprint(i+10), sender, "private task", "untrusted-context"), a.token.Load())
		if result.Disposition != im.ReceiveRejected || result.RecordID == "" {
			t.Fatal(result)
		}
		record, err := s.store.Get(result.RecordID)
		if err != nil || record.Text != "" || record.State != "rejected" {
			t.Fatal(record, err)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.replyContexts) != 0 || len(wechatTaskWrites(writer)) != 0 {
		t.Fatal("unauthorized context refreshed readiness or dispatched work")
	}
}

func TestWechatIngressCredentialsNeverPersist(t *testing.T) {
	a, s, _, _ := newWechatIngressTest(t)
	result := a.receiveIncomingMessage(t.Context(), wechatTextMessage("21", "owner", "sample task", "private-reply-credential"), a.token.Load())
	if result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	record, err := s.store.Get(result.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(record)
	if strings.Contains(string(encoded), "private-reply-credential") || strings.Contains(string(encoded), "sample-token") {
		t.Fatal("transport credentials persisted in receipt")
	}
}

// An unpaired private sender is guided with a hint, but the transport still
// has to prepare the inbound reply context or the hint can never be sent.
func TestWechatUnpairedHintPreparesReplyContext(t *testing.T) {
	r, _ := unpairedTestRouter(t)
	store := NewIngressStore()
	service := NewIngressService(store, r)
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close(); store.Close() })
	a := newTestWechatAdapter(t)
	a.msgRouter = r
	a.imCfg = *r.getConfig().IMConfigByType("wechat")
	a.token.Store(&TokenData{AccountID: "bot", Token: "sample-token"})
	r.SetAdapter(a)
	result := a.receiveIncomingMessage(t.Context(), wechatTextMessage("1", "owner", "hello", "sample-context"), a.token.Load())
	if result.Reason != "pairing_hint_text" {
		t.Fatal(result)
	}
	a.mu.Lock()
	reply, ok := a.replyContexts[compositeKey("bot", "owner")]
	a.mu.Unlock()
	if !ok || reply.token != "sample-context" {
		t.Fatal("hint cannot be sent: verified inbound reply context was not prepared")
	}
}

func TestWechatIngressOldTokenAndMalformedTransport(t *testing.T) {
	a, s, _, _ := newWechatIngressTest(t)
	old := a.token.Load()
	a.token.Store(&TokenData{AccountID: old.AccountID, Token: "replacement-token"})
	result := a.receiveIncomingMessage(t.Context(), wechatTextMessage("30", "owner", "sample task", "stale-context"), old)
	if result.Disposition != im.ReceiveRetryable || result.Reason != "transport_identity_changed" {
		t.Fatal(result)
	}
	for _, message := range []ilinkMessage{
		wechatTextMessage("0", "owner", "sample task", "context"),
		wechatTextMessage("31", "", "sample task", "context"),
		{MessageType: ilinkMessageTypeBot, MessageID: "32"},
		{MessageType: ilinkMessageTypeUser, MessageID: "33", FromUserID: "owner"},
	} {
		if result := a.receiveIncomingMessage(t.Context(), message, a.token.Load()); result.Disposition != im.ReceiveTransportIgnored {
			t.Fatal(result)
		}
	}
	records, err := s.store.listAll()
	if err != nil || len(records) != 0 {
		t.Fatal(records, err)
	}
}

func TestWechatIngressPrepareOnlyAfterCommonAuthorization(t *testing.T) {
	_, s, _, _ := newWechatIngressTest(t)
	calls := 0
	message := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "stranger", SenderID: "stranger", MessageID: "prepare-denied", Text: "task"}
	result := s.router.ReceiveIngress(context.Background(), im.IngressEnvelope{Message: message, Preparation: &im.IngressPreparation{Valid: func() bool { return true }, Prepare: func(im.AdmissionResult) bool { calls++; return true }}})
	if result.Disposition != im.ReceiveRejected || calls != 0 {
		t.Fatal(result, calls)
	}
}

func TestWechatIngressOversizedTextCommitsRefusalAndContinues(t *testing.T) {
	a, s, _, writer := newWechatIngressTest(t)
	long := wechatTextMessage("41", "owner", strings.Repeat("x", (64<<10)+1), "unaccepted-context")
	refused := a.receiveIncomingMessage(t.Context(), long, a.token.Load())
	if refused.Disposition != im.ReceiveRejected || refused.RecordID == "" || refused.Reason != "invalid_input" {
		t.Fatal(refused)
	}
	record, err := s.store.Get(refused.RecordID)
	if err != nil || record.Text != "" || record.State != "rejected" {
		t.Fatal(record, err)
	}
	if readiness := a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "wechat", AccountID: "bot", ChatID: "owner"}); readiness.Ready {
		t.Fatal("rejected text opened delivery context")
	}
	next := a.receiveIncomingMessage(t.Context(), wechatTextMessage("42", "owner", "next task", "fresh-context"), a.token.Load())
	if next.Disposition != im.ReceiveAccepted {
		t.Fatal(next)
	}
	waitIngressCondition(t, func() bool { return len(wechatTaskWrites(writer)) == 1 })
	if writes := wechatTaskWrites(writer); writes[0]["content"] != "next task" {
		t.Fatal(writes)
	}
}

func TestWechatIngressWriterRechecksPreparedIdentity(t *testing.T) {
	_, s, _, _ := newWechatIngressTest(t)
	valid := true
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "owner", SenderID: "owner", MessageID: "identity-changed", Text: "sample task"}
	// The authority permits preparation, then the transport's snapshot expires
	// before the writer commits its business record.
	result := s.router.ReceiveIngress(t.Context(), im.IngressEnvelope{Message: msg, Preparation: &im.IngressPreparation{
		Valid:   func() bool { return valid },
		Prepare: func(im.AdmissionResult) bool { valid = false; return true },
	}})
	if result.Disposition != im.ReceiveRetryable || result.Reason != "transport_identity_changed" {
		t.Fatal(result)
	}
	if _, err := s.store.lookupCommittedBusiness("wechat", "bot", "identity-changed"); err != errIngressMissing {
		t.Fatal("expired snapshot left a durable acceptance", err)
	}
}

func TestWechatIngressDuplicatePairingRefusalDoesNotSpendAttempts(t *testing.T) {
	r, code := pairedTestRouter(t)
	store := NewIngressStore()
	s := NewIngressService(store, r)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); store.Close() })
	a := newTestWechatAdapter(t)
	a.msgRouter = r
	p := r.admission.pairing
	p.mu.Lock()
	fixed := p.now()
	p.now = func() time.Time { return fixed }
	p.mu.Unlock()
	bad := wechatTextMessage("51", "owner", fmt.Sprintf("/bind %q invalid-code", t.TempDir()), "invalid-context")
	first := a.receiveIncomingMessage(t.Context(), bad, a.token.Load())
	if first.Disposition != im.ReceiveRejected {
		t.Fatal(first)
	}
	p.mu.Lock()
	tokens := p.platforms["wechat"].tokens
	p.mu.Unlock()
	a.mu.Lock()
	contexts := len(a.replyContexts)
	a.mu.Unlock()
	if contexts == 0 {
		t.Fatal("fresh refusal must prepare a reply context for its hint")
	}
	for range 35 {
		if duplicate := a.receiveIncomingMessage(t.Context(), bad, a.token.Load()); duplicate.Disposition != im.ReceiveDuplicate || duplicate.State != "rejected" {
			t.Fatal(duplicate)
		}
	}
	p.mu.Lock()
	remaining := p.platforms["wechat"].tokens
	p.mu.Unlock()
	a.mu.Lock()
	after := len(a.replyContexts)
	a.mu.Unlock()
	if remaining != tokens || after != contexts {
		t.Fatal("replayed refusal spent pairing attempts or prepared a context")
	}
	good := a.receiveIncomingMessage(t.Context(), wechatTextMessage("52", "owner", fmt.Sprintf("/bind %q %s", t.TempDir(), code), "pair-context"), a.token.Load())
	if good.Disposition != im.ReceiveAccepted {
		t.Fatal(good)
	}
	waitIngressCondition(t, func() bool { record, err := store.Get(good.RecordID); return err == nil && record.State == "completed" })
	if readiness := a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "wechat", AccountID: "bot", ChatID: "owner"}); readiness.Ready {
		t.Fatal("first pairing context enabled ordinary task delivery")
	}
}

func TestWechatOutgoingUsesOriginalAccountBeforeHTTP(t *testing.T) {
	a := newTestWechatAdapter(t)
	a.token.Store(&TokenData{AccountID: "replacement", Token: "replacement-token", BaseURL: "https://example.invalid"})
	a.cachePairedContext("replacement", "owner", "replacement-context", true)
	calls := 0
	a.httpClient.Transport = imageRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("unexpected HTTP request")
	})
	ctx := context.WithValue(t.Context(), replyAccountContextKey{}, "original")
	if err := a.SendTextContext(ctx, "owner", "original account result"); err == nil {
		t.Fatal("text switched account")
	}
	if err := a.SendImageContext(ctx, "owner", imageForUpload(t)); err == nil {
		t.Fatal("image switched account")
	}
	if calls != 0 {
		t.Fatal("old output reached replacement-account HTTP endpoint")
	}
}
