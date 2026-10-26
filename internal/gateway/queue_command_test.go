package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func TestParseQueueRequest(t *testing.T) {
	valid := []struct {
		rest   string
		action string
		index  int
		text   string
	}{
		{"", "list", 0, ""},
		{"list", "list", 0, ""},
		{"pause", "pause", 0, ""},
		{"RESUME", "resume", 0, ""},
		{"clear", "clear", 0, ""},
		{"show 3", "show", 3, ""},
		{"delete 2", "delete", 2, ""},
		{"drop 2", "delete", 2, ""},
		{"rm 2", "delete", 2, ""},
		{"edit 7 fix the typo", "edit", 7, "fix the typo"},
		{"after run the tests", "after", 0, "run the tests"},
		// Only the action is lowercased; the queued text keeps its case.
		{"edit 1 Keep This Case", "edit", 1, "Keep This Case"},
	}
	for _, tc := range valid {
		request, ok := parseQueueRequest(tc.rest)
		if !ok {
			t.Errorf("parseQueueRequest(%q) rejected", tc.rest)
			continue
		}
		if request.action != tc.action || request.index != tc.index || request.text != tc.text {
			t.Errorf("parseQueueRequest(%q) = %+v, want %s/%d/%q", tc.rest, request, tc.action, tc.index, tc.text)
		}
	}

	// Every one of these would otherwise be applied to the wrong item or to
	// nothing at all, so they must not parse.
	invalid := []string{
		"nonsense", "show", "show 0", "show -1", "show two", "show 2 extra",
		"delete", "delete 1 extra", "edit 2", "edit 2   ", "after", "pause now",
		"list everything",
	}
	for _, rest := range invalid {
		if _, ok := parseQueueRequest(rest); ok {
			t.Errorf("parseQueueRequest(%q) accepted", rest)
		}
	}
}

func TestFormatInputQueueNumbersItems(t *testing.T) {
	queue := &InputQueue{
		InputQueueTarget: InputQueueTarget{RuntimeID: "runtime", SessionID: "session", SessionEpoch: 1, OwnerTaskID: "main"},
		ViewVersion:      4,
		QueueRevision:    9,
		Gate:             InputQueueGate{Mode: "paused", Reason: "user_paused"},
		Inputs: []InputQueueItem{
			{InputID: "a", Revision: 1, Sequence: 1, Policy: "next_request", Stage: "queued", Preview: "rename the flag"},
			{InputID: "b", Revision: 2, Sequence: 2, Policy: "after_completion", Stage: "queued", Preview: "run the suite", Attachments: 2},
			{InputID: "c", Revision: 1, Sequence: 3, Policy: "after_completion", Stage: "queued", Preview: "held draft", EditHeld: true},
		},
	}
	text := formatInputQueue(queue)
	for _, want := range []string{
		"3 pending", "paused", "user_paused",
		"1. [next_request, queued] rename the flag",
		"2. [after_completion, queued] run the suite (2 attachment(s))",
		"3. [after_completion, queued] held draft (locked for editing in the local TUI)",
		"/queue show <n>", "/queue delete <n>", "/queue resume",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("formatInputQueue missing %q in:\n%s", want, text)
		}
	}

	// The listing stops at the limit instead of sending 128 rows to a phone.
	many := &InputQueue{ViewVersion: 1, Gate: InputQueueGate{Mode: "running"}}
	for i := 0; i < 14; i++ {
		many.Inputs = append(many.Inputs, InputQueueItem{InputID: string(rune('a' + i)), Revision: 1, Sequence: int64(i + 1), Policy: "next_request", Stage: "queued", Preview: "item"})
	}
	text = formatInputQueue(many)
	if !strings.Contains(text, "14 pending") || !strings.Contains(text, "… and 4 more") {
		t.Fatalf("formatInputQueue limit = %q", text)
	}
	if strings.Contains(text, "11. [") {
		t.Fatalf("formatInputQueue listed past the limit: %q", text)
	}

	blocked := formatInputQueue(&InputQueue{ViewVersion: 1, Gate: InputQueueGate{Mode: "running"}, Blocked: "storage\noffline"})
	if !strings.Contains(blocked, "storage offline") {
		t.Fatalf("blocked queue must stay on one line: %q", blocked)
	}
}

