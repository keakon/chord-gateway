package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func TestTailBufferKeepsOnlyTail(t *testing.T) {
	buf := newTailBuffer(5)
	if n, err := buf.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("first write n=%d err=%v", n, err)
	}
	if n, err := buf.Write([]byte("defgh")); err != nil || n != 5 {
		t.Fatalf("second write n=%d err=%v", n, err)
	}
	if got := buf.String(); got != "defgh" {
		t.Fatalf("buffer tail = %q, want defgh", got)
	}

	if n, err := buf.Write([]byte("1234567")); err != nil || n != 7 {
		t.Fatalf("oversize write n=%d err=%v", n, err)
	}
	if got := buf.String(); got != "34567" {
		t.Fatalf("oversize tail = %q, want 34567", got)
	}
}

func TestTruncateStderrTail(t *testing.T) {
	if got := truncateStderrTail("abcdef", 3); got != "def" {
		t.Fatalf("truncateStderrTail = %q, want def", got)
	}
	if got := truncateStderrTail("short", 10); got != "short" {
		t.Fatalf("truncateStderrTail short = %q", got)
	}
	if got := truncateStderrTail(strings.Repeat("x", 2100), 0); len(got) != 2000 {
		t.Fatalf("default truncate len = %d, want 2000", len(got))
	}
}

func TestProcessEnvelopeReadyPersistsPinOutsideProcessLock(t *testing.T) {
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	pins := &sessionPinStore{
		pins: make(map[string]string),
		writer: func(string, []byte, os.FileMode) error {
			close(writeStarted)
			<-releaseWrite
			return errors.New("write failed")
		},
	}
	mgr := &ChordManager{pins: pins}
	p := &ChordProcess{key: "ws|wechat|chat", mgr: mgr}
	payload, err := json.Marshal(map[string]string{"session_id": "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		p.processEnvelope(&HeadlessEnvelope{Type: "ready", Payload: payload})
		close(done)
	}()

	<-writeStarted
	stateRead := make(chan struct{})
	go func() {
		_ = p.State()
		close(stateRead)
	}()
	select {
	case <-stateRead:
	case <-time.After(time.Second):
		t.Fatal("State blocked while session pin was being persisted")
	}
	close(releaseWrite)
	<-done
}

func BenchmarkProcessEnvelope(b *testing.B) {
	cases := []struct {
		name string
		env  HeadlessEnvelope
	}{
		{name: "subscribe-response", env: HeadlessEnvelope{Type: "subscribe_response"}},
		{name: "activity", env: HeadlessEnvelope{Type: "activity", Payload: json.RawMessage(`{"type":"tool","detail":"running"}`)}},
		{name: "assistant-message", env: HeadlessEnvelope{Type: "assistant_message", Payload: json.RawMessage(`{"text":"completed","agent_id":"agent","tool_calls":2}`)}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			p := &ChordProcess{key: "ws|wechat|chat"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.processEnvelope(&tc.env)
			}
		})
	}
}

func TestProcessEnvelopeUsesConsistentEventTimestamp(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat"}
	p.processEnvelope(&HeadlessEnvelope{
		Type:    "assistant_message",
		Payload: json.RawMessage(`{"text":"completed","agent_id":"agent","tool_calls":2}`),
	})
	state := p.State()
	if !state.LastPushAt.Equal(p.lastActivity) {
		t.Fatalf("LastPushAt = %v, lastActivity = %v", state.LastPushAt, p.lastActivity)
	}
	updatedAt, err := time.Parse(time.RFC3339, state.UpdatedAt)
	if err != nil {
		t.Fatalf("parse UpdatedAt: %v", err)
	}
	if updatedAt.Unix() != p.lastActivity.Unix() {
		t.Fatalf("UpdatedAt = %v, lastActivity = %v", updatedAt, p.lastActivity)
	}
}

