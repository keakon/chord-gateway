package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestInputQueueProjectionVersionsAndBindings(t *testing.T) {
	p := &ChordProcess{runtimeID: "runtime", sessionEpoch: 1, state: ControlState{SessionID: "session"}}
	published := 0
	p.onEvent = func(_ string, kind string, state ControlState) {
		if kind == "input_queue_changed" {
			published++
			state.InputQueue.Inputs = nil
		}
	}
	q := InputQueue{InputQueueTarget: InputQueueTarget{RuntimeID: "runtime", SessionID: "session", SessionEpoch: 1, OwnerTaskID: "main"}, ViewVersion: 1, QueueRevision: 2, Gate: InputQueueGate{Mode: "running"}, Inputs: []InputQueueItem{{InputID: "input", Revision: 1, Sequence: 1, Policy: "after_completion", Stage: "queued", Preview: "sample"}}}
	send := func(seq uint64) {
		data, err := json.Marshal(q)
		if err != nil {
			t.Fatal(err)
		}
		p.processEnvelope(&HeadlessEnvelope{Type: "input_queue_changed", Seq: seq, Payload: data})
	}
	send(1)
	if published != 1 || len(p.State().InputQueue.Inputs) != 1 {
		t.Fatal("projection was not published")
	}
	q.ViewVersion = 2
	q.Blocked = "storage failed"
	send(2)
	if published != 2 || p.State().InputQueue.Blocked == "" {
		t.Fatal("same durable revision lost blocked update")
	}
	q.ViewVersion = 1
	q.Blocked = ""
	send(3)
	if published != 2 {
		t.Fatal("older view published")
	}
	q.ViewVersion = 3
	q.RuntimeID = "old-runtime"
	send(4)
	if published != 2 {
		t.Fatal("stale runtime published")
	}
	q.RuntimeID = "runtime"
	q.Inputs = nil
	send(5)
	if published != 3 || len(p.State().InputQueue.Inputs) != 0 {
		t.Fatal("known empty queue did not replace old projection")
	}
}
func TestInputQueueUnknownReceiptDoesNotStartWork(t *testing.T) {
	p := &ChordProcess{pendingInputs: map[string]struct{}{"request": {}}}
	data, _ := json.Marshal(InputResultPayload{RequestID: "request", Status: inputUnknown})
	if !p.applyInputResult(&HeadlessEnvelope{Payload: data, Seq: 1}, time.Now()) {
		t.Fatal("unknown receipt rejected")
	}
	if len(p.pendingInputs) != 0 || p.state.Busy {
		t.Fatal("unknown receipt retained waiter or started work")
	}
}
func TestInputQueueReceiptRejectsStaleBinding(t *testing.T) {
	p := &ChordProcess{runtimeID: "runtime", sessionEpoch: 2, state: ControlState{SessionID: "session"}, pendingInputs: map[string]struct{}{"request": {}}}
	target := InputQueueTarget{RuntimeID: "old", SessionID: "session", SessionEpoch: 2}
	result := InputResultPayload{Target: &target, RequestID: "request", Status: inputQueued}
	data, _ := json.Marshal(result)
	if p.applyInputResult(&HeadlessEnvelope{Payload: data, Seq: 1}, time.Now()) || len(p.pendingInputs) != 1 {
		t.Fatal("stale receipt consumed current waiter")
	}
	target.RuntimeID = "runtime"
	data, _ = json.Marshal(result)
	if !p.applyInputResult(&HeadlessEnvelope{Payload: data, Seq: 2}, time.Now()) || len(p.pendingInputs) != 0 {
		t.Fatal("current receipt did not settle waiter")
	}
}

func TestInputQueueGuardsDirectorySwitch(t *testing.T) {
	for _, blocked := range []string{"queued", "storage", "restoring", "empty"} {
		t.Run(blocked, func(t *testing.T) {
			r, code := pairedTestRouter(t)
			first, second := t.TempDir(), t.TempDir()
			msg, key := bindTestOwner(t, r, code, first)
			ready := make(chan struct{})
			close(ready)
			p := &ChordProcess{readySignal: ready, runtimeID: "runtime", sessionEpoch: 1, state: ControlState{SessionID: "session", PausedInputs: &PausedInputs{}}}
			r.mgr.procs[key] = p
			q := InputQueue{InputQueueTarget: InputQueueTarget{RuntimeID: "runtime", SessionID: "session", SessionEpoch: 1, OwnerTaskID: "main"}, ViewVersion: 1}
			switch blocked {
			case "queued":
				q.Inputs = []InputQueueItem{{InputID: "input", Revision: 1, Sequence: 1, Policy: "next_request", Stage: "queued"}}
			case "storage":
				q.Blocked = "storage outcome unknown"
			case "restoring":
				q.RestoredNextInputsBlocked = true
			}
			data, _ := json.Marshal(q)
			p.processEnvelope(&HeadlessEnvelope{Type: "input_queue_changed", Seq: 1, Payload: data})
			msg.Text = fmt.Sprintf("/bind %q", second)
			msg.MessageID = "switch"
			dispatchPairingTest(r, msg)
			ws, err := r.getConfig().ResolveWorkspace("wechat", "chat")
			if err != nil {
				t.Fatal(err)
			}
			want := first
			if blocked == "empty" {
				want = second
			}
			canonical, _, _ := canonicalBindingDirectory(want)
			if ws.Path != canonical {
				t.Fatalf("directory = %s, want %s", ws.Path, canonical)
			}
			if blocked != "empty" && p.stoppedByGateway {
				t.Fatal("pending input process was stopped")
			}
		})
	}
}

