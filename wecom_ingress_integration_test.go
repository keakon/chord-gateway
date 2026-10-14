package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

type wecomCommandFixture struct {
	adapter        *WecomAdapter
	store          *IngressStore
	terminal       *bytes.Buffer
	private, group string
}

// Commands use the real common receiver and outbound WebSocket with no Chord
// runtime or session pin. A nonexistent binary prevents accidental model work.
func newWecomCommandFixture(t *testing.T, paired bool) wecomCommandFixture {
	t.Helper()
	private, _ := EncodeWecomTarget("private", "example-owner")
	group, _ := EncodeWecomTarget("group", "example-group")
	access := ""
	if paired {
		access = fmt.Sprintf(", owner: {account_id: example-bot, user_id: example-owner}, chat_bindings: {%q: project, %q: project}", private, group)
	}
	store := testConfigStore(t, fmt.Sprintf("chord_path: %q\nims: {wecom: {bot_id: example-bot, secret: example-secret%s}}\nworkspaces: {project: {path: %q}}\n", filepath.Join(t.TempDir(), "missing-chord"), access, t.TempDir()))
	paths := &config.Paths{StateDir: t.TempDir()}
	manager := NewChordManager(store.Snapshot(), paths)
	manager.store = store
	router := NewNotificationRouter(manager)
	var terminal bytes.Buffer
	pairing := NewPairingService(store, !paired, &terminal)
	router.admission = NewAdmission(pairing)
	manager.admission = router.admission
	adapter, err := NewWecomAdapter(store.Snapshot(), *store.Snapshot().IMConfigByType("wecom"), paths, router)
	if err != nil {
		t.Fatal(err)
	}
	router.SetAdapter(adapter)
	ingress := NewIngressStore()
	service := NewIngressService(ingress, router)
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = adapter.authenticated(adapter.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adapter.Disconnect()
		service.Close()
		router.admission.Close()
		router.Close()
		if err := ingress.Close(); err != nil {
			t.Error(err)
		}
	})
	return wecomCommandFixture{adapter: adapter, store: ingress, terminal: &terminal, private: private, group: group}
}