func TestProcessEnvelopeOnlyGlobalIdleStopsGatewayNotifications(t *testing.T) {
	events := make([]string, 0, 2)
	p := &ChordProcess{
		key: "ws|wechat|chat",
		onEvent: func(_ string, eventType string, _ ControlState) {
			events = append(events, eventType)
		},
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "agent_started", Payload: json.RawMessage(`{"agent_id":"agent-1"}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "activity", Payload: json.RawMessage(`{"agent_id":"agent-1","type":"streaming"}`)})
	if state := p.State(); !state.Busy {
		t.Fatal("gateway became idle while a SubAgent was still active")
	}
	for _, eventType := range events {
		if eventType == "idle" {
			t.Fatal("gateway routed idle before receiving the global idle envelope")
		}
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "idle", Payload: json.RawMessage(`{"last_outcome":"completed"}`)})
	state := p.State()
	if state.Busy || state.LastOutcome != "completed" {
		t.Fatalf("state after global idle = busy=%v outcome=%q", state.Busy, state.LastOutcome)
	}
	if got := events[len(events)-1]; got != "idle" {
		t.Fatalf("last routed event = %q, want idle", got)
	}
}

func TestProcessEnvelopeHandoffCancelled(t *testing.T) {
	newProc := func(pending *HandoffPayload) (*ChordProcess, *[]string) {
		events := &[]string{}
		p := &ChordProcess{
			key: "ws|wechat|chat",
			onEvent: func(_ string, eventType string, _ ControlState) {
				*events = append(*events, eventType)
			},
		}
		p.state.PendingHandoff = pending
		return p, events
	}
	newPayload := func(requestID string) json.RawMessage {
		data, err := json.Marshal(map[string]string{"request_id": requestID, "reason": "superseded"})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	t.Run("matching request ID clears and notifies", func(t *testing.T) {
		p, events := newProc(&HandoffPayload{RequestID: "handoff-1"})
		p.processEnvelope(&HeadlessEnvelope{Type: "handoff_cancelled", Payload: newPayload("handoff-1")})

		state := p.State()
		if state.PendingHandoff != nil {
			t.Fatalf("pending handoff was not cleared: %#v", state.PendingHandoff)
		}
		if state.ExpiredHandoff == nil || state.ExpiredHandoff.RequestID != "handoff-1" {
			t.Fatalf("cancelled handoff was not recorded: %#v", state.ExpiredHandoff)
		}
		if len(*events) != 1 || (*events)[0] != "handoff_cancelled" {
			t.Fatalf("events = %v, want [handoff_cancelled]", *events)
		}
	})

	t.Run("empty request ID clears and notifies", func(t *testing.T) {
		p, events := newProc(&HandoffPayload{RequestID: "handoff-2"})
		p.processEnvelope(&HeadlessEnvelope{Type: "handoff_cancelled", Payload: newPayload("")})

		state := p.State()
		if state.PendingHandoff != nil {
			t.Fatalf("pending handoff was not cleared: %#v", state.PendingHandoff)
		}
		if state.ExpiredHandoff == nil || state.ExpiredHandoff.RequestID != "handoff-2" {
			t.Fatalf("cancelled handoff was not recorded: %#v", state.ExpiredHandoff)
		}
		if len(*events) != 1 || (*events)[0] != "handoff_cancelled" {
			t.Fatalf("events = %v, want [handoff_cancelled]", *events)
		}
	})

	t.Run("mismatched request ID leaves newer pending", func(t *testing.T) {
		p, events := newProc(&HandoffPayload{RequestID: "handoff-new"})
		p.processEnvelope(&HeadlessEnvelope{Type: "handoff_cancelled", Payload: newPayload("handoff-old")})

		state := p.State()
		if state.PendingHandoff == nil || state.PendingHandoff.RequestID != "handoff-new" {
			t.Fatalf("newer pending handoff was cleared: %#v", state.PendingHandoff)
		}
		if state.ExpiredHandoff != nil {
			t.Fatalf("mismatched event marked the newer handoff expired: %#v", state.ExpiredHandoff)
		}
		if len(*events) != 0 {
			t.Fatalf("events = %v, want none", *events)
		}
	})

	t.Run("no pending handoff is a no-op", func(t *testing.T) {
		p, events := newProc(nil)
		p.processEnvelope(&HeadlessEnvelope{Type: "handoff_cancelled", Payload: newPayload("")})

		if state := p.State(); state.ExpiredHandoff != nil {
			t.Fatalf("no pending handoff should not record expiry: %#v", state.ExpiredHandoff)
		}
		if len(*events) != 0 {
			t.Fatalf("events = %v, want none", *events)
		}
	})
}

func TestProcessEnvelopeCompactionStatusSavesTerminalOutcomeWithoutPush(t *testing.T) {
	events := make([]string, 0, 1)
	p := &ChordProcess{
		key: "ws|wechat|chat",
		onEvent: func(_ string, eventType string, _ ControlState) {
			events = append(events, eventType)
		},
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"skipped","trigger":"model_driven","reason":"projected savings too small"}`)})
	state := p.State()
	if state.LastCompaction == nil {
		t.Fatal("compaction_status must be saved to ControlState")
	}
	if state.LastCompaction.Status != "skipped" || state.LastCompaction.Trigger != "model_driven" {
		t.Fatalf("LastCompaction = %+v, want skipped/model_driven", state.LastCompaction)
	}
	if !strings.Contains(state.LastCompaction.Reason, "projected savings") {
		t.Fatalf("LastCompaction.Reason = %q, want projected savings", state.LastCompaction.Reason)
	}
	// A compaction outcome is never pushed as a chat message.
	if len(events) != 0 {
		t.Fatalf("compaction_status must not push an event, got %v", events)
	}

	// A later terminal outcome replaces the previous one.
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"succeeded","trigger":"model_driven"}`)})
	if got := p.State().LastCompaction.Status; got != "succeeded" {
		t.Fatalf("LastCompaction.Status after replace = %q, want succeeded", got)
	}
}

func TestProcessEnvelopeCompactionStatusIgnoresSyntheticSkipWhileSlotActive(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat"}

	// A real usage-driven compaction owns the slot.
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"started","trigger":"usage_driven","plan_id":"11"}`)})

	// A synthetic started (sync interval/cooldown skip) and its skipped
	// terminal must not overwrite the running plan's state.
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"started","trigger":"model_driven","plan_id":"12","synthetic":true}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"skipped","trigger":"model_driven","plan_id":"12","reason":"minimum 3-request-batch interval"}`)})
	state := p.State()
	if state.LastCompaction == nil || state.LastCompaction.Status != "started" || state.LastCompaction.PlanID != "11" {
		t.Fatalf("LastCompaction after synthetic pair = %+v, want the running plan's started", state.LastCompaction)
	}

	// The running plan's own terminal resolves the slot.
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"succeeded","trigger":"usage_driven","plan_id":"11"}`)})
	if got := p.State().LastCompaction.Status; got != "succeeded" {
		t.Fatalf("LastCompaction.Status after owning terminal = %q, want succeeded", got)
	}
}

