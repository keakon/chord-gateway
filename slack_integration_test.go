package main

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

type slackIntegrationAdapter struct{ pairingTestAdapter }

func (*slackIntegrationAdapter) Type() string { return "slack" }

type slackIntegrationHarness struct {
	router  *NotificationRouter
	service *IngressService
	inbound *slackInbound
	code    string
	mu      sync.Mutex
	lookup  func(http.ResponseWriter, *http.Request)
}

func newSlackIntegrationHarness(t *testing.T) *slackIntegrationHarness {
	t.Helper()
	store := testConfigStore(t, "ims:\n  slack:\n    app_token: test-app-token\n    bot_token: test-bot-token\n    app_id: A1\n    team_id: T1\n")
	paths := &config.Paths{StateDir: t.TempDir()}
	manager := NewChordManager(store.Snapshot(), paths)
	manager.store = store
	router := NewNotificationRouter(manager)
	var terminal bytes.Buffer
	pairing := NewPairingService(store, true, &terminal)
	router.admission = NewAdmission(pairing)
	manager.admission = router.admission
	router.SetAdapter(&slackIntegrationAdapter{})
	if err := pairing.PlatformReadyWithPresentation("slack", "account", im.PlatformPresentation{Platform: "slack", BotMention: "<@UBOT>"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(terminal.String(), "/chord bind") {
		t.Fatal("pairing instructions omitted installed slash entry point")
	}
	pairing.mu.Lock()
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(pairing.codeNonce[:])
	pairing.mu.Unlock()
	ingressStore := NewIngressStore()
	service := NewIngressService(ingressStore, router)
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h := &slackIntegrationHarness{router: router, service: service, code: code}
	h.lookup = func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		channel := r.Form.Get("channel")
		conversation := map[string]any{"id": channel, "is_channel": true}
		if strings.HasPrefix(channel, "D") {
			conversation = map[string]any{"id": channel, "is_im": true, "user": "U1"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": conversation})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		lookup := h.lookup
		h.mu.Unlock()
		lookup(w, r)
	}))
	h.inbound = newSlackInbound(slack.New("test-token", slack.OptionAPIURL(server.URL+"/")), slackTestInbound().identity, router)
	t.Cleanup(func() {
		service.Close()
		router.admission.Close()
		router.Close()
		server.Close()
		if err := ingressStore.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}

func (h *slackIntegrationHarness) receive(t *testing.T, kind string, payload json.RawMessage, delivery string) im.ReceiveResult {
	t.Helper()
	future, cancel, err := h.inbound.submit(t.Context(), kind, payload, delivery, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	ctx, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	return im.WaitIngressNormalization(ctx, future)
}

func (h *slackIntegrationHarness) bindDM(t *testing.T) {
	t.Helper()
	result := h.receive(t, "slash_commands", slackTestSlash(fmt.Sprintf("bind %q %s", t.TempDir(), h.code), "PAIR1"), "ENVPAIR1")
	if result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	waitIngressCondition(t, func() bool {
		owner := h.router.getConfig().IMConfigByType("slack").OwnerIdentity()
		return owner != nil && owner.AccountID == "account" && owner.UserID == "U1"
	})
}

func (h *slackIntegrationHarness) attachProcess(t *testing.T, target string) (*ChordProcess, *ingressTestWriter) {
	t.Helper()
	workspace, err := h.router.getConfig().ResolveWorkspace("slack", target)
	if err != nil || workspace == nil {
		t.Fatal("target not bound", err)
	}
	key := (processKey{workspaceID: workspace.ID, imType: "slack", chatID: target}).String()
	writer := &ingressTestWriter{}
	ready := make(chan struct{})
	close(ready)
	process := &ChordProcess{key: key, runtimeID: "runtime", sessionEpoch: 1, stdin: writer, readySignal: ready, state: ControlState{SessionID: "session"}}
	h.router.mgr.mu.Lock()
	h.router.mgr.procs[key] = process
	h.router.mgr.mu.Unlock()
	if err = h.router.mgr.pins.Set(pinKeyForBinding(key), "session"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		process.mu.Lock()
		for _, observer := range process.inputObservers {
			if observer.timer != nil {
				observer.timer.Stop()
			}
		}
		process.mu.Unlock()
	})
	return process, writer
}

func slackIntegrationEvent(channel, thread, text, fraction string) slackInboundMessage {
	event := slackInboundMessage{Type: "app_mention", Channel: channel, User: "U1", Text: text, TS: fmt.Sprintf("%d.%s", time.Now().Unix(), fraction), ThreadTS: thread}
	if strings.HasPrefix(channel, "D") {
		event.Type, event.ChannelType = "message", "im"
	}
	return event
}

func TestSlackIntegrationPairingDMProofAndPersistence(t *testing.T) {
	h := newSlackIntegrationHarness(t)
	h.mu.Lock()
	validLookup := h.lookup
	h.lookup = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"id": "D1", "is_im": true, "user": "UOTHER"}})
	}
	h.mu.Unlock()
	result := h.receive(t, "slash_commands", slackTestSlash(fmt.Sprintf("bind %q %s", t.TempDir(), h.code), "BADPAIR"), "ENVBADPAIR")
	if result.Disposition != im.ReceiveRejected || h.router.getConfig().IMConfigByType("slack").OwnerIdentity() != nil {
		t.Fatal("unverified DM paired owner", result)
	}
	record, err := h.service.store.Get(result.RecordID)
	if err != nil || record.Text != "" {
		t.Fatal("rejected pairing retained command body", record, err)
	}
	h.mu.Lock()
	h.lookup = validLookup
	h.mu.Unlock()
	h.bindDM(t)
	data, err := os.ReadFile(h.router.mgr.store.path)
	if err != nil || !bytes.Contains(data, []byte("account_id: account")) || !bytes.Contains(data, []byte("user_id: U1")) {
		t.Fatal("owner not written to configuration", string(data), err)
	}
}