type queueHarness struct {
	r      *NotificationRouter
	proc   *ChordProcess
	sender *stubIMAdapter
	stdin  *captureWriteCloser
	ws     *config.Workspace
}

func newQueueHarness(t *testing.T) *queueHarness {
	t.Helper()
	r, proc, sender, stdin, ws := newModelsTestRouter()
	// The target Chord advertises embeds the runtime identity, so the process
	// must look like a live one for the projection to be accepted.
	proc.runtimeID = "runtime-1"
	proc.sessionEpoch = 3
	proc.state.SessionID = "session-1"
	// Managed processes always run with --gateway-instance, and Chord refuses a
	// queue command that does not echo it back.
	proc.bindingInstanceID = "binding-1"
	return &queueHarness{r: r, proc: proc, sender: sender, stdin: stdin, ws: ws}
}

// chordRejectsQueueCommand mirrors the identity check Chord applies before it
// acts on an input_queue command. Answering from this instead of a canned result
// keeps every command test honest about the wire contract.
func (h *queueHarness) chordRejectsQueueCommand() bool {
	line := ""
	for _, candidate := range strings.Split(h.stdin.String(), "\n") {
		if strings.Contains(candidate, `"type":"input_queue"`) {
			line = candidate
		}
	}
	if line == "" {
		return false
	}
	var cmd struct {
		BindingID string `json:"binding_id"`
	}
	if json.Unmarshal([]byte(line), &cmd) != nil {
		return true
	}
	return cmd.BindingID != h.proc.bindingInstanceID
}

func testQueue(revision uint64, items ...InputQueueItem) *InputQueue {
	return &InputQueue{
		InputQueueTarget: InputQueueTarget{RuntimeID: "runtime-1", SessionID: "session-1", SessionEpoch: 3, OwnerTaskID: "main"},
		ViewVersion:      revision,
		ViewVersionCAS:   true,
		QueueRevision:    revision,
		Gate:             InputQueueGate{Mode: "running"},
		Inputs:           items,
	}
}

// pendingQueueOperation reports the operation_id a caller is currently waiting
// on. Chord echoes it back, so a test responder can answer without parsing the
// command it just received.
func (h *queueHarness) pendingQueueOperation() string {
	h.proc.mu.Lock()
	defer h.proc.mu.Unlock()
	for id := range h.proc.queueWaiters.waiters {
		return id
	}
	return ""
}

func (h *queueHarness) deliverStatus(queue *InputQueue) {
	payload, err := json.Marshal(struct {
		SessionID  string      `json:"session_id"`
		InputQueue *InputQueue `json:"input_queue"`
	}{SessionID: "session-1", InputQueue: queue})
	if err != nil {
		panic(err)
	}
	h.proc.processEnvelope(&HeadlessEnvelope{Type: "status_response", Payload: payload})
}