func TestProcessEnvelopeCompactionStatusRealTakeoverSupersedesSlot(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat"}

	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"started","trigger":"usage_driven","plan_id":"11"}`)})
	// A real model-driven started takes over the slot...
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"started","trigger":"model_driven","plan_id":"22"}`)})
	// ...so the superseded plan's late terminal must be dropped.
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"succeeded","trigger":"usage_driven","plan_id":"11"}`)})
	state := p.State()
	if state.LastCompaction == nil || state.LastCompaction.PlanID != "22" || state.LastCompaction.Status != "started" {
		t.Fatalf("LastCompaction after superseded terminal = %+v, want plan 22 started", state.LastCompaction)
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"succeeded","trigger":"model_driven","plan_id":"22"}`)})
	if got := p.State().LastCompaction.Status; got != "succeeded" {
		t.Fatalf("LastCompaction.Status after owning terminal = %q, want succeeded", got)
	}
}

func TestProcessEnvelopeCompactionStatusLoneSyntheticSkipShowsOnIdleSlot(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat"}

	// No compaction is running: the synthetic started is ignored, but the
	// skipped terminal still surfaces because the slot is idle.
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"started","trigger":"model_driven","plan_id":"9","synthetic":true}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"skipped","trigger":"model_driven","plan_id":"9","reason":"minimum 3-request-batch interval"}`)})
	state := p.State()
	if state.LastCompaction == nil || state.LastCompaction.Status != "skipped" || state.LastCompaction.PlanID != "9" {
		t.Fatalf("LastCompaction after lone synthetic skip = %+v, want skipped/9", state.LastCompaction)
	}
}

func TestProcessEnvelopeCompactionStatusEmptyPlanIDTerminalKeepsRunningSlot(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat"}

	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"started","trigger":"usage_driven","plan_id":"11"}`)})
	// A terminal without a plan id matches nothing: it must not clear a
	// running plan's state.
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"skipped","trigger":"model_driven","reason":"unknown plan"}`)})
	state := p.State()
	if state.LastCompaction == nil || state.LastCompaction.Status != "started" || state.LastCompaction.PlanID != "11" {
		t.Fatalf("LastCompaction after plan-less terminal = %+v, want the running plan's started", state.LastCompaction)
	}
}

func TestProcessEnvelopeSuppressedIdleStopsStateButSkipsNotification(t *testing.T) {
	events := make([]string, 0, 1)
	p := &ChordProcess{
		key: "ws|wechat|chat",
		onEvent: func(_ string, eventType string, state ControlState) {
			events = append(events, eventType)
			if eventType == "idle" && !state.SuppressUserNotification {
				t.Fatalf("idle state SuppressUserNotification = false, want true")
			}
		},
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "activity", Payload: json.RawMessage(`{"type":"streaming"}`)})
	events = events[:0]
	p.processEnvelope(&HeadlessEnvelope{Type: "idle", Payload: json.RawMessage(`{"last_outcome":"completed","suppress_user_notification":true}`)})

	state := p.State()
	if state.Busy || state.Phase != "" || state.LastOutcome != "completed" {
		t.Fatalf("state after suppressed idle = busy=%v phase=%q outcome=%q", state.Busy, state.Phase, state.LastOutcome)
	}
	if len(events) != 1 || events[0] != "idle" {
		t.Fatalf("events = %v, want [idle]", events)
	}
}

func TestProcessEnvelopePreservesSubAgentMetadata(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat"}
	p.processEnvelope(&HeadlessEnvelope{Type: "agent_started", Payload: json.RawMessage(`{"agent_id":"agent-1","task_id":"adhoc-1","agent_type":"reviewer","description":"Review changes","parent_agent_id":"main"}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "agent_notify", Payload: json.RawMessage(`{"agent_id":"agent-1","task_id":"adhoc-1","agent_type":"reviewer","kind":"blocked","subtype":"stall_resolved","message":"Tests pass","parent_agent_id":"main","target_agent_id":"main"}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "assistant_message", Payload: json.RawMessage(`{"text":"Reviewed","agent_id":"agent-1","task_id":"adhoc-1","agent_type":"reviewer","parent_agent_id":"main","tool_calls":1}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "agent_done", Payload: json.RawMessage(`{"agent_id":"agent-1","task_id":"adhoc-1","agent_type":"reviewer","summary":"Done","parent_agent_id":"main"}`)})

	state := p.State()
	if state.LastAgentStarted == nil || state.LastAgentStarted.Description != "Review changes" {
		t.Fatalf("LastAgentStarted = %#v", state.LastAgentStarted)
	}
	if state.LastAgentNotify == nil || state.LastAgentNotify.Kind != "blocked" || state.LastAgentNotify.Subtype != "stall_resolved" || state.LastAgentNotify.Message != "Tests pass" || state.LastAgentNotify.TargetAgentID != "main" {
		t.Fatalf("LastAgentNotify = %#v", state.LastAgentNotify)
	}
	if state.LastAssistantAgentID != "agent-1" || state.LastAssistantTaskID != "adhoc-1" || state.LastAssistantAgentType != "reviewer" || state.LastAssistantParentAgentID != "main" {
		t.Fatalf("assistant metadata = agent=%q task=%q type=%q parent=%q", state.LastAssistantAgentID, state.LastAssistantTaskID, state.LastAssistantAgentType, state.LastAssistantParentAgentID)
	}
	if state.LastAgentDone == nil || state.LastAgentDone.Summary != "Done" {
		t.Fatalf("LastAgentDone = %#v", state.LastAgentDone)
	}
}

