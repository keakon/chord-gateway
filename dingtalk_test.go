package main

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"

	dingtalk "github.com/keakon/chord-gateway/internal/platform/dingtalk"
)

type dingtalkAdapterFixture struct {
	a        *dingtalk.DingtalkAdapter
	r        *NotificationRouter
	s        *IngressService
	terminal *bytes.Buffer
	store    *ConfigStore
}

func newDingtalkAdapterFixture(t *testing.T) dingtalkAdapterFixture {
	t.Helper()
	store := testConfigStore(t, fmt.Sprintf("ims: {dingtalk: {client_id: %q, client_secret: test-secret, corp_id: test-corp, robot_code: test-robot}}\n", "test-client-"+newIngressID()))
	mgr := NewChordManager(store.Snapshot(), &config.Paths{StateDir: t.TempDir()})
	mgr.store = store
	r := NewNotificationRouter(mgr)
	terminal := &bytes.Buffer{}
	r.admission = NewAdmission(NewPairingService(store, true, terminal))
	mgr.admission = r.admission
	db := testIngressStore(t)
	s := NewIngressService(db, r)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	a, err := dingtalk.NewDingtalkAdapter(store.Snapshot(), *store.Snapshot().IMConfigByType("dingtalk"), &config.Paths{StateDir: t.TempDir()}, r)
	if err != nil {
		t.Fatal(err)
	}
	r.SetAdapter(a)
	t.Cleanup(func() { a.Disconnect(); s.Close(); r.admission.Close(); r.Close() })
	return dingtalkAdapterFixture{a, r, s, terminal, store}
}

func dingtalkAdapterFrame(t *testing.T, callback dingtalk.DingtalkCallback, delivery string) dingtalk.DingtalkFrame {
	t.Helper()
	return dingtalk.DingtalkFrame{SpecVersion: "1.0", Type: "CALLBACK", Headers: map[string]json.RawMessage{
		"topic": json.RawMessage(fmt.Sprintf("%q", dingtalk.DingtalkRobotTopic)), "messageId": json.RawMessage(fmt.Sprintf("%q", delivery)),
		"time": json.RawMessage(fmt.Sprintf("%d", time.Now().UnixMilli())),
	}, Data: dingtalk.IdentityTestPayloadForTest(t, callback)}
}