func TestUnknownInputOutcomeRemainsVisibleAndBlocksRebind(t *testing.T) {
	for _, seq := range []uint64{1, 3} {
		t.Run(fmt.Sprint(seq), func(t *testing.T) {
			r := newTestRouter()
			adapter := &stubIMAdapter{typ: "wechat"}
			r.adapter = adapter
			key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat-1"}).String()
			ready := make(chan struct{})
			close(ready)
			stdin := &captureWriteCloser{}
			p := &ChordProcess{key: key, readySignal: ready, stdin: stdin, onEvent: r.HandleChordEvent, pendingInputs: map[string]struct{}{"request": {}}, state: ControlState{LastEnvelopeSeq: 2}}
			r.mgr.procs[key] = p
			data, _ := json.Marshal(InputResultPayload{RequestID: "request", Status: inputUnknown})
			env := &HeadlessEnvelope{Type: "input_result", Seq: seq, Payload: data}
			p.processEnvelope(env)
			if !strings.Contains(adapter.lastMessage().text, "unconfirmed") {
				t.Fatal("unknown input outcome was hidden")
			}
			p.processEnvelope(env)
			if p.State().UnconfirmedInputs != 1 || len(adapter.sentMessages()) != 1 {
				t.Fatal("duplicate receipt counted or delivered twice")
			}
			if p.CanRebind() || p.State().Busy || len(p.pendingInputs) != 0 || stdin.String() != "" {
				t.Fatal("unknown outcome started work, retried input or allowed rebind")
			}
			p.abandonPendingInputs()
			if p.State().UnconfirmedInputs != 1 {
				t.Fatal("disconnect lost or double-counted uncertain input")
			}
			if !strings.Contains(formatBindingStatus(nil, "", "", p.State()), "Unconfirmed inputs: 1") {
				t.Fatal("status lost unknown outcome")
			}
		})
	}
}

func TestInputQueueSessionSwitchDropsPreviousProjection(t *testing.T) {
	p := &ChordProcess{state: ControlState{SessionID: "first", InputQueue: &InputQueue{ViewVersion: 1}}}
	p.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Seq: 1, Payload: json.RawMessage(`{"session_id":"second"}`)})
	if p.State().InputQueue != nil {
		t.Fatal("previous session queue remained visible")
	}
}

func TestIdleCleanupPreservesInputGuards(t *testing.T) {
	for _, kind := range []string{"unknown", "queued", "blocked", "receipt", "paused", "restored"} {
		t.Run(kind, func(t *testing.T) {
			r, code := pairedTestRouter(t)
			first, second := t.TempDir(), t.TempDir()
			msg, key := bindTestOwner(t, r, code, first)
			ready := make(chan struct{})
			close(ready)
			p := &ChordProcess{key: key, readySignal: ready, lastActivity: time.Now().Add(-time.Hour), state: ControlState{SessionID: "session", PausedInputs: &PausedInputs{}}}
			switch kind {
			case "receipt":
				p.pendingInputs = map[string]struct{}{"request": {}}
			case "paused":
				p.state.PausedInputs.PendingInputCount = 1
			case "restored":
				p.state.InputQueue = &InputQueue{RestoredNextInputsBlocked: true}
			case "unknown":
				p.state.UnconfirmedInputs = 1
			case "queued":
				p.state.InputQueue = &InputQueue{Inputs: []InputQueueItem{{InputID: "pending"}}}
			case "blocked":
				p.state.InputQueue = &InputQueue{Blocked: "storage outcome unknown"}
			}
			r.mgr.procs[key] = p
			if p.CanRebind() {
				t.Fatal("setup is not guarded")
			}
			retired := r.mgr.collectIdleProcesses(time.Minute)
			msg.Text = fmt.Sprintf("/bind %q", second)
			msg.MessageID = "switch"
			dispatchPairingTest(r, msg)
			ws, err := r.getConfig().ResolveWorkspace("wechat", "chat")
			if err != nil {
				t.Fatal(err)
			}
			expected, _, _ := canonicalBindingDirectory(first)
			if ws.Path != expected {
				t.Fatalf("guard bypassed: retired=%d directory switched without resolving %s input state", len(retired), kind)
			}
		})
	}
}