func TestProcessEnvelopeEmitsSubAgentLifecycleCallbacks(t *testing.T) {
	var got []string
	p := &ChordProcess{
		key: "ws|wechat|chat",
		onEvent: func(_ string, eventType string, _ ControlState) {
			got = append(got, eventType)
		},
	}
	for _, env := range []HeadlessEnvelope{
		{Type: "agent_started", Payload: json.RawMessage(`{"agent_id":"agent-1","task_id":"adhoc-1"}`)},
		{Type: "agent_notify", Payload: json.RawMessage(`{"agent_id":"agent-1","task_id":"adhoc-1","message":"working"}`)},
		{Type: "agent_done", Payload: json.RawMessage(`{"agent_id":"agent-1","task_id":"adhoc-1","summary":"done"}`)},
	} {
		p.processEnvelope(&env)
	}
	if strings.Join(got, ",") != "agent_started,agent_notify,agent_done" {
		t.Fatalf("callbacks = %v", got)
	}
}

func TestProcessEnvelopeLogsOutsideProcessLock(t *testing.T) {
	logStarted := make(chan struct{})
	releaseLog := make(chan struct{})
	p := &ChordProcess{
		key: "ws|wechat|chat",
		eventLogf: func(string, ...any) {
			close(logStarted)
			<-releaseLog
		},
	}
	done := make(chan struct{})
	go func() {
		p.processEnvelope(&HeadlessEnvelope{Type: "agent_done"})
		close(done)
	}()
	<-logStarted

	stateRead := make(chan struct{})
	go func() {
		_ = p.State()
		close(stateRead)
	}()
	select {
	case <-stateRead:
	case <-time.After(time.Second):
		t.Fatal("State blocked while event log was being written")
	}
	close(releaseLog)
	<-done
}

func TestCollectIdleProcessesDoesNotHoldManagerLockWhileWaitingForProcess(t *testing.T) {
	blocked := &ChordProcess{key: "blocked", lastActivity: time.Now()}
	available := &ChordProcess{key: "available", lastActivity: time.Now()}
	mgr := &ChordManager{procs: map[string]*ChordProcess{
		blocked.key:   blocked,
		available.key: available,
	}}
	blocked.mu.Lock()
	scanDone := make(chan struct{})
	go func() {
		mgr.collectIdleProcesses(time.Hour)
		close(scanDone)
	}()
	time.Sleep(20 * time.Millisecond)

	lookupDone := make(chan *ChordProcess, 1)
	go func() { lookupDone <- mgr.GetProcessForKey(available.key) }()
	select {
	case got := <-lookupDone:
		if got != available {
			t.Fatalf("lookup = %p, want %p", got, available)
		}
	case <-time.After(time.Second):
		t.Fatal("process lookup blocked behind idle process state lock")
	}
	blocked.mu.Unlock()
	<-scanDone
}

func TestCollectIdleProcessesDoesNotRemoveReplacement(t *testing.T) {
	key := "key"
	oldProcess := &ChordProcess{key: key, lastActivity: time.Now().Add(-time.Hour)}
	newProcess := &ChordProcess{key: key, lastActivity: time.Now()}
	mgr := &ChordManager{procs: map[string]*ChordProcess{key: oldProcess}}
	oldProcess.mu.Lock()
	done := make(chan []*ChordProcess, 1)
	go func() { done <- mgr.collectIdleProcesses(time.Minute) }()
	time.Sleep(20 * time.Millisecond)
	mgr.mu.Lock()
	mgr.procs[key] = newProcess
	mgr.mu.Unlock()
	oldProcess.mu.Unlock()
	idle := <-done
	if len(idle) != 0 {
		t.Fatalf("replaced process should not be returned for termination: %v", idle)
	}
	if got := mgr.GetProcessForKey(key); got != newProcess {
		t.Fatalf("replacement = %p, want %p", got, newProcess)
	}
}

func TestCollectIdleProcessesKeepsProcessThatBecomesActive(t *testing.T) {
	key := "key"
	p := &ChordProcess{key: key, lastActivity: time.Now().Add(-time.Hour)}
	mgr := &ChordManager{procs: map[string]*ChordProcess{key: p}}
	p.mu.Lock()
	done := make(chan []*ChordProcess, 1)
	go func() { done <- mgr.collectIdleProcesses(time.Minute) }()
	time.Sleep(20 * time.Millisecond)
	p.lastActivity = time.Now()
	p.mu.Unlock()
	if idle := <-done; len(idle) != 0 {
		t.Fatalf("newly active process classified as idle: %v", idle)
	}
	if got := mgr.GetProcessForKey(key); got != p {
		t.Fatalf("active process removed: got %p, want %p", got, p)
	}
}

func BenchmarkChordManagerGetProcessForKeyParallel(b *testing.B) {
	mgr := &ChordManager{procs: map[string]*ChordProcess{"key": {key: "key"}}}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = mgr.GetProcessForKey("key")
		}
	})
}

func BenchmarkChordProcessStateParallel(b *testing.B) {
	p := &ChordProcess{state: ControlState{SessionID: "session", Busy: true, Phase: "working"}}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = p.State()
		}
	})
}