func dingtalkAdapterReceive(t *testing.T, a *dingtalk.DingtalkAdapter, callback dingtalk.DingtalkCallback, delivery string) im.ReceiveResult {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		future, err := a.OnCallbackForTest(t.Context(), dingtalkAdapterFrame(t, callback, delivery))
		if err == nil {
			return receiveNormalization(t, future)
		}
		// Capture reads the target's process state without waiting, so a delivery
		// that arrives while that state is being written is refused as
		// target_busy. Robot callbacks are fire-and-forget, so production does
		// not redeliver: the user resends. The retry below models that resend to
		// keep the suite immune to a scheduling race between deliveries of one
		// chat.
		if !errors.Is(err, im.ErrIngressCaptureBusy) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dingtalkAdapterVerify(t *testing.T, f dingtalkAdapterFixture) {
	t.Helper()
	if err := f.a.OnConnectedForTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	callback := dingtalk.IdentityTestCallbackForTest()
	callback.MsgID, callback.Text.Content = "identity-hello", "hello"
	if result := dingtalkAdapterReceive(t, f.a, callback, "identity-delivery"); result.Disposition != im.ReceiveRejected || result.Reason != "identity_probe" {
		t.Fatalf("probe result: %+v", result)
	}
}

func dingtalkAdapterNonce(f dingtalkAdapterFixture) string {
	p := f.r.admission.pairing
	p.mu.Lock()
	defer p.mu.Unlock()
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(p.codeNonce[:])
}

func TestDingtalkAdapterProbeNeverExecutesOrAuthorizesOwner(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	if err := f.a.OnConnectedForTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.terminal.String(), "Valid for 10 minutes") {
		t.Fatal("pairing code was shown before verified enterprise identity")
	}
	group := dingtalk.IdentityTestCallbackForTest()
	group.MsgID, group.ConversationType, group.IsInAtList = "first-group", "2", true
	group.Text.Content = "sensitive unverified task"
	result := dingtalkAdapterReceive(t, f.a, group, "group-delivery")
	if result.Disposition != im.ReceiveRejected || result.Reason != "identity_unverified" {
		t.Fatal(result)
	}
	groupRecordID := result.RecordID
	if f.a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "dingtalk", AccountID: f.a.AccountForTest(), ChatID: "any"}).Ready {
		t.Fatal("group callback verified enterprise identity")
	}
	probe := dingtalk.IdentityTestCallbackForTest()
	probe.MsgID, probe.Text.Content = "probe-bind-body", "/bind \"/project\" invalid-nonce"
	result = dingtalkAdapterReceive(t, f.a, probe, "probe-delivery")
	if result.Disposition != im.ReceiveRejected || result.Reason != "identity_probe" {
		t.Fatal(result)
	}
	if !f.a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "dingtalk", AccountID: f.a.AccountForTest(), ChatID: "any"}).Ready || !strings.Contains(f.terminal.String(), "Valid for 10 minutes") {
		t.Fatal("successful identity probe did not unlock pairing")
	}
	if strings.Contains(f.terminal.String(), "@robot /bind") {
		t.Fatal("private pairing instructions require a group mention")
	}
	privateTarget, _ := dingtalk.EncodeTargetForTest(f.a.ConfigForTest().CorpID, "private", probe.SenderStaffID)
	if hint := f.r.commandHint("dingtalk", im.CommandTargetForChat("dingtalk", privateTarget), "/bind /project"); hint != "/bind /project" {
		t.Fatalf("private reply hint: %q", hint)
	}
	if f.store.Snapshot().IMConfigByType("dingtalk").OwnerIdentity() != nil {
		t.Fatal("identity probe registered an owner")
	}
	if valid := f.a.VerifiedForTest(); !valid {
		t.Fatalf("unexpected identity verification: %v", valid)
	}
	records, err := f.s.store.listAll()
	if err != nil || len(records) != 2 {
		t.Fatalf("records=%v err=%v", records, err)
	}
	for _, record := range records {
		if record.Text != "" || record.State != "rejected" {
			t.Fatalf("unverified input retained or became executable: %+v", record)
		}
	}
	duplicate := dingtalkAdapterReceive(t, f.a, group, "different-delivery")
	if duplicate.Disposition != im.ReceiveDuplicate || duplicate.RecordID != groupRecordID || duplicate.State != "rejected" {
		t.Fatalf("unverified group replayed after readiness: %+v", duplicate)
	}
	f.r.mgr.mu.Lock()
	defer f.r.mgr.mu.Unlock()
	if len(f.r.mgr.procs) != 0 {
		t.Fatal("identity probe started a Chord process")
	}
}

func TestDingtalkAdapterRejectsUnsupportedSenderAndTenant(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	if err := f.a.OnConnectedForTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	callback := dingtalk.IdentityTestCallbackForTest()
	callback.SenderStaffID = ""
	frame := dingtalkAdapterFrame(t, callback, "fallback-delivery")
	frame.Data = strings.TrimSuffix(frame.Data, "}") + `,"senderId":"fallback-owner","isAdmin":true}`
	future, err := f.a.OnCallbackForTest(t.Context(), frame)
	if err != nil || receiveNormalization(t, future).Disposition != im.ReceiveRejected {
		t.Fatalf("unsupported sender accepted: %v", err)
	}
	callback = dingtalk.IdentityTestCallbackForTest()
	callback.RobotCode = "other-robot"
	future, err = f.a.OnCallbackForTest(t.Context(), dingtalkAdapterFrame(t, callback, "wrong-robot"))
	dingtalk.AssertIdentityCategoryForTest(t, err, "tenant_mismatch", true)
	if future != nil {
		t.Fatal("mismatched tenant was enqueued")
	}
	if valid := f.a.VerifiedForTest(); valid {
		t.Fatalf("unexpected identity verification: %v", valid)
	}
}