// answerQueue plays Chord's side: it serves the status refresh the handler
// starts with, then answers the input_queue command that follows.
func (h *queueHarness) answerQueue(queue *InputQueue, result InputQueueResult) {
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		statusSent := false
		for time.Now().Before(deadline) {
			if !statusSent && strings.Contains(h.stdin.String(), `"type":"status"`) {
				statusSent = true
				h.deliverStatus(queue)
				continue
			}
			if id := h.pendingQueueOperation(); id != "" {
				result.OperationID = id
				if h.chordRejectsQueueCommand() {
					result = InputQueueResult{OperationID: id, Result: inputQueueRejected, Reason: "Input queue binding changed; refresh its state"}
				}
				payload, err := json.Marshal(result)
				if err != nil {
					panic(err)
				}
				h.proc.processEnvelope(&HeadlessEnvelope{Type: "input_queue_result", Payload: payload})
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

func TestQueueCommandRoundTrip(t *testing.T) {
	items := []InputQueueItem{
		{InputID: "input-a", Revision: 4, Sequence: 1, Policy: "next_request", Stage: "queued", Preview: "rename the flag"},
		{InputID: "input-b", Revision: 7, Sequence: 2, Policy: "after_completion", Stage: "queued", Preview: "run the suite"},
	}
	cases := []struct {
		name     string
		command  string
		wantStd  []string
		result   InputQueueResult
		wantText string
	}{
		{
			name:     "list",
			command:  "/queue",
			wantStd:  []string{`"type":"status"`},
			wantText: "2 pending",
		},
		{
			name:    "show",
			command: "/queue show 2",
			wantStd: []string{`"type":"input_queue"`, `"action":"get"`, `"input_id":"input-b"`},
			result:  InputQueueResult{Result: inputQueueCommitted, InputID: "input-b", Content: "run the suite", Attachments: 1},
			// Chord's get reply omits expected_revision because it only reads.
			wantText: "2. run the suite",
		},
		{
			name:     "edit",
			command:  "/queue edit 1 rename the flag please",
			wantStd:  []string{`"type":"input_queue"`, `"action":"update"`, `"input_id":"input-a"`, `"expected_revision":4`, `"content":"rename the flag please"`},
			result:   InputQueueResult{Result: inputQueueCommitted, InputID: "input-a"},
			wantText: "Replaced the text of queued input #1",
		},
		{
			name:     "delete",
			command:  "/queue delete 2",
			wantStd:  []string{`"action":"delete"`, `"input_id":"input-b"`, `"expected_revision":7`},
			result:   InputQueueResult{Result: inputQueueCommitted, InputID: "input-b"},
			wantText: "Dropped queued input #2",
		},
		{
			name:     "pause",
			command:  "/queue pause",
			wantStd:  []string{`"type":"input_queue"`, `"action":"pause"`, `"expected_queue_revision":9`},
			result:   InputQueueResult{Result: inputQueueCommitted},
			wantText: "Paused the input queue",
		},
		{
			name:     "resume",
			command:  "/queue resume",
			wantStd:  []string{`"action":"resume"`, `"expected_queue_revision":9`},
			result:   InputQueueResult{Result: inputQueueCommitted},
			wantText: "Resumed the input queue",
		},
		{
			name:     "clear",
			command:  "/queue clear",
			wantStd:  []string{`"action":"clear"`, `"expected_queue_revision":9`},
			result:   InputQueueResult{Result: inputQueueCommitted},
			wantText: "Cleared the queued after-completion inputs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newQueueHarness(t)
			h.r.queueViews = map[string]queueView{h.proc.key: {proc: h.proc, queue: testQueue(9, items...)}}
			h.answerQueue(testQueue(9, items...), tc.result)
			h.r.handleChordCommand(h.ws, "chat-1", parseIMCommand(tc.command), "wechat", nil)

			if got := h.stdin.String(); !containsAll(got, tc.wantStd) {
				t.Fatalf("stdin = %q, want every field in %q", got, tc.wantStd)
			}
			if tc.result.Result == "" {
				return
			}
			// Every command carries the target Chord compares verbatim: a
			// mismatch is refused, so omitting it would make the whole surface
			// silently fail.
			for _, want := range []string{`"target":{"runtime_id":"runtime-1","session_id":"session-1","session_epoch":3,"owner_task_id":"main"`, `"binding_id":"binding-1"`, `"operation_id":"gwq-`} {
				if !strings.Contains(h.stdin.String(), want) {
					t.Fatalf("stdin = %q, want %q", h.stdin.String(), want)
				}
			}
			if got := h.sender.lastMessage().text; !strings.Contains(got, tc.wantText) {
				t.Fatalf("reply = %q, want %q", got, tc.wantText)
			}
		})
	}
}

// TestQueueCommandConflictAsksForRefresh covers the one outcome a stale
// projection produces. It must not read as a failure: nothing was applied, and
// the fix is to re-read the queue rather than retry the same address.
func TestQueueCommandConflictAsksForRefresh(t *testing.T) {
	h := newQueueHarness(t)
	items := []InputQueueItem{{InputID: "input-a", Revision: 1, Sequence: 1, Policy: "next_request", Stage: "queued", Preview: "gone"}}
	h.r.queueViews = map[string]queueView{h.proc.key: {proc: h.proc, queue: testQueue(9, items...)}}
	h.answerQueue(testQueue(9, items...), InputQueueResult{Result: inputQueueConflict, Reason: `queued input "input-a" no longer exists`})
	h.r.handleChordCommand(h.ws, "chat-1", parseIMCommand("/queue delete 1"), "wechat", nil)

	reply := h.sender.lastMessage().text
	if !strings.Contains(reply, "no longer exists") || !strings.Contains(reply, "/queue") {
		t.Fatalf("reply = %q, want Chord's reason and a refresh hint", reply)
	}
}

func TestQueueCommandRejectsUnaddressableIndex(t *testing.T) {
	h := newQueueHarness(t)
	items := []InputQueueItem{{InputID: "input-a", Revision: 1, Sequence: 1, Policy: "next_request", Stage: "queued", Preview: "only one"}}
	h.r.queueViews = map[string]queueView{h.proc.key: {proc: h.proc, queue: testQueue(9, items...)}}
	h.answerQueue(testQueue(9, items...), InputQueueResult{Result: inputQueueCommitted})
	h.r.handleChordCommand(h.ws, "chat-1", parseIMCommand("/queue delete 3"), "wechat", nil)

	if got := h.sender.lastMessage().text; !strings.Contains(got, "no queued input #3") {
		t.Fatalf("reply = %q", got)
	}
	if got := h.stdin.String(); strings.Contains(got, `"type":"input_queue"`) {
		t.Fatalf("an unaddressable index must not reach Chord: %q", got)
	}
}

// TestQueueCommandEditRefusesAttachments pins the local refusal. Chord rejects
// the same update with a pointer to the local TUI, which is not where an IM
// operator is, so the gateway answers before spending a round trip.
func TestQueueCommandEditRefusesAttachments(t *testing.T) {
	h := newQueueHarness(t)
	items := []InputQueueItem{{InputID: "input-a", Revision: 2, Sequence: 1, Policy: "next_request", Stage: "queued", Preview: "chart", Attachments: 1}}
	h.r.queueViews = map[string]queueView{h.proc.key: {proc: h.proc, queue: testQueue(9, items...)}}
	h.answerQueue(testQueue(9, items...), InputQueueResult{Result: inputQueueCommitted})
	h.r.handleChordCommand(h.ws, "chat-1", parseIMCommand("/queue edit 1 new text"), "wechat", nil)

	if got := h.sender.lastMessage().text; !strings.Contains(got, "attachment") {
		t.Fatalf("reply = %q", got)
	}
	if got := h.stdin.String(); strings.Contains(got, `"action":"update"`) {
		t.Fatalf("attachment edit must not reach Chord: %q", got)
	}
}

// TestQueueCommandWithoutProjection covers a Chord build that predates the
// surface, or a session that has published nothing yet.
func TestQueueCommandWithoutProjection(t *testing.T) {
	h := newQueueHarness(t)
	h.answerQueue(nil, InputQueueResult{})
	h.r.handleChordCommand(h.ws, "chat-1", parseIMCommand("/queue"), "wechat", nil)

	if got := h.sender.lastMessage().text; !strings.Contains(got, "has not reported an input queue") {
		t.Fatalf("reply = %q", got)
	}
}

// TestQueueAfterCompletionSkipsTheReceiptPipeline is the guard for the sharpest
// edge in this surface: Chord answers a policied send with input_queue_result,
// never input_result. Registering a receipt observer the way an ordinary send
// does would leave the process waiting for a reply that never arrives, and
// every later input would be refused as backpressure.
func TestQueueAfterCompletionSkipsTheReceiptPipeline(t *testing.T) {
	h := newQueueHarness(t)
	h.answerQueue(testQueue(9), InputQueueResult{Result: inputQueueCommitted, InputID: "input-new"})
	h.r.handleChordCommand(h.ws, "chat-1", parseIMCommand("/queue after run the suite"), "wechat", nil)

	stdin := h.stdin.String()
	for _, want := range []string{`"type":"send"`, `"delivery_policy":"after_completion"`, `"operation_id":"gwq-`, `"request_id":"im-queue-`, `"content":"run the suite"`} {
		if !strings.Contains(stdin, want) {
			t.Fatalf("stdin = %q, want %q", stdin, want)
		}
	}
	if got := h.sender.lastMessage().text; !strings.Contains(got, "after the current work completes") {
		t.Fatalf("reply = %q", got)
	}

	h.proc.mu.RLock()
	pending := len(h.proc.pendingInputs)
	observers := len(h.proc.inputObservers)
	waiters := len(h.proc.queueWaiters.waiters)
	h.proc.mu.RUnlock()
	if pending != 0 || observers != 0 {
		t.Fatalf("after-completion send must not open a receipt: pending=%d observers=%d", pending, observers)
	}
	if waiters != 0 {
		t.Fatalf("%d queue waiter(s) leaked", waiters)
	}
	if got := h.proc.State().UnconfirmedInputs; got != 0 {
		t.Fatalf("UnconfirmedInputs = %d, want 0", got)
	}
}

func TestQueueAfterCompletionRefusesNew(t *testing.T) {
	h := newQueueHarness(t)
	h.answerQueue(testQueue(9), InputQueueResult{Result: inputQueueCommitted})
	h.r.handleChordCommand(h.ws, "chat-1", parseIMCommand("/queue after /new"), "wechat", nil)

	if got := h.sender.lastMessage().text; !strings.Contains(got, "cannot be scheduled after completion") {
		t.Fatalf("reply = %q", got)
	}
	if got := h.stdin.String(); strings.Contains(got, `"delivery_policy"`) {
		t.Fatalf("/new must not reach Chord as a queued submit: %q", got)
	}
}

// TestInputQueueWaitersAreKeyedByOperationID proves two concurrent /queue
// callers cannot settle each other's command. Chord echoes the operation_id, so
// a broadcast waiter would hand one user's result to the other.
func TestInputQueueWaitersAreKeyedByOperationID(t *testing.T) {
	p := &ChordProcess{stdin: &captureWriteCloser{}, runtimeID: "runtime-1", sessionEpoch: 3}
	p.state.SessionID = "session-1"
	p.state.InputQueue = testQueue(3, InputQueueItem{InputID: "input-a", Revision: 1, Sequence: 1, Policy: "next_request", Stage: "queued", Preview: "a"})

	results := make(chan InputQueueResult, 2)
	for _, id := range []string{"input-a", "input-b"} {
		go func(inputID string) {
			result, err := p.WaitInputQueue(context.Background(), "get", map[string]any{"input_id": inputID})
			if err != nil {
				t.Errorf("WaitInputQueue(%s): %v", inputID, err)
			}
			results <- result
		}(id)
	}

	deadline := time.Now().Add(2 * time.Second)
	var ops []string
	for time.Now().Before(deadline) {
		p.mu.Lock()
		ops = ops[:0]
		for id := range p.queueWaiters.waiters {
			ops = append(ops, id)
		}
		p.mu.Unlock()
		if len(ops) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(ops) != 2 {
		t.Fatalf("waiters = %v, want 2", ops)
	}

	// Answer in reverse order: the first reply must reach the second caller.
	payload, _ := json.Marshal(InputQueueResult{OperationID: ops[1], Result: inputQueueCommitted, InputID: "second-answer"})
	p.processEnvelope(&HeadlessEnvelope{Type: "input_queue_result", Payload: payload})
	payload, _ = json.Marshal(InputQueueResult{OperationID: ops[0], Result: inputQueueCommitted, InputID: "first-answer"})
	p.processEnvelope(&HeadlessEnvelope{Type: "input_queue_result", Payload: payload})

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			got[result.InputID] = true
		case <-time.After(2 * time.Second):
			t.Fatal("waiter was not released")
		}
	}
	if !got["first-answer"] || !got["second-answer"] {
		t.Fatalf("results = %v, want each caller to receive its own", got)
	}
}

// TestQueueWaiterReportsUnknownWithoutProjection keeps the "no target yet"
// case out of the wire: sending a fabricated target would address a queue that
// is not this session's.
func TestQueueWaiterReportsUnknownWithoutProjection(t *testing.T) {
	p := &ChordProcess{stdin: &captureWriteCloser{}}
	if _, err := p.WaitInputQueue(context.Background(), "pause", map[string]any{"expected_queue_revision": 1}); err == nil {
		t.Fatal("WaitInputQueue accepted a process with no projection")
	}
}