func BenchmarkChordProcessAliveParallel(b *testing.B) {
	p := &ChordProcess{cmd: &exec.Cmd{Process: &os.Process{Pid: 1}}}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = p.Alive()
		}
	})
}

func TestChordProcessReadMethodsShareStateLock(t *testing.T) {
	p := &ChordProcess{cmd: &exec.Cmd{Process: &os.Process{Pid: 1}}}
	p.mu.RLock()
	defer p.mu.RUnlock()

	stateDone := make(chan struct{})
	go func() {
		_ = p.State()
		close(stateDone)
	}()
	aliveDone := make(chan struct{})
	go func() {
		_ = p.Alive()
		close(aliveDone)
	}()
	for name, done := range map[string]<-chan struct{}{"State": stateDone, "Alive": aliveDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s blocked behind another reader", name)
		}
	}
}

func TestChordManagerProcessLookupAndStop(t *testing.T) {
	cfg := &config.Config{Workspaces: []config.Workspace{{ID: "ws1", Path: t.TempDir()}, {ID: "ws2", Path: t.TempDir()}}}
	mgr := &ChordManager{procs: make(map[string]*ChordProcess)}
	mgr.cfg.Store(cfg)

	key1 := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	key2 := (processKey{workspaceID: "ws2", imType: "feishu", chatID: "chat-2"}).String()
	p1 := &ChordProcess{key: key1, workspaceID: "ws1", stdin: &captureWriteCloser{}}
	p2 := &ChordProcess{key: key2, workspaceID: "ws2", stdin: &captureWriteCloser{}}
	mgr.procs[key1] = p1
	mgr.procs[key2] = p2

	if got := mgr.GetProcessForKey(key1); got != p1 {
		t.Fatalf("GetProcessForKey key1 = %#v, want p1", got)
	}
	if got := mgr.GetProcessForKey("missing"); got != nil {
		t.Fatalf("GetProcessForKey missing = %#v, want nil", got)
	}

	mgr.StopProcessKey("missing")
	if got := len(mgr.procs); got != 2 {
		t.Fatalf("StopProcessKey missing changed map size to %d", got)
	}

	mgr.StopProcessKey(key1)
	if _, ok := mgr.procs[key1]; ok {
		t.Fatal("StopProcessKey did not remove key1")
	}
	if !p1.stoppedByGateway {
		t.Fatal("StopProcessKey should mark process stopped by gateway")
	}

	mgr.StopProcessKey(key2)
	if _, ok := mgr.procs[key2]; ok {
		t.Fatal("StopProcessKey did not remove key2")
	}
	if !p2.stoppedByGateway {
		t.Fatal("StopProcessKey should mark process stopped by gateway")
	}
}

func TestChordManagerStopAllPermanentlyRejectsSpawn(t *testing.T) {
	mgr := &ChordManager{procs: make(map[string]*ChordProcess)}
	mgr.StopAll(0)
	key := (processKey{workspaceID: "ws", imType: "wechat", chatID: "chat"}).String()
	if _, err := mgr.GetOrSpawnForKey(key); !errors.Is(err, ErrManagerShuttingDown) {
		t.Fatalf("get or spawn after StopAll error = %v, want ErrManagerShuttingDown", err)
	}
	if _, err := mgr.SpawnWithArgsForKey(key); !errors.Is(err, ErrManagerShuttingDown) {
		t.Fatalf("spawn after StopAll error = %v, want ErrManagerShuttingDown", err)
	}
}

func TestStopAllClosesShutdownSignalOnce(t *testing.T) {
	mgr := &ChordManager{shutdownCh: make(chan struct{}), procs: make(map[string]*ChordProcess)}
	mgr.StopAll(0)
	mgr.StopAll(0)
	select {
	case <-mgr.shutdownSignal():
	default:
		t.Fatal("shutdown signal was not closed after StopAll")
	}
}

func TestStopAllWithoutShutdownChannelIsSafe(t *testing.T) {
	mgr := &ChordManager{}
	mgr.StopAll(0)
	mgr.StopAll(0)
	if mgr.shutdownSignal() != nil {
		t.Fatal("struct-literal manager should not report a shutdown signal")
	}
}

func TestStopAllCancelsAutoRestartWait(t *testing.T) {
	mgr := &ChordManager{shutdownCh: make(chan struct{})}
	p := &ChordProcess{mgr: mgr}
	done := make(chan bool, 1)
	go func() { done <- p.waitAutoRestart(time.Hour) }()

	mgr.StopAll(0)

	select {
	case restarted := <-done:
		if restarted {
			t.Fatal("waitAutoRestart returned true after StopAll")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitAutoRestart did not observe manager shutdown")
	}
}

func TestAutoRestartWaitElapsesWhenManagerActive(t *testing.T) {
	for name, mgr := range map[string]*ChordManager{
		"open channel": {shutdownCh: make(chan struct{})},
		"nil channel":  {},
		"nil manager":  nil,
	} {
		p := &ChordProcess{mgr: mgr}
		if !p.waitAutoRestart(10 * time.Millisecond) {
			t.Fatalf("%s: waitAutoRestart = false, want the delay to elapse", name)
		}
	}
}

func TestIdleCheckLoopStopsOnShutdown(t *testing.T) {
	mgr := &ChordManager{shutdownCh: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		mgr.IdleCheckLoop()
		close(done)
	}()

	mgr.StopAll(0)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("IdleCheckLoop did not exit after StopAll")
	}
}