func TestDingtalkAdapterBindingProbeFailureRetainsNonce(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	dingtalkAdapterVerify(t, f)
	var mu sync.Mutex
	denied, sends := true, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/oauth2/accessToken" {
			fmt.Fprint(w, `{"accessToken":"test-token","expireIn":7200}`)
			return
		}
		var payload struct {
			UserIDs  []string `json:"userIds"`
			MsgParam string   `json:"msgParam"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		var body struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal([]byte(payload.MsgParam), &body)
		if r.URL.Path != "/v1.0/robot/oToMessages/batchSend" || (body.Content != "Gateway binding check." && !strings.HasPrefix(body.Content, "output ")) || len(payload.UserIDs) != 1 || payload.UserIDs[0] != "test-staff" {
			t.Errorf("invalid binding request: %s %+v", r.URL.Path, payload)
		}
		mu.Lock()
		sends++
		fail := denied
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"code":"Forbidden"}`)
			return
		}
		fmt.Fprint(w, `{"processQueryKey":"accepted-probe"}`)
	}))
	defer server.Close()
	f.a.SetAPIServerForTest(server.URL, server.Client())
	f.a.SetAPIPacingForTest(0, 0)
	msg, err := dingtalk.NormalizeCallbackForTest(f.a.ConfigForTest(), dingtalk.IdentityTestPayloadForTest(t, dingtalk.IdentityTestCallbackForTest()))
	if err != nil {
		t.Fatal(err)
	}
	code := dingtalkAdapterNonce(f)
	dir := t.TempDir()
	msg.Text = fmt.Sprintf("/bind %q %s", dir, code)
	for attempt := range 2 {
		ticket, ok := f.r.admission.Authorize(msg)
		if !ok {
			t.Fatalf("attempt %d lost nonce authorization", attempt)
		}
		result, err := (&BindingService{router: f.r}).Bind(t.Context(), msg, ticket, []string{dir, code})
		if attempt == 0 {
			if err == nil || f.store.Snapshot().IMConfigByType("dingtalk").OwnerIdentity() != nil {
				t.Fatal("failed active send saved owner/binding")
			}
			mu.Lock()
			denied = false
			mu.Unlock()
		} else if err != nil || !result.First {
			t.Fatalf("successful retry: %+v/%v", result, err)
		}
	}
	msg.Text = fmt.Sprintf("/bind %q", dir)
	ticket, ok := f.r.admission.Authorize(msg)
	if !ok {
		t.Fatal("bound owner denied")
	}
	result, err := (&BindingService{router: f.r}).Bind(t.Context(), msg, ticket, []string{dir})
	if err != nil || !result.Already {
		t.Fatalf("idempotent bind: %+v/%v", result, err)
	}
	mu.Lock()
	count := sends
	mu.Unlock()
	if count != 2 {
		t.Fatalf("active probes=%d", sends)
	}
	// Continue through actual task ingress after owner registration. Delivery IDs
	// identify ACKs; the stable platform message ID alone deduplicates execution.
	ws, err := f.r.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	key := (processKey{workspaceID: ws.ID, imType: msg.IMType, chatID: msg.ChatID}).String()
	writer := &ingressTestWriter{}
	ready := make(chan struct{})
	close(ready)
	p := &ChordProcess{key: key, runtimeID: "runtime", sessionEpoch: 1, stdin: writer, readySignal: ready, state: ControlState{SessionID: "session"}}
	f.r.mgr.mu.Lock()
	f.r.mgr.procs[key] = p
	f.r.mgr.mu.Unlock()
	if err := f.r.mgr.pins.Set(pinKeyForBinding(key), "session"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, observer := range p.inputObservers {
			if observer.timer != nil {
				observer.timer.Stop()
			}
		}
	})
	callback := dingtalk.IdentityTestCallbackForTest()
	callback.MsgID, callback.Text.Content = "bound-task", "run this task once"
	accepted := dingtalkAdapterReceive(t, f.a, callback, "task-delivery-one")
	if accepted.Disposition != im.ReceiveAccepted {
		t.Fatal(accepted)
	}
	duplicate := dingtalkAdapterReceive(t, f.a, callback, "task-delivery-two")
	if duplicate.Disposition != im.ReceiveDuplicate || duplicate.RecordID != accepted.RecordID {
		t.Fatal(duplicate)
	}
	waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 1 })
	callback.SenderStaffID, callback.MsgID = "another-staff", "unauthorized-task"
	deniedResult := dingtalkAdapterReceive(t, f.a, callback, "unauthorized-delivery")
	if deniedResult.Disposition != im.ReceiveRejected || len(writer.snapshot()) != 1 {
		t.Fatalf("nonowner controlled Chord: %+v", deniedResult)
	}
	// A binding switch cancels the shared delivery lease after the first accepted
	// segment. Later segments must never enter even the adapter's HTTP transport.
	f.a.OnUnavailableForTest() // HTTP output keeps its lease through a Stream outage.
	target, ok := f.r.targetForKey(key)
	if !ok {
		t.Fatal("bound delivery target unavailable")
	}
	original := f.a.SenderForTest().SendSegmentForTest()
	f.a.SenderForTest().SetSendSegmentForTest(func(ctx context.Context, chat, text string) error {
		err := original(ctx, chat, text)
		if err == nil {
			ticket.Gate.Switching()
			<-ctx.Done() // Join the router's asynchronous lease cancellation.
		}
		return err
	})
	err = f.r.SendBound(t.Context(), target, strings.Repeat("output ", 400))
	var stopped *im.IMTextSendError
	if !errors.As(err, &stopped) || stopped.AcceptedSegments != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("binding switch failed to stop remaining segments: %v", err)
	}
	mu.Lock()
	count = sends
	mu.Unlock()
	if count != 3 {
		t.Fatalf("binding switch sent %d messages after 2 probes", count)
	}
}