func TestDeadProcessCannotDiscardUnknownInputs(t *testing.T) {
	m := newTestChordManager(testConfig())
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	p := &ChordProcess{key: key, exited: true, state: ControlState{UnconfirmedInputs: 1}}
	m.procs[key] = p
	if _, err := m.getOrSpawnForKeyChatLocked(key); !errors.Is(err, errUnresolvedInputs) {
		t.Fatalf("reconnect: %v", err)
	}
	if _, err := m.replaceOrdinarySession(key, "other"); !errors.Is(err, errUnresolvedInputs) {
		t.Fatalf("replace: %v", err)
	}
	if m.GetProcessForKey(key) != p {
		t.Fatal("lost guarded process")
	}
	r := NewNotificationRouter(m)
	defer r.Close()
	adapter := &stubIMAdapter{typ: "wechat"}
	r.SetAdapter(adapter)
	r.handleChordCommand(r.getConfig().WorkspaceByID("test"), "chat", im.IMCommand{Type: "status"}, "wechat", nil)
	if text := adapter.lastMessage().text; !strings.Contains(text, "disconnected") || !strings.Contains(text, "Unconfirmed inputs: 1") {
		t.Fatalf("guarded status: %s", text)
	}
	p.stdin = &captureWriteCloser{}
	if err := p.SendCommand(map[string]any{"type": "send", "content": "/new"}); !errors.Is(err, errUnresolvedInputs) {
		t.Fatalf("new: %v", err)
	}
}
func TestInputQueueStatusExplainsBlockedAndPausedQueues(t *testing.T) {
	for _, q := range []*InputQueue{{Blocked: "private storage path", Gate: InputQueueGate{Mode: "running"}}, {Gate: InputQueueGate{Mode: "paused"}}, {RestoredNextInputsBlocked: true}} {
		text := formatBindingStatus(nil, "", "", ControlState{InputQueue: q})
		if !strings.Contains(text, "Input queue:") || strings.Contains(text, "private storage path") {
			t.Fatal(text)
		}
		if q.Blocked != "" && !strings.Contains(text, "storage is blocked") {
			t.Fatal(text)
		}
		if q.Gate.Mode == "paused" && !strings.Contains(text, "local Chord TUI") {
			t.Fatal(text)
		}
		if q.RestoredNextInputsBlocked && !strings.Contains(text, "explicit continuation") {
			t.Fatal(text)
		}
	}
}

// The bounded 128-input case exercises the projection carried by every event,
// including envelopes that do not publish a callback.
func BenchmarkProcessEnvelopeWithInputQueue(b *testing.B) {
	for _, size := range []int{0, 128} {
		b.Run(fmt.Sprintf("inputs-%d", size), func(b *testing.B) {
			p := &ChordProcess{}
			if size > 0 {
				p.state.InputQueue = &InputQueue{Inputs: make([]InputQueueItem, size)}
			}
			env := HeadlessEnvelope{Type: "subscribe_response"}
			b.ReportAllocs()
			for b.Loop() {
				p.processEnvelope(&env)
			}
		})
	}
}

func TestQuestionCallbacksOwnInputQueueSnapshot(t *testing.T) {
	p := &ChordProcess{state: ControlState{SessionID: "session", InputQueue: &InputQueue{Inputs: []InputQueueItem{{InputID: "original"}}}}}
	calls := 0
	p.onEvent = func(_ string, kind string, state ControlState) {
		if kind != "question_request" {
			return
		}
		calls++
		if state.InputQueue.Inputs[0].InputID != "original" {
			t.Fatal("callback inherited another caller's mutation")
		}
		state.InputQueue.Inputs[0].InputID = "changed"
	}
	for _, id := range []string{"first", "second"} {
		payload := json.RawMessage(fmt.Sprintf(`{"request_id":%q,"question":"Choose","version":1}`, id))
		p.processEnvelope(&HeadlessEnvelope{Type: "question_request", Payload: payload})
	}
	if calls != 2 || p.State().InputQueue.Inputs[0].InputID != "original" {
		t.Fatalf("queue leaked across callbacks: calls=%d, queue=%+v", calls, p.State().InputQueue)
	}
}

func TestWaitStatusOwnsInputQueueSnapshot(t *testing.T) {
	writer := &captureWriteCloser{}
	p := &ChordProcess{stdin: writer, state: ControlState{SessionID: "session", InputQueue: &InputQueue{Inputs: []InputQueueItem{{InputID: "original"}}}}}
	done := make(chan ControlState, 1)
	go func() { state, _ := p.WaitStatus(t.Context()); done <- state }()
	deadline := time.Now().Add(time.Second)
	for writer.String() == "" {
		if time.Now().After(deadline) {
			t.Fatal("status request not sent")
		}
		time.Sleep(time.Millisecond)
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Payload: json.RawMessage(`{"session_id":"session"}`)})
	select {
	case state := <-done:
		state.InputQueue.Inputs[0].InputID = "changed"
		if p.State().InputQueue.Inputs[0].InputID != "original" {
			t.Fatal("status caller mutated cached queue")
		}
	case <-time.After(time.Second):
		t.Fatal("status response not received")
	}
}