func TestChordManagerLifecycleLocksAreScopedAndReleased(t *testing.T) {
	mgr := &ChordManager{}
	key1 := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	key2 := (processKey{workspaceID: "ws2", imType: "wechat", chatID: "chat-2"}).String()

	releaseKey1 := mgr.acquireKeyLock(key1)
	if got := lifecycleKeyLockCount(mgr); got != 1 {
		t.Fatalf("lifecycle lock count = %d, want 1", got)
	}
	releaseKey2 := mgr.acquireKeyLock(key2)
	if got := lifecycleKeyLockCount(mgr); got != 2 {
		t.Fatalf("lifecycle lock count = %d, want 2", got)
	}
	releaseKey2()
	releaseKey1()
	if got := lifecycleKeyLockCount(mgr); got != 0 {
		t.Fatalf("released lifecycle locks retained: %d", got)
	}
}

func TestChordManagerStopWaitsForSameKeyLifecycleOperation(t *testing.T) {
	mgr := &ChordManager{procs: make(map[string]*ChordProcess)}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	p := &ChordProcess{key: key, stdin: &captureWriteCloser{}}
	mgr.procs[key] = p

	releaseKeyLock := mgr.acquireKeyLock(key)
	stopped := make(chan struct{})
	go func() {
		mgr.StopProcessKey(key)
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("StopProcessKey completed during another lifecycle operation for the same key")
	case <-time.After(20 * time.Millisecond):
	}
	if got := mgr.GetProcessForKey(key); got != p {
		t.Fatalf("process changed while lifecycle lock was held: got %#v, want %#v", got, p)
	}

	releaseKeyLock()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("StopProcessKey did not complete after lifecycle operation released the key")
	}
	if got := mgr.GetProcessForKey(key); got != nil {
		t.Fatalf("process remains after StopProcessKey: %#v", got)
	}
	if got := lifecycleKeyLockCount(mgr); got != 0 {
		t.Fatalf("released lifecycle lock retained: %d", got)
	}
}

func TestChordManagerDifferentKeyLifecycleOperationsRemainParallel(t *testing.T) {
	mgr := &ChordManager{procs: make(map[string]*ChordProcess)}
	key1 := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	key2 := (processKey{workspaceID: "ws2", imType: "wechat", chatID: "chat-2"}).String()
	mgr.procs[key2] = &ChordProcess{key: key2, stdin: &captureWriteCloser{}}

	releaseKey1 := mgr.acquireKeyLock(key1)
	defer releaseKey1()

	stopped := make(chan struct{})
	go func() {
		mgr.StopProcessKey(key2)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("operation for a different process key was unnecessarily serialized")
	}
}

func lifecycleKeyLockCount(mgr *ChordManager) int {
	mgr.keyLocksMu.Lock()
	defer mgr.keyLocksMu.Unlock()
	return len(mgr.keyLocks)
}

func TestChordManagerGetOrSpawnForKeyMissingWorkspace(t *testing.T) {
	mgr := &ChordManager{procs: make(map[string]*ChordProcess)}
	mgr.cfg.Store(&config.Config{})
	key := (processKey{workspaceID: "missing", imType: "wechat", chatID: "chat"}).String()
	p, err := mgr.GetOrSpawnForKey(key)
	if err != nil {
		t.Fatalf("GetOrSpawnForKey error = %v", err)
	}
	if p != nil {
		t.Fatalf("GetOrSpawnForKey missing = %#v, want nil", p)
	}
}

func TestChordManagerGetOrSpawnForKeyReplacesExitedProcess(t *testing.T) {
	chordBinary := makeFakeChordBinary(t, "")
	workspaceDir := t.TempDir()
	cfg := &config.Config{
		ChordPath:  chordBinary,
		Workspaces: []config.Workspace{{ID: "ws1", Path: workspaceDir}},
	}
	mgr := newTestChordManager(cfg)
	key := (processKey{workspaceID: "ws1", imType: "feishu", chatID: "chat-1"}).String()
	dead := &ChordProcess{key: key, workspaceID: "ws1"}
	mgr.procs[key] = dead

	p, err := mgr.GetOrSpawnForKey(key)
	if err != nil {
		t.Fatalf("GetOrSpawnForKey error = %v", err)
	}
	defer mgr.StopAll(time.Millisecond)
	if p == nil || p == dead {
		t.Fatalf("GetOrSpawnForKey returned %#v, want fresh process", p)
	}
	if got := mgr.GetProcessForKey(key); got != p {
		t.Fatalf("process map = %#v, want fresh process %#v", got, p)
	}
}

func TestChordManagerConcurrentGetOrSpawnForKeyReturnsSameProcess(t *testing.T) {
	chordBinary := makeFakeChordBinary(t, "")
	cfg := &config.Config{
		ChordPath:  chordBinary,
		Workspaces: []config.Workspace{{ID: "ws1", Path: t.TempDir()}},
	}
	mgr := newTestChordManager(cfg)
	key := (processKey{workspaceID: "ws1", imType: "feishu", chatID: "chat-1"}).String()
	releaseKeyLock := mgr.acquireKeyLock(key)

	type result struct {
		process *ChordProcess
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			process, err := mgr.GetOrSpawnForKey(key)
			results <- result{process: process, err: err}
		}()
	}
	waitForLifecycleKeyLockRefs(t, mgr, key, 3)
	releaseKeyLock()

	first := <-results
	second := <-results
	defer mgr.StopAll(time.Millisecond)
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent GetOrSpawnForKey errors = (%v, %v)", first.err, second.err)
	}
	if first.process == nil || first.process != second.process {
		t.Fatalf("concurrent processes = (%p, %p), want the same non-nil process", first.process, second.process)
	}
	if got := mgr.GetProcessForKey(key); got != first.process {
		t.Fatalf("managed process = %p, want %p", got, first.process)
	}
}