func wecomCommandPairingCode(t *testing.T, fixture wecomCommandFixture) string {
	t.Helper()
	waitIngressCondition(t, func() bool {
		return fixture.adapter.IngressDeliveryReadiness(ChatIdentity{"wecom", "example-bot", fixture.private}).Ready
	})
	pairing := fixture.adapter.router.admission.pairing
	pairing.mu.Lock()
	defer pairing.mu.Unlock()
	fields := strings.Fields(fixture.terminal.String())
	for i, field := range fields {
		if field == "\"~/workspace\"" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	t.Fatal("ready connection did not display a pairing code")
	return ""
}

func wecomCommandEnvelope(t *testing.T, id, delivery, kind, sender, content string) IngressEnvelope {
	t.Helper()
	chatType := "single"
	if kind == "group" {
		chatType = "group"
	}
	raw, err := json.Marshal(map[string]any{
		"cmd": "aibot_msg_callback", "headers": map[string]string{"req_id": delivery},
		"body": map[string]any{"aibotid": "example-bot", "msgid": id, "chattype": chatType, "chatid": "example-group", "from": map[string]string{"userid": sender}, "msgtype": "text", "text": map[string]string{"content": content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := NormalizeWecomCallback(raw, "example-bot", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func waitWecomCommandTerminal(t *testing.T, store *IngressStore, id string) IngressRecord {
	t.Helper()
	var record IngressRecord
	waitIngressCondition(t, func() bool {
		var err error
		record, err = store.Get(id)
		return err == nil && record.Terminal()
	})
	return record
}

func TestWecomPrivateAndGroupCommandsWithoutActiveProcess(t *testing.T) {
	fixture := newWecomCommandFixture(t, true)
	var mu sync.Mutex
	replies := map[string][]string{}
	startWecomAdapterTestWS(t, fixture.adapter, func(target, text string) int {
		mu.Lock()
		defer mu.Unlock()
		replies[target] = append(replies[target], text)
		return 0
	})
	for _, kind := range []string{"private", "group"} {
		for _, command := range []string{"/status", "/help"} {
			id := kind + command
			envelope := wecomCommandEnvelope(t, id, "delivery-"+id, kind, "example-owner", command)
			result := fixture.adapter.router.ReceiveIngress(t.Context(), envelope)
			if result.Disposition != ReceiveAccepted {
				t.Fatalf("%s %s: %+v", kind, command, result)
			}
			if record := waitWecomCommandTerminal(t, fixture.store, result.RecordID); record.State != "completed" || record.Kind != "control" {
				t.Fatalf("command was not completed: %+v", record)
			}
			// Delivery IDs are diagnostics and never create another business operation.
			envelope.DeliveryID = "different-delivery-" + id
			duplicate := fixture.adapter.router.ReceiveIngress(t.Context(), envelope)
			if duplicate.Disposition != ReceiveDuplicate || duplicate.RecordID != result.RecordID {
				t.Fatalf("duplicate callback: %+v", duplicate)
			}
		}
	}
	waitIngressCondition(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(replies["example-owner"]) == 2 && len(replies["example-group"]) == 2
	})
	mu.Lock()
	for target, texts := range replies {
		if !strings.Contains(texts[0], "No active session") || !strings.Contains(texts[1], "/status") || !strings.Contains(texts[1], "/bind") {
			t.Errorf("query replies in %s: %v", target, texts)
		}
	}
	mu.Unlock()
	fixture.adapter.router.mgr.mu.Lock()
	count := len(fixture.adapter.router.mgr.procs)
	fixture.adapter.router.mgr.mu.Unlock()
	if count != 0 {
		t.Fatalf("queries created %d processes", count)
	}
	for _, kind := range []string{"private", "group"} {
		envelope := wecomCommandEnvelope(t, "stranger-"+kind, "stranger-delivery", kind, "example-stranger", "/allow example-request")
		result := fixture.adapter.router.ReceiveIngress(t.Context(), envelope)
		if result.Disposition != ReceiveRejected {
			t.Fatalf("non-owner approval admitted in %s: %+v", kind, result)
		}
	}
}

func TestWecomFirstBindingRequiresSuccessfulProactiveACK(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprintf("accepted=%t", accepted), func(t *testing.T) {
			fixture := newWecomCommandFixture(t, false)
			probes := make(chan string, 1)
			startWecomAdapterTestWS(t, fixture.adapter, func(target, text string) int {
				if text == wecomBindingCheck {
					probes <- target
					if !accepted {
						return 600
					}
				}
				return 0
			})
			code := wecomCommandPairingCode(t, fixture)
			configStore := fixture.adapter.router.mgr.store
			before, err := os.ReadFile(configStore.path)
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			groupClaim := wecomCommandEnvelope(t, "group-first-claim", "group-claim-delivery", "group", "example-owner", fmt.Sprintf("/bind %q %s", directory, code))
			if denied := fixture.adapter.router.ReceiveIngress(t.Context(), groupClaim); denied.Disposition != ReceiveRejected {
				t.Fatalf("group callback claimed owner before private pairing: %+v", denied)
			}
			envelope := wecomCommandEnvelope(t, "first-bind", "first-bind-delivery", "private", "example-owner", fmt.Sprintf("/bind %q %s", directory, code))
			result := fixture.adapter.router.ReceiveIngress(t.Context(), envelope)
			if result.Disposition != ReceiveAccepted {
				t.Fatalf("verified /bind not admitted: %+v", result)
			}
			record := waitWecomCommandTerminal(t, fixture.store, result.RecordID)
			select {
			case target := <-probes:
				if target != "example-owner" {
					t.Fatalf("binding probe sent to %s", target)
				}
			default:
				t.Fatal("binding completed without an actual proactive probe")
			}
			im := configStore.Snapshot().IMConfigByType("wecom")
			if accepted {
				if record.State != "completed" || im.OwnerIdentity() == nil || im.OwnerIdentity().UserID != "example-owner" || im.Bindings()[fixture.private] == "" {
					t.Fatalf("successful probe did not pair and bind: %+v %+v", record, im)
				}
				reloaded, err := NewConfigStore(configStore.path)
				if err != nil || reloaded.Snapshot().IMConfigByType("wecom").Bindings()[fixture.private] == "" {
					t.Fatalf("binding not durable: %v", err)
				}
				groupBind := wecomCommandEnvelope(t, "owner-group-bind", "owner-group-delivery", "group", "example-owner", fmt.Sprintf("/bind %q", directory))
				groupResult := fixture.adapter.router.ReceiveIngress(t.Context(), groupBind)
				if groupResult.Disposition != ReceiveAccepted {
					t.Fatalf("paired owner cannot explicitly bind group: %+v", groupResult)
				}
				groupRecord := waitWecomCommandTerminal(t, fixture.store, groupResult.RecordID)
				if groupRecord.State != "completed" || configStore.Snapshot().IMConfigByType("wecom").Bindings()[fixture.group] == "" {
					t.Fatalf("group binding did not persist: %+v", groupRecord)
				}
				select {
				case target := <-probes:
					if target != "example-group" {
						t.Fatalf("group binding probed %s", target)
					}
				default:
					t.Fatal("group binding skipped proactive probe")
				}
			} else {
				after, err := os.ReadFile(configStore.path)
				if err != nil {
					t.Fatal(err)
				}
				if record.State != "rejected" || record.Reason != "binding_not_changed" || im.OwnerIdentity() != nil || len(im.Bindings()) != 0 || !bytes.Equal(before, after) {
					t.Fatalf("rejected probe changed configuration: %+v", record)
				}
			}
		})
	}
}

func TestWecomMissingCommonIngressNeverUsesLegacyDispatch(t *testing.T) {
	fixture := newWecomCommandFixture(t, true)
	fixture.adapter.router.ingress.Close()
	fixture.adapter.router.ingress = nil
	result := fixture.adapter.router.ReceiveIngress(t.Context(), wecomCommandEnvelope(t, "missing-ingress", "missing-delivery", "private", "example-owner", "task"))
	if result.Disposition != ReceiveRetryable || result.Reason != "ingress_unavailable" {
		t.Fatal(result)
	}
	raw, err := json.Marshal(map[string]any{"cmd": "aibot_msg_callback", "headers": map[string]string{"req_id": "callback-without-ingress"}, "body": map[string]any{"aibotid": "example-bot", "msgid": "callback-without-ingress", "chattype": "single", "from": map[string]string{"userid": "example-owner"}, "msgtype": "text", "text": map[string]string{"content": "task"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.adapter.receiveFrame(t.Context(), raw); err != nil {
		t.Fatal(err)
	}
	records, err := fixture.store.listAll()
	if err != nil || len(records) != 0 {
		t.Fatalf("missing common receiver persisted input: %v %v", records, err)
	}
	fixture.adapter.router.mgr.mu.Lock()
	count := len(fixture.adapter.router.mgr.procs)
	fixture.adapter.router.mgr.mu.Unlock()
	if count != 0 {
		t.Fatalf("legacy path spawned %d processes", count)
	}
}