func TestDingtalkAdapterUnboundSendAndOldGenerationDenied(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	dingtalkAdapterVerify(t, f)
	target, _ := dingtalk.EncodeTargetForTest(f.a.ConfigForTest().CorpID, "private", "test-staff")
	if err := f.a.SendText(target, "must not send"); !errors.Is(err, im.ErrPairingDenied) {
		t.Fatalf("unbound send: %v", err)
	}
	generation := f.a.GenerationForTest()
	f.a.OnUnavailableForTest()
	if err := f.a.ActivateForTest(t.Context(), generation); !errors.Is(err, context.Canceled) {
		t.Fatalf("old generation activated: %v", err)
	}
	ready := f.a.ReadyForTest()
	if ready {
		t.Fatal("old connection enabled new admission")
	}
	f.a.Disconnect()
	if err := f.a.OnConnectedForTest(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped adapter connected: %v", err)
	}
}

func TestDingtalkAdapterTextSegmentsKeepUnicodeAndPartialAcceptance(t *testing.T) {
	for _, scenario := range []string{"success", "second_denied", "second_unknown", "cancel_after_first"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDingtalkAdapterFixture(t)
			dingtalkAdapterVerify(t, f)
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), im.ReplyAccountContextKey{}, f.a.AccountForTest()))
			defer cancel()
			var mu sync.Mutex
			var segments []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1.0/oauth2/accessToken" {
					fmt.Fprint(w, `{"accessToken":"test-token","expireIn":7200}`)
					return
				}
				var payload struct {
					OpenConversationID string `json:"openConversationId"`
					MsgParam           string `json:"msgParam"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				var body struct {
					Content string `json:"content"`
				}
				_ = json.Unmarshal([]byte(payload.MsgParam), &body)
				if payload.OpenConversationID != "original/group:id" || !utf8.ValidString(body.Content) || len(body.Content) > 1024 {
					t.Errorf("invalid segment payload: %+v", payload)
				}
				mu.Lock()
				segments = append(segments, body.Content)
				number := len(segments)
				mu.Unlock()
				if scenario == "second_unknown" && number == 2 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if scenario == "second_denied" && number == 2 {
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"code":"Forbidden"}`)
					return
				}
				fmt.Fprint(w, `{"processQueryKey":"accepted-segment"}`)
			}))
			defer server.Close()
			f.a.SetAPIServerForTest(server.URL, server.Client())
			f.a.SetAPIPacingForTest(0, 0)
			if scenario == "cancel_after_first" {
				original := f.a.SenderForTest().SendSegmentForTest()
				f.a.SenderForTest().SetSendSegmentForTest(func(ctx context.Context, target, text string) error {
					err := original(ctx, target, text)
					if err == nil {
						cancel()
					}
					return err
				})
			}
			target, _ := dingtalk.EncodeTargetForTest(f.a.ConfigForTest().CorpID, "group", "original/group:id")
			text := strings.Repeat("测试🙂", 250)
			err := f.a.SendTextContext(ctx, target, text)
			mu.Lock()
			got := append([]string(nil), segments...)
			mu.Unlock()
			if scenario == "success" {
				if err != nil || len(got) != 3 || strings.Join(got, "") != text {
					t.Fatalf("segments=%d err=%v", len(got), err)
				}
			} else {
				var stopped *im.IMTextSendError
				if !errors.As(err, &stopped) || stopped.AcceptedSegments != 1 {
					t.Fatalf("partial acceptance: %v", err)
				}
				want := map[string]string{"second_denied": "forbidden", "second_unknown": "unknown_send", "cancel_after_first": "cancelled"}[scenario]
				if stopped.Category != want || f.a.TextSendTargetSnapshot(target).LastFailureCategory != want {
					t.Fatalf("category=%s want=%s", stopped.Category, want)
				}
				if scenario == "cancel_after_first" && len(got) != 1 {
					t.Fatalf("cancel sent %d segments", len(got))
				}
			}
		})
	}
}