func TestSlackIntegrationBusinessAndSemanticDeduplication(t *testing.T) {
	h := newSlackIntegrationHarness(t)
	h.bindDM(t)
	process, writer := h.attachProcess(t, "D1")
	event := slackIntegrationEvent("D1", "", "first task", "000001")
	first := h.receive(t, "events_api", slackTestEvent(event, "E1"), "ENV1")
	redelivery := h.receive(t, "events_api", slackTestEvent(event, "E1"), "ENV2")
	semanticDuplicate := h.receive(t, "events_api", slackTestEvent(event, "E2"), "ENV3")
	if first.Disposition != im.ReceiveAccepted || redelivery.Disposition != im.ReceiveDuplicate || semanticDuplicate.Disposition != im.ReceiveDuplicate || first.RecordID != redelivery.RecordID || first.RecordID != semanticDuplicate.RecordID {
		t.Fatal(first, redelivery, semanticDuplicate)
	}
	event.TS = fmt.Sprintf("%d.000002", time.Now().Unix())
	event.Text = "second task"
	second := h.receive(t, "events_api", slackTestEvent(event, "E3"), "ENV4")
	if second.Disposition != im.ReceiveAccepted || second.RecordID == first.RecordID {
		t.Fatal(second)
	}
	waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 1 })
	payload, _ := json.Marshal(InputResultPayload{RequestID: "im-" + first.RecordID, Status: inputQueued})
	process.mu.Lock()
	process.applyInputResult(&HeadlessEnvelope{Payload: payload, Seq: 1}, time.Now())
	process.mu.Unlock()
	waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 2 })
	if messages := writer.snapshot(); messages[0]["content"] != "first task" || messages[1]["content"] != "second task" {
		t.Fatal(messages)
	}
}

