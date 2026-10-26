package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// newModelsTestRouter mirrors the setup shared by the router command tests so
// the models cases can assert on the exact stdin bytes Chord would receive.
func newModelsTestRouter() (*NotificationRouter, *ChordProcess, *stubIMAdapter, *captureWriteCloser, *config.Workspace) {
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Feishu: &config.FeishuConfig{ChatBindings: map[string]string{"chat-1": "ws1"}}}}, Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1"}}}
	mgr := newTestChordManager(cfg)
	sender := &stubIMAdapter{typ: "wechat"}
	stdin := &captureWriteCloser{}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	proc := &ChordProcess{key: key, workspaceID: "ws1", stdin: stdin}
	attachQuestionTestReceipts(proc)
	mgr.procs[key] = proc
	r := &NotificationRouter{mgr: mgr, adapter: sender, lastKeyChatID: make(map[string]string), expiredPending: make(map[string]expiredPendingState)}
	return r, proc, sender, stdin, &cfg.Workspaces[0]
}

// TestModelsCommandRoundTrip covers both actions the models protocol command
// takes. Chord answers every models command with models_response, so the IM
// reply reflects a confirmed answer rather than optimism.
func TestModelsCommandRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		command string
		// JSON object key order from a Go map is unspecified, so assert on each
		// field instead of one contiguous fragment.
		wantStd []string
		wantMsg string
	}{
		{
			name:    "status",
			command: "/models",
			wantStd: []string{`"type":"models"`, `"action":"status"`},
			wantMsg: "current pool: anthropic",
		},
		{
			name:    "switch pool",
			command: "/models dev-pool",
			wantStd: []string{`"type":"models"`, `"action":"set_current_model_pool"`, `"pool":"dev-pool"`},
			wantMsg: "Switched the current model pool",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, proc, sender, stdin, ws := newModelsTestRouter()
			// Answer the way Chord does: whoever asks gets a models_response.
			go func() {
				for i := 0; i < 200; i++ {
					if strings.Contains(stdin.String(), `"type":"models"`) {
						proc.processEnvelope(&HeadlessEnvelope{Type: "models_response", Payload: []byte(`{"ok":true,"status":"current pool: anthropic"}`)})
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			r.handleChordCommand(ws, "chat-1", parseIMCommand(tc.command), "wechat", &im.IncomingMessage{})
			if got := stdin.String(); !containsAll(got, tc.wantStd) {
				t.Fatalf("stdin = %q, want every field in %q", got, tc.wantStd)
			}
			if got := sender.lastMessage().text; !strings.Contains(got, tc.wantMsg) {
				t.Fatalf("reply = %q, want %q", got, tc.wantMsg)
			}
		})
	}
}

func containsAll(haystack string, needles []string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}

// TestModelsCommandRejection surfaces Chord's explanation rather than a generic
// failure, because "no such pool" is actionable and "failed" is not.
func TestModelsCommandRejection(t *testing.T) {
	r, proc, sender, stdin, ws := newModelsTestRouter()
	go func() {
		for i := 0; i < 200; i++ {
			if strings.Contains(stdin.String(), `"type":"models"`) {
				proc.processEnvelope(&HeadlessEnvelope{Type: "models_response", Payload: []byte(`{"ok":false,"message":"unknown pool nope"}`)})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	r.handleChordCommand(ws, "chat-1", parseIMCommand("/models nope"), "wechat", &im.IncomingMessage{})
	if got := sender.lastMessage().text; !strings.Contains(got, "unknown pool nope") {
		t.Fatalf("reply = %q, want Chord's reason", got)
	}
}

// TestModelsResponseWakesWaiters proves models_response releases the caller.
// models_response carries no request id, so modelsMu is what stops a concurrent
// wait from consuming another's reply and reporting a switch that never happened.
func TestModelsResponseWakesWaiters(t *testing.T) {
	p := &ChordProcess{stdin: &captureWriteCloser{}}

	done := make(chan ModelsResponse, 1)
	go func() {
		resp, err := p.WaitModels(context.Background(), "status", "")
		if err != nil {
			t.Errorf("WaitModels: %v", err)
		}
		done <- resp
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := len(p.modelsWaiters.waiters)
		p.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	select {
	case <-done:
		t.Fatal("WaitModels returned before a models_response arrived")
	default:
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "models_response", Payload: []byte(`{"ok":true,"status":"pool: alpha"}`)})
	select {
	case resp := <-done:
		if resp.Status != "pool: alpha" || !resp.OK {
			t.Fatalf("response = %+v, want ok with pool: alpha", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was not released by models_response")
	}
}