func TestDingtalkAdapterConnectCachedIdentityAndDisconnect(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	f.a.SetVerifiedForTest(true)
	connections := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/gateway/connections/open":
			fmt.Fprintf(w, `{"endpoint":%q,"ticket":"single-use-test-ticket"}`, "wss://"+r.Host+"/stream")
		case "/stream":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			connections <- conn
		}
	}))
	defer server.Close()
	f.a.SetAPIServerForTest(server.URL, server.Client())
	f.a.SetStreamTLSConfigForTest(server.Client().Transport.(*http.Transport).TLSClientConfig.Clone())
	done := make(chan error, 1)
	go func() { done <- f.a.Connect() }()
	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-time.After(2 * time.Second):
		t.Fatal("no Stream handshake")
	}
	defer conn.Close()
	waitIngressCondition(t, func() bool {
		return f.a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "dingtalk", AccountID: f.a.AccountForTest(), ChatID: "chat"}).Ready
	})
	f.a.Disconnect()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Disconnect did not join Connect")
	}
	if f.a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "dingtalk", AccountID: f.a.AccountForTest(), ChatID: "chat"}).Ready {
		t.Fatal("disconnected adapter remained ready")
	}
}

func TestDingtalkAdapterConnectTenantMismatchStopsReconnect(t *testing.T) {
	f := newDingtalkAdapterFixture(t)
	connections := make(chan *websocket.Conn, 1)
	var registrations atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/gateway/connections/open":
			registrations.Add(1)
			fmt.Fprintf(w, `{"endpoint":%q,"ticket":"test-ticket"}`, "wss://"+r.Host+"/stream")
		case "/stream":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err == nil {
				connections <- conn
			}
		}
	}))
	defer server.Close()
	f.a.SetAPIServerForTest(server.URL, server.Client())
	f.a.SetStreamTLSConfigForTest(server.Client().Transport.(*http.Transport).TLSClientConfig.Clone())
	f.a.SetStreamBackoffForTest(func(int) time.Duration { return time.Millisecond })
	done := make(chan error, 1)
	go func() { done <- f.a.Connect() }()
	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-time.After(2 * time.Second):
		t.Fatal("no Stream handshake")
	}
	defer conn.Close()
	callback := dingtalk.IdentityTestCallbackForTest()
	callback.ChatbotCorpID = "unexpected-corp"
	if err := conn.WriteJSON(dingtalkAdapterFrame(t, callback, "tenant-mismatch")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		dingtalk.AssertIdentityCategoryForTest(t, err, "tenant_mismatch", true)
	case <-time.After(2 * time.Second):
		t.Fatal("tenant mismatch did not terminate supervisor")
	}
	if registrations.Load() != 1 {
		t.Fatalf("terminal mismatch reconnected %d times", registrations.Load())
	}
	if f.a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "dingtalk", AccountID: f.a.AccountForTest(), ChatID: "chat"}).Ready {
		t.Fatal("mismatched tenant became ready")
	}
}

