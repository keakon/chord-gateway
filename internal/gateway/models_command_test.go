package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
			done := make(chan struct{})
			go func() {
				r.handleChordCommand(ws, "chat-1", parseIMCommand(tc.command), "wechat", &im.IncomingMessage{})
				close(done)
			}()
			requestIDs := waitForModelsRequestIDs(t, proc, 1)
			payload, err := json.Marshal(ModelsResponse{RequestID: requestIDs[0], OK: true, Status: "current pool: anthropic"})
			if err != nil {
				t.Fatal(err)
			}
			proc.processEnvelope(&HeadlessEnvelope{Type: "models_response", Payload: payload})
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("models command did not finish after its response")
			}
			if got := stdin.String(); !containsAll(got, tc.wantStd) {
				t.Fatalf("stdin = %q, want every field in %q", got, tc.wantStd)
			}
			if !strings.Contains(stdin.String(), `"request_id":"`+requestIDs[0]+`"`) {
				t.Fatalf("models command omitted request_id %q", requestIDs[0])
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
	r, proc, sender, _, ws := newModelsTestRouter()
	done := make(chan struct{})
	go func() {
		r.handleChordCommand(ws, "chat-1", parseIMCommand("/models nope"), "wechat", &im.IncomingMessage{})
		close(done)
	}()
	requestIDs := waitForModelsRequestIDs(t, proc, 1)
	payload, err := json.Marshal(ModelsResponse{RequestID: requestIDs[0], Message: "unknown pool nope"})
	if err != nil {
		t.Fatal(err)
	}
	proc.processEnvelope(&HeadlessEnvelope{Type: "models_response", Payload: payload})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("models command did not finish after its response")
	}
	if got := sender.lastMessage().text; !strings.Contains(got, "unknown pool nope") {
		t.Fatalf("reply = %q, want Chord's reason", got)
	}
}

func TestModelsFailureMessageMatchesAction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		{name: "status", command: "/models", want: "Could not read the model pool from Chord. Retry /models."},
		{name: "switch", command: "/models fast", want: "Could not confirm the model pool switch. Reply /models to check the current pool."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, proc, sender, _, ws := newModelsTestRouter()
			proc.stdin = &errorWriteCloser{err: io.ErrClosedPipe}
			r.handleModelsCommand(ws, "chat-1", parseIMCommand(tc.command), proc.key, proc)
			reply := sender.lastMessage().text
			if !strings.Contains(reply, tc.want) {
				t.Fatalf("reply = %q, want %q", reply, tc.want)
			}
			if strings.Contains(reply, "/status") {
				t.Fatalf("reply = %q, should direct the user to /models", reply)
			}
		})
	}
}

// TestModelsResponseWakesWaiters proves only the matching models_response
// releases the caller.
func TestModelsResponseWakesWaiters(t *testing.T) {
	p := &ChordProcess{stdin: &captureWriteCloser{}}

	done := make(chan ModelsResponse, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		resp, err := p.WaitModels(ctx, "status", "")
		if err != nil {
			t.Errorf("WaitModels: %v", err)
		}
		done <- resp
	}()

	select {
	case <-done:
		t.Fatal("WaitModels returned before a models_response arrived")
	default:
	}

	requestIDs := waitForModelsRequestIDs(t, p, 1)
	payload, err := json.Marshal(ModelsResponse{RequestID: requestIDs[0], OK: true, Status: "pool: alpha"})
	if err != nil {
		t.Fatal(err)
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "models_response", Payload: payload})
	select {
	case resp := <-done:
		if resp.RequestID != requestIDs[0] || resp.Status != "pool: alpha" || !resp.OK {
			t.Fatalf("response = %+v, want ok with pool: alpha", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was not released by models_response")
	}
}

func TestLateModelsResponseDoesNotWakeNextWaiter(t *testing.T) {
	p := &ChordProcess{stdin: &captureWriteCloser{}}

	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelFirst()
	if _, err := p.WaitModels(firstCtx, "set_current_model_pool", "pool-old"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first WaitModels error = %v, want deadline exceeded", err)
	}
	firstRequestIDs := waitForModelsRequestIDs(t, p, 1)
	firstRequestID := firstRequestIDs[0]

	type result struct {
		response ModelsResponse
		err      error
	}
	resultCh := make(chan result, 1)
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelSecond()
	go func() {
		resp, err := p.WaitModels(secondCtx, "set_current_model_pool", "pool-new")
		resultCh <- result{response: resp, err: err}
	}()

	secondRequestIDs := waitForModelsRequestIDs(t, p, 2)
	secondRequestID := secondRequestIDs[1]
	if firstRequestID == secondRequestID {
		t.Fatalf("request ids should be unique: %q", firstRequestID)
	}

	latePayload, err := json.Marshal(ModelsResponse{RequestID: firstRequestID, OK: true, Status: "pool-old"})
	if err != nil {
		t.Fatal(err)
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "models_response", Payload: latePayload})
	select {
	case got := <-resultCh:
		t.Fatalf("late response woke the next waiter: %+v", got)
	default:
	}

	currentPayload, err := json.Marshal(ModelsResponse{RequestID: secondRequestID, OK: true, Status: "pool-new"})
	if err != nil {
		t.Fatal(err)
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "models_response", Payload: currentPayload})
	select {
	case got := <-resultCh:
		if got.err != nil || got.response.Status != "pool-new" {
			t.Fatalf("second WaitModels = %+v, want pool-new response", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("matching response did not release the second waiter")
	}
}

func modelsRequestIDs(input string) []string {
	var requestIDs []string
	for _, line := range strings.Split(input, "\n") {
		var command struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal([]byte(line), &command) == nil && command.Type == "models" {
			requestIDs = append(requestIDs, command.RequestID)
		}
	}
	return requestIDs
}

func waitForModelsRequestIDs(t *testing.T, p *ChordProcess, count int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		requestIDs := modelsRequestIDs(p.stdin.(*captureWriteCloser).String())
		if len(requestIDs) >= count {
			for _, requestID := range requestIDs {
				if requestID == "" {
					t.Fatal("models command has no request_id")
				}
			}
			return requestIDs[:count]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("models request %d was not registered", count)
	return nil
}
