package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/keakon/chord-gateway/internal/im"
)

// Exercise actual text normalization and the shared persistent service, rather
// than invoking a parallel Feishu command dispatcher.
func receiveFeishuCapabilityText(t *testing.T, r *NotificationRouter, business, text string) im.ReceiveResult {
	t.Helper()
	content, _ := json.Marshal(map[string]string{"text": text})
	payload, _ := json.Marshal(map[string]any{"schema": "2.0", "header": map[string]string{"app_id": "app", "event_id": "event-" + business}, "event": map[string]any{"sender": map[string]any{"sender_type": "user", "sender_id": map[string]string{"open_id": "owner"}}, "message": map[string]string{"message_id": business, "chat_id": "chat", "chat_type": "p2p", "message_type": "text", "content": string(content)}}})
	var event larkim.P2MessageReceiveV1
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	future, err := findFeishuAdapterForTest(r).SubmitMessageEvent(t.Context(), &event, "delivery-"+business)
	if err != nil || future == nil {
		t.Fatal(err)
	}
	return im.WaitIngressNormalization(t.Context(), future)
}

func TestFeishuCapabilitiesDoneDenialRequiresReason(t *testing.T) {
	var mu sync.Mutex
	var replies []string
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(req.Body).Decode(&body)
		var content map[string]string
		_ = json.Unmarshal([]byte(body["content"]), &content)
		mu.Lock()
		replies = append(replies, content["text"])
		mu.Unlock()
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
	})
	service, id := migrationFeishuIngress(t, r, msg)
	p.mu.Lock()
	p.state.PendingConfirm = &ConfirmPayload{RequestID: "done", ToolName: "Done"}
	p.mu.Unlock()
	accepted := receiveFeishuCapabilityText(t, r, "deny", "/deny")
	if accepted.Disposition != im.ReceiveAccepted {
		t.Fatal(accepted)
	}
	dispatchIngressTestRoute(service, id)
	flushIngressPresentation(t, r, msg)
	rec, err := service.store.Get(accepted.RecordID)
	if err != nil || rec.State != "rejected" || rec.Reason != "done_reason_required" {
		t.Fatal(rec, err)
	}
	if writes := p.stdin.(*ingressTestWriter).snapshot(); len(writes) != 0 {
		t.Fatal(writes)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(replies) != 1 || !strings.Contains(replies[0], "Rejecting Done requires a reason") || !strings.Contains(replies[0], "/deny <what remains to do>") {
		t.Fatal(replies)
	}
}

func TestFeishuCapabilitiesPartialStdinRemainsUnknown(t *testing.T) {
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
	})
	service, id := migrationFeishuIngress(t, r, msg)
	p.mu.Lock()
	p.stdin = shortCommandWriter{}
	p.mu.Unlock()
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, observer := range p.inputObservers {
			if observer.timer != nil {
				observer.timer.Stop()
			}
		}
	})
	accepted := receiveFeishuCapabilityText(t, r, "partial", "Task with partial input")
	if accepted.Disposition != im.ReceiveAccepted {
		t.Fatal(accepted)
	}
	dispatchIngressTestRoute(service, id)
	rec, err := service.store.Get(accepted.RecordID)
	if err != nil || rec.State != "unknown" || rec.Reason != "stdin_outcome_unknown" || rec.SessionID != "session" || rec.Text != "Task with partial input" {
		t.Fatal(rec, err)
	}
	duplicate := receiveFeishuCapabilityText(t, r, "partial", "Task with partial input")
	if duplicate.Disposition != im.ReceiveDuplicate || duplicate.State != "unknown" {
		t.Fatal(duplicate)
	}
	if r.mgr.GetProcessForKey(p.key) != p || r.mgr.pins.Get(pinKeyForBinding(p.key)) != "session" {
		t.Fatal("partial write replaced original process or session")
	}
}

func TestFeishuCapabilitiesPinnedRestoreFailureIsQuarantined(t *testing.T) {
	r, p, msg := migrationFeishuRouter(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"message"}}`)
	})
	service, id := migrationFeishuIngress(t, r, msg)
	r.mgr.mu.Lock()
	delete(r.mgr.procs, p.key)
	r.mgr.mu.Unlock()
	r.getConfig().ChordPath = filepath.Join(t.TempDir(), "missing-chord")
	accepted := receiveFeishuCapabilityText(t, r, "restore", "Preserve original session task")
	if accepted.Disposition != im.ReceiveAccepted {
		t.Fatal(accepted)
	}
	dispatchIngressTestRoute(service, id)
	rec, err := service.store.Get(accepted.RecordID)
	if err != nil || rec.State != "quarantined" || rec.Reason != "session_restore_failed" || rec.SessionID != "session" {
		t.Fatal(rec, err)
	}
	// The body is released on isolation: the input will never reach Chord.
	if rec.Text != "" {
		t.Fatal("isolation retained the body", rec)
	}
	if r.mgr.GetProcessForKey(p.key) != nil || r.mgr.pins.Get(pinKeyForBinding(p.key)) != "session" {
		t.Fatal("failed resume lost original pin or created another runtime")
	}
	duplicate := receiveFeishuCapabilityText(t, r, "restore", "Preserve original session task")
	if duplicate.Disposition != im.ReceiveDuplicate || duplicate.State != "quarantined" {
		t.Fatal(duplicate)
	}
}