func TestDingtalkAdapterBindingProbeRechecksNonceAndRoute(t *testing.T) {
	for _, scenario := range []string{"nonce", "route"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDingtalkAdapterFixture(t)
			dingtalkAdapterVerify(t, f)
			callback := dingtalk.IdentityTestCallbackForTest()
			msg, err := dingtalk.NormalizeCallbackForTest(f.a.ConfigForTest(), dingtalk.IdentityTestPayloadForTest(t, callback))
			if err != nil {
				t.Fatal(err)
			}
			code, dir := dingtalkAdapterNonce(f), t.TempDir()
			msg.Text = fmt.Sprintf("/bind %q %s", dir, code)
			ticket, ok := f.r.admission.Authorize(msg)
			if !ok {
				t.Fatal("valid nonce denied")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1.0/oauth2/accessToken" {
					fmt.Fprint(w, `{"accessToken":"test-token","expireIn":7200}`)
					return
				}
				if scenario == "nonce" {
					p := f.r.admission.pairing
					p.mu.Lock()
					p.codeID = "replaced-nonce"
					p.mu.Unlock()
				} else {
					if _, err := f.s.store.BeginRouteChange(r.Context(), ticket.Identity); err != nil {
						t.Error(err)
					}
				}
				fmt.Fprint(w, `{"processQueryKey":"accepted-but-authority-changed"}`)
			}))
			defer server.Close()
			f.a.SetAPIServerForTest(server.URL, server.Client())
			_, err = (&BindingService{router: f.r}).Bind(t.Context(), msg, ticket, []string{dir, code})
			if !errors.Is(err, im.ErrPairingDenied) {
				t.Fatalf("authority changed during probe: %v", err)
			}
			imCfg := f.store.Snapshot().IMConfigByType("dingtalk")
			if imCfg.OwnerIdentity() != nil || len(imCfg.Bindings()) != 0 {
				t.Fatal("probe saved binding under replaced authority")
			}
		})
	}
}