func waitForLifecycleKeyLockRefs(t *testing.T, mgr *ChordManager, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mgr.keyLocksMu.Lock()
		refs := 0
		if lock := mgr.keyLocks[key]; lock != nil {
			refs = lock.refs
		}
		mgr.keyLocksMu.Unlock()
		if refs == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("lifecycle key lock refs did not reach %d", want)
}

func TestProcessKeyRoundTripOpaqueParts(t *testing.T) {
	key := processKey{workspaceID: "ws|1", imType: "wechat", chatID: "chat|room|42"}.String()
	workspaceID, imType, chatID := parseProcessKey(key)
	if workspaceID != "ws|1" || imType != "wechat" || chatID != "chat|room|42" {
		t.Fatalf("parseProcessKey(%q) = (%q, %q, %q)", key, workspaceID, imType, chatID)
	}
}

func TestParseProcessKeyLegacyFormat(t *testing.T) {
	workspaceID, imType, chatID := parseProcessKey("ws1|feishu|chat-1")
	if workspaceID != "ws1" || imType != "feishu" || chatID != "chat-1" {
		t.Fatalf("legacy parse = (%q, %q, %q)", workspaceID, imType, chatID)
	}
}

func TestChordManagerSpawnArgsForPinnedSession(t *testing.T) {
	tmp := t.TempDir()
	pins := newSessionPinStore(tmp)
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	if err := pins.Set(key, " sess-123 "); err != nil {
		t.Fatalf("pin session: %v", err)
	}
	mgr := &ChordManager{pins: pins}
	got := mgr.spawnArgsForKey(key)
	if len(got) != 2 || got[0] != "--resume" || got[1] != "sess-123" {
		t.Fatalf("spawnArgsForKey = %v", got)
	}
}

func TestChordManagerSpawnArgsForLegacyPinnedSession(t *testing.T) {
	tmp := t.TempDir()
	pins := newSessionPinStore(tmp)
	legacyKey := legacyProcessKeyString("ws1", "wechat", "chat-1")
	if err := pins.Set(legacyKey, "sess-legacy"); err != nil {
		t.Fatalf("pin legacy session: %v", err)
	}
	mgr := &ChordManager{pins: pins}
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	got := mgr.spawnArgsForKey(key)
	if len(got) != 2 || got[0] != "--resume" || got[1] != "sess-legacy" {
		t.Fatalf("spawnArgsForKey legacy = %v", got)
	}
}

func TestNewChordManagerUsesSessionPinOverride(t *testing.T) {
	tmp := t.TempDir()
	pinsPath := filepath.Join(tmp, "custom-pins.json")
	paths := &config.Paths{StateDir: filepath.Join(tmp, "state")}
	mgr := NewChordManager(&config.Config{SessionPinsFile: pinsPath}, paths)
	if mgr.pins == nil || mgr.pins.path != pinsPath {
		t.Fatalf("pins path = %#v, want %q", mgr.pins, pinsPath)
	}
}

func TestChordProcessSendCommandClosedPipe(t *testing.T) {
	p := &ChordProcess{}
	if err := p.SendCommand(map[string]any{"type": "status"}); err != io.ErrClosedPipe {
		t.Fatalf("SendCommand closed pipe err = %v, want %v", err, io.ErrClosedPipe)
	}
}

func TestChordProcessLocalShellResultEndsTurn(t *testing.T) {
	payload, err := json.Marshal(LocalShellPayload{Command: "pwd", Output: "/tmp", Failed: false})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	p := &ChordProcess{
		state: ControlState{
			Busy:           true,
			Phase:          "local_shell",
			PhaseDetail:    "pwd",
			PendingConfirm: &ConfirmPayload{RequestID: "confirm-1"},
			LastError:      "previous error",
		},
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "local_shell_result", Payload: payload})
	state := p.State()

	if state.Busy {
		t.Fatal("local_shell_result should mark process idle")
	}
	if state.LastLocalShell == nil || state.LastLocalShell.Command != "pwd" || state.LastLocalShell.Output != "/tmp" {
		t.Fatalf("LastLocalShell = %#v", state.LastLocalShell)
	}
	if state.PendingConfirm == nil || state.PendingConfirm.RequestID != "confirm-1" {
		t.Fatalf("PendingConfirm = %#v, want preserved", state.PendingConfirm)
	}
	if state.LastError != "previous error" {
		t.Fatalf("LastError = %q, want preserved", state.LastError)
	}
}

func TestChordProcessTerminateGroupNilAndNoProcess(t *testing.T) {
	var nilProcess *ChordProcess
	nilProcess.TerminateGroup(time.Millisecond)

	p := &ChordProcess{stdin: &captureWriteCloser{}}
	p.TerminateGroup(time.Millisecond)
	if !p.stoppedByGateway {
		t.Fatal("TerminateGroup should mark no-process instance stopped by gateway")
	}
}

// newTestPinStore returns a pin store that persists in memory only.
func newTestPinStore() *sessionPinStore {
	return &sessionPinStore{
		pins:   make(map[string]string),
		writer: func(string, []byte, os.FileMode) error { return nil },
	}
}