func TestSlackIntegrationThreadBindingDoesNotBindSlashRoot(t *testing.T) {
	h := newSlackIntegrationHarness(t)
	h.bindDM(t)
	thread := fmt.Sprintf("%d.000000", time.Now().Unix())
	target, _ := slackTarget("C1", thread)
	bind := slackIntegrationEvent("C1", thread, fmt.Sprintf("<@UBOT> /bind %q", t.TempDir()), "000001")
	result := h.receive(t, "events_api", slackTestEvent(bind, "THREADBIND"), "ENVTHREADBIND")
	if result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	waitIngressCondition(t, func() bool {
		workspace, err := h.router.getConfig().ResolveWorkspace("slack", target)
		return err == nil && workspace != nil
	})
	process, writer := h.attachProcess(t, target)
	var slash slackSlashPayload
	_ = json.Unmarshal(slackTestSlash("status", "ROOTSTATUS"), &slash)
	slash.ChannelID = "C1"
	payload, _ := json.Marshal(slash)
	root := h.receive(t, "slash_commands", payload, "ENVROOTSTATUS")
	if root.Disposition != im.ReceiveRejected || root.Reason != "chat_unbound" {
		t.Fatal("slash inherited thread binding", root)
	}
	first := slackIntegrationEvent("C1", thread, "<@UBOT> first thread task", "000002")
	one := h.receive(t, "events_api", slackTestEvent(first, "THREAD1"), "ENVTHREAD1")
	first.Text, first.TS = "<@UBOT> second thread task", fmt.Sprintf("%d.000003", time.Now().Unix())
	two := h.receive(t, "events_api", slackTestEvent(first, "THREAD2"), "ENVTHREAD2")
	if one.Disposition != im.ReceiveAccepted || two.Disposition != im.ReceiveAccepted || one.RecordID == two.RecordID {
		t.Fatal("different thread messages deduplicated", one, two)
	}
	waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 1 })
	receipt, _ := json.Marshal(InputResultPayload{RequestID: "im-" + one.RecordID, Status: inputQueued})
	process.mu.Lock()
	process.applyInputResult(&HeadlessEnvelope{Payload: receipt, Seq: 1}, time.Now())
	process.mu.Unlock()
	waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 2 })
	first.Text = "task without mention"
	if _, _, _, err := h.inbound.decode("events_api", slackTestEvent(first, "UNMENTIONED"), "ENVUNMENTIONED", time.Now()); err != errSlackIgnored {
		t.Fatal("thread task without mention admitted", err)
	}
}

func TestSlackIntegrationFrozenAllowCannotApproveNewConfirmation(t *testing.T) {
	h := newSlackIntegrationHarness(t)
	h.bindDM(t)
	process, writer := h.attachProcess(t, "D1")
	process.mu.Lock()
	process.state.PendingConfirm = &ConfirmPayload{RequestID: "old-confirm"}
	process.mu.Unlock()
	started, release := make(chan struct{}), make(chan struct{})
	h.mu.Lock()
	lookup := h.lookup
	h.lookup = func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			lookup(w, r)
		case <-r.Context().Done():
		}
	}
	h.mu.Unlock()
	h.inbound.mu.Lock()
	h.inbound.cache = make(map[string]slackCachedConversation)
	h.inbound.mu.Unlock()
	future, cancel, err := h.inbound.submit(t.Context(), "slash_commands", slackTestSlash("allow", "ALLOW1"), "ENVALLOW", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	<-started
	process.mu.Lock()
	process.state.PendingConfirm = &ConfirmPayload{RequestID: "new-confirm"}
	process.mu.Unlock()
	close(release)
	result := im.WaitIngressNormalization(t.Context(), future)
	if result.Disposition != im.ReceiveAccepted {
		t.Fatal(result)
	}
	waitIngressCondition(t, func() bool {
		record, err := h.service.store.Get(result.RecordID)
		return err == nil && record.State == "rejected" && record.Reason == "control_target_expired"
	})
	if len(writer.snapshot()) != 0 {
		t.Fatal("frozen allow approved replacement confirmation", writer.snapshot())
	}
}