func TestDingtalkAdapterSourceTimeStrictlyAcceptsMilliseconds(t *testing.T) {
	for _, scenario := range []string{"missing", "integer", "decimal_string", "boolean", "object", "null", "empty_string", "fraction", "old", "future", "exponent_string", "huge", "negative_huge"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDingtalkAdapterFixture(t)
			if err := f.a.OnConnectedForTest(t.Context()); err != nil {
				t.Fatal(err)
			}
			frame := dingtalkAdapterFrame(t, dingtalk.IdentityTestCallbackForTest(), "timestamp-delivery")
			ms := time.Now().UnixMilli()
			valid := scenario == "missing" || scenario == "integer" || scenario == "decimal_string"
			switch scenario {
			case "missing":
				delete(frame.Headers, "time")
			case "integer":
				frame.Headers["time"] = json.RawMessage(fmt.Sprintf("%d", ms))
			case "decimal_string":
				frame.Headers["time"] = json.RawMessage(fmt.Sprintf("\"%d\"", ms))
			case "boolean":
				frame.Headers["time"] = json.RawMessage(`true`)
			case "object":
				frame.Headers["time"] = json.RawMessage(`{}`)
			case "null":
				frame.Headers["time"] = json.RawMessage(`null`)
			case "empty_string":
				frame.Headers["time"] = json.RawMessage(`""`)
			case "fraction":
				frame.Headers["time"] = json.RawMessage(`1.5`)
			case "old":
				frame.Headers["time"] = json.RawMessage(fmt.Sprintf("%d", time.Now().Add(-25*time.Hour).UnixMilli()))
			case "future":
				frame.Headers["time"] = json.RawMessage(fmt.Sprintf("%d", time.Now().Add(6*time.Minute).UnixMilli()))
			case "exponent_string":
				frame.Headers["time"] = json.RawMessage(`"1e12"`)
			case "huge":
				frame.Headers["time"] = json.RawMessage(`9223372036854775807`)
			case "negative_huge":
				frame.Headers["time"] = json.RawMessage(`-9223372036854775808`)
			}
			future, err := f.a.OnCallbackForTest(t.Context(), frame)
			if err != nil {
				t.Fatal(err)
			}
			result := receiveNormalization(t, future)
			if result.Disposition != im.ReceiveRejected {
				t.Fatal(result)
			}
			if valid && result.Reason != "identity_probe" || !valid && result.Reason != "source_time_out_of_range" {
				t.Fatalf("timestamp %s: %+v", scenario, result)
			}
			if f.a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "dingtalk", AccountID: f.a.AccountForTest(), ChatID: "chat"}).Ready != valid {
				t.Fatal("invalid timestamp established identity readiness")
			}
			record, err := f.s.store.Get(result.RecordID)
			if err != nil || record.Text != "" {
				t.Fatalf("timestamp rejection retained body or omitted tombstone: %+v/%v", record, err)
			}
			if valid && scenario != "missing" && !record.SourceTime.Equal(time.UnixMilli(ms)) {
				t.Fatalf("timestamp mismatch: %v want %v", record.SourceTime, time.UnixMilli(ms))
			}
			if scenario == "missing" && !record.SourceTime.Equal(record.ReceivedAt) {
				t.Fatal("missing timestamp did not use first receive time")
			}
			if !valid {
				// Correcting transport metadata must not upgrade a permanently
				// rejected business message into identity evidence on redelivery.
				frame.Headers["time"] = json.RawMessage(fmt.Sprintf("%d", time.Now().UnixMilli()))
				frame.Headers["messageId"] = json.RawMessage(`"replacement-delivery"`)
				future, err := f.a.OnCallbackForTest(t.Context(), frame)
				if err != nil {
					t.Fatal(err)
				}
				duplicate := receiveNormalization(t, future)
				if duplicate.Disposition != im.ReceiveDuplicate || duplicate.RecordID != result.RecordID || duplicate.Reason != "source_time_out_of_range" {
					t.Fatal(duplicate)
				}
				if f.a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "dingtalk", AccountID: f.a.AccountForTest(), ChatID: "chat"}).Ready {
					t.Fatal("previously rejected business ID established identity")
				}
				if valid := f.a.VerifiedForTest(); valid {
					t.Fatalf("unexpected identity verification: %v", valid)
				}
			}
		})
	}
}