func TestProcessEnvelopeSessionSwitchedFollowsActiveSession(t *testing.T) {
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	pins := newTestPinStore()
	if err := pins.Set(key, "old-session"); err != nil {
		t.Fatalf("seed pin: %v", err)
	}
	events := make([]string, 0, 1)
	p := &ChordProcess{
		key: key,
		mgr: &ChordManager{pins: pins},
		onEvent: func(_ string, eventType string, _ ControlState) {
			events = append(events, eventType)
		},
	}
	p.state.SessionID = "old-session"

	p.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Payload: json.RawMessage(`{"session_id":"new-session"}`)})

	if got := p.State().SessionID; got != "new-session" {
		t.Fatalf("SessionID = %q, want new-session", got)
	}
	// The pin is otherwise written only by the ready envelope, so a pinned
	// binding would keep resuming the session the switch abandoned.
	if got := pins.Get(key); got != "new-session" {
		t.Fatalf("pin = %q, want new-session", got)
	}
	// The command that caused the switch already answered the user; a switch
	// must not add a second chat message.
	if len(events) != 0 {
		t.Fatalf("session_switched must not push an event, got %v", events)
	}
}

func TestProcessEnvelopeSessionSwitchedRepinsClearedBinding(t *testing.T) {
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	pins := newTestPinStore()
	p := &ChordProcess{key: key, mgr: &ChordManager{pins: pins}}
	p.state.SessionID = "old-session"

	p.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Payload: json.RawMessage(`{"session_id":"new-session"}`)})

	if got := p.State().SessionID; got != "new-session" {
		t.Fatalf("SessionID = %q, want new-session", got)
	}
	// Chord only reports a switch that actually happened, so the pin is
	// re-pointed at the live session even when it was empty (for example
	// after /new): later spawns must resume the session the runtime runs.
	if got := pins.Get(key); got != "new-session" {
		t.Fatalf("pin = %q, want new-session", got)
	}
}

func TestProcessEnvelopeSessionSwitchedIgnoresBlankID(t *testing.T) {
	pins := newTestPinStore()
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	if err := pins.Set(key, "old-session"); err != nil {
		t.Fatalf("seed pin: %v", err)
	}
	p := &ChordProcess{key: key, mgr: &ChordManager{pins: pins}}
	p.state.SessionID = "old-session"

	p.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Payload: json.RawMessage(`{"session_id":"  "}`)})

	if got := p.State().SessionID; got != "old-session" {
		t.Fatalf("SessionID = %q, want the previous session kept", got)
	}
	if got := pins.Get(key); got != "old-session" {
		t.Fatalf("pin = %q, want the previous pin kept", got)
	}
}

func TestProcessEnvelopeSessionSwitchedClearsCompactionState(t *testing.T) {
	pins := newTestPinStore()
	key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
	if err := pins.Set(key, "old-session"); err != nil {
		t.Fatalf("seed pin: %v", err)
	}
	p := &ChordProcess{key: key, mgr: &ChordManager{pins: pins}}
	p.state.SessionID = "old-session"

	// The old session ran a compaction that owns the slot.
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"started","trigger":"usage_driven","plan_id":"11"}`)})
	if p.State().LastCompaction == nil {
		t.Fatal("seed compaction state was not recorded")
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Payload: json.RawMessage(`{"session_id":"new-session"}`)})

	state := p.State()
	if state.LastCompaction != nil {
		t.Fatalf("LastCompaction after switch = %+v, want nil (it belongs to the abandoned session)", state.LastCompaction)
	}
	// The new session starts its own compaction; the old plan's late terminal
	// must not overwrite it.
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"started","trigger":"usage_driven","plan_id":"22"}`)})
	p.processEnvelope(&HeadlessEnvelope{Type: "compaction_status", Payload: json.RawMessage(`{"status":"succeeded","trigger":"usage_driven","plan_id":"11"}`)})
	if got := p.State().LastCompaction; got == nil || got.Status != "started" || got.PlanID != "22" {
		t.Fatalf("LastCompaction after old plan's late terminal = %+v, want the new plan's started", got)
	}
}

func TestProcessEnvelopeBackgroundResultAndContextNotice(t *testing.T) {
	events := make([]string, 0, 2)
	p := &ChordProcess{
		key: "ws|wechat|chat",
		onEvent: func(_ string, eventType string, _ ControlState) {
			events = append(events, eventType)
		},
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "background_result", Payload: json.RawMessage(`{"target_agent_id":"agent-1","content":"job finished"}`)})
	state := p.State()
	if state.LastBackgroundResult == nil {
		t.Fatal("background_result must be saved to ControlState")
	}
	if state.LastBackgroundResult.Content != "job finished" || state.LastBackgroundResult.TargetAgentID != "agent-1" {
		t.Fatalf("LastBackgroundResult = %+v", state.LastBackgroundResult)
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "context_notice", Payload: json.RawMessage(`{"level":"imminent","message":"Context will be compacted"}`)})
	state = p.State()
	if state.LastContextNotice == nil {
		t.Fatal("context_notice must be saved to ControlState")
	}
	if state.LastContextNotice.Level != "imminent" || state.LastContextNotice.Message != "Context will be compacted" {
		t.Fatalf("LastContextNotice = %+v", state.LastContextNotice)
	}

	want := []string{"background_result", "context_notice"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", events, want)
	}
}