func TestDingtalkLiteralGroupCallback(t *testing.T) {
	// Independent wire fixture following the official Stream protocol example:
	// https://open-dingtalk.github.io/developerpedia/docs/learn/stream/protocol/
	// Group mentions are represented by isInAtList/atUsers; text has no bot prefix.
	const payload = `{
		"conversationId":"test-group", "conversationType":"2",
		"chatbotCorpId":"test-corp", "chatbotUserId":"test-bot-user",
		"senderCorpId":"test-corp", "senderStaffId":"test-staff",
		"senderId":"test-user", "robotCode":"test-robot", "msgId":"test-message",
		"isInAtList":true, "atUsers":[{"dingtalkId":"test-bot-user"}],
		"msgtype":"text", "text":{"content":" /bind /project/path"}
	}`
	msg, err := dingtalk.NormalizeCallbackForTest(dingtalk.IdentityTestConfigForTest(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ChatKind != "group" || msg.SenderID != "test-staff" || parseIMCommand(msg.Text).Type != "bind" {
		t.Fatalf("group command did not normalize: %+v", msg)
	}
	// Literal mentions in the user's task must not be stripped speculatively.
	msg, err = dingtalk.NormalizeCallbackForTest(dingtalk.IdentityTestConfigForTest(), strings.Replace(payload, "/bind /project/path", "@someone review this", 1))
	if err != nil || msg.Text != "@someone review this" {
		t.Fatal(msg.Text, err)
	}
}

func TestDingtalkAdapterReportsActualRateWaits(t *testing.T) {
	for _, scenario := range []string{"target", "account", "retry", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDingtalkAdapterFixture(t)
			dingtalkAdapterVerify(t, f)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1.0/oauth2/accessToken" {
					dingtalk.WriteTokenForTest(w)
					return
				}
				if calls.Add(1) == 1 && scenario == "retry" {
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				fmt.Fprint(w, `{"processQueryKey":"accepted"}`)
			}))
			defer server.Close()
			f.a.SetAPIServerForTest(server.URL, server.Client())
			f.a.SetAPIPacingForTest(0, 0)
			f.a.SetAPIRetryBaseForTest(20 * time.Millisecond)
			if scenario == "cancel" {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				if err := f.a.WaitRateLimitForTest(ctx, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			} else {
				target, _ := dingtalk.EncodeTargetForTest(f.a.ConfigForTest().CorpID, "private", "test-staff")
				ctx := context.WithValue(t.Context(), im.ReplyAccountContextKey{}, f.a.AccountForTest())
				if err := f.a.SendTextContext(ctx, target, "first output"); err != nil {
					t.Fatal(err)
				}
				if scenario != "retry" {
					// Place the next slot in the future deterministically; the HTTP
					// test server's scheduling cannot consume the intended wait.
					if scenario == "target" {
						f.a.HoldTargetSlotForTest("private:test-staff", time.Now().Add(20*time.Millisecond))
					} else {
						f.a.HoldAccountSlotForTest(time.Now().Add(20 * time.Millisecond))
					}
					if err := f.a.SendTextContext(ctx, target, "second output"); err != nil {
						t.Fatal(err)
					}
				}
				if calls.Load() != 2 {
					t.Fatalf("unexpected HTTP sends: %d", calls.Load())
				}
			}
			metrics := f.a.TextSendMetrics()
			if metrics.RateLimitWaits != 1 || metrics.RateLimitWaitTotal <= 0 || metrics.RateLimitWaitMax != metrics.RateLimitWaitTotal || metrics.RateLimitWaitTotal >= time.Second {
				t.Fatalf("actual wait missing or requested delay counted: %+v", metrics)
			}
			before := metrics
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_ = f.a.WaitRateLimitForTest(ctx, time.Hour)
			_ = f.a.WaitRateLimitForTest(t.Context(), 0)
			if after := f.a.TextSendMetrics(); after != before {
				t.Fatalf("non-waits changed metrics: before=%+v after=%+v", before, after)
			}
		})
	}
}
