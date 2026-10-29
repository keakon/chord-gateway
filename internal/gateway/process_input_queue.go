package gateway

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type InputQueueTarget struct {
	RuntimeID         string `json:"runtime_id"`
	SessionID         string `json:"session_id"`
	SessionEpoch      uint64 `json:"session_epoch"`
	OwnerTaskID       string `json:"owner_task_id"`
	WorkDirGeneration uint64 `json:"work_dir_generation"`
}
type InputQueueWork struct {
	WorkID  string `json:"work_id"`
	Origin  string `json:"origin"`
	Outcome string `json:"outcome"`
}
type InputQueueItem struct {
	InputID     string `json:"input_id"`
	Revision    uint64 `json:"revision"`
	Sequence    int64  `json:"sequence"`
	Policy      string `json:"delivery_policy"`
	Stage       string `json:"stage"`
	WorkID      string `json:"work_id,omitempty"`
	WaitWorkID  string `json:"wait_work_id,omitempty"`
	WorkDir     string `json:"work_dir"`
	WorktreeID  string `json:"worktree_id,omitempty"`
	Preview     string `json:"preview"`
	Attachments int    `json:"attachments"`
	EditHeld    bool   `json:"edit_held"`
}
type InputQueue struct {
	InputQueueTarget
	ViewVersion    uint64           `json:"view_version"`
	ViewVersionCAS bool             `json:"view_version_cas"`
	QueueRevision  uint64           `json:"queue_revision"`
	Works          []InputQueueWork `json:"works"`
	Inputs         []InputQueueItem `json:"inputs"`
	Blocked        string           `json:"blocked,omitempty"`
}

func (q *InputQueue) clone() *InputQueue {
	if q == nil {
		return nil
	}
	out := *q
	out.Works = slices.Clone(q.Works)
	out.Inputs = slices.Clone(q.Inputs)
	return &out
}

// Input queue result values. Chord reports a stale expected revision as
// "conflict" and every other refusal as "rejected"; both mean the gateway's
// projection is behind and nothing was applied.
const (
	inputQueueCommitted = "committed"
	inputQueueConflict  = "conflict"
	inputQueueRejected  = "rejected"
)

// InputQueueResult is the reply to one input_queue command. Chord echoes the
// operation_id it was handed, which is the only correlation available: these
// commands answer with this envelope instead of input_result.
//
// The get action reuses this shape for its content and attachment count, so a
// single decoding covers every action.
type InputQueueResult struct {
	OperationID   string            `json:"operation_id"`
	Target        *InputQueueTarget `json:"target,omitempty"`
	Result        string            `json:"result"`
	Reason        string            `json:"reason,omitempty"`
	InputID       string            `json:"input_id,omitempty"`
	ItemRevision  uint64            `json:"item_revision,omitempty"`
	QueueRevision uint64            `json:"queue_revision"`
	Content       string            `json:"content,omitempty"`
	Attachments   int               `json:"attachments,omitempty"`
}

// errInputQueueUnknown means the gateway has never seen a queue projection for
// the live binding, so it has no target to send. Chord requires the exact
// target it advertised, and inventing one would address a queue that is not
// this session's.
var errInputQueueUnknown = errors.New("input queue state is unknown; use /status and retry")

// inputQueueWaiters keys replies by operation_id. Chord echoes the id, and a
// broadcast waiter would let one caller consume another's result, so these
// cannot use envelopeWaiters.
type inputQueueWaiters struct {
	waiters map[string]chan InputQueueResult
}

func (w *inputQueueWaiters) add(id string, ch chan InputQueueResult) {
	if w.waiters == nil {
		w.waiters = make(map[string]chan InputQueueResult)
	}
	w.waiters[id] = ch
}

func (w *inputQueueWaiters) remove(id string) { delete(w.waiters, id) }

// take removes and returns the waiter for id, or nil when nothing is waiting.
func (w *inputQueueWaiters) take(id string) chan InputQueueResult {
	ch := w.waiters[id]
	delete(w.waiters, id)
	return ch
}

// notifyInputQueueResult runs under p.mu and hands one reply to its waiter. A
// reply nobody waits for is dropped: it can only be a late answer to an
// operation that already timed out, and acting on it would apply a result the
// caller has stopped waiting for.
func (p *ChordProcess) notifyInputQueueResult(result InputQueueResult) {
	ch := p.queueWaiters.take(result.OperationID)
	if ch == nil {
		return
	}
	select {
	case ch <- result:
	default:
	}
}

// WaitInputQueue issues one input_queue command against the target Chord last
// advertised and waits for the matching input_queue_result.
//
// Chord compares the whole target struct and rejects anything else, so a
// session switch, epoch bump or working-directory change since the cached
// projection makes it refuse the command instead of applying it to a queue the
// gateway cannot see. That rejection is the safety property here: a stale
// cache can only fail, never retarget another session's input.
func (p *ChordProcess) WaitInputQueue(ctx context.Context, action string, fields map[string]any) (InputQueueResult, error) {
	cmd := map[string]any{"type": "input_queue", "action": action}
	for key, value := range fields {
		cmd[key] = value
	}
	return p.waitForInputQueueResult(ctx, cmd)
}

// WaitAfterCompletion submits text that starts only once the current work
// finishes. Chord routes a policied send through its input delivery queue and
// answers with input_queue_result rather than input_result, so this shares the
// operation_id correlation above instead of the receipt pipeline.
func (p *ChordProcess) WaitAfterCompletion(ctx context.Context, content, requestID string) (InputQueueResult, error) {
	return p.waitForInputQueueResult(ctx, map[string]any{
		"type":            "send",
		"content":         content,
		"request_id":      requestID,
		"delivery_policy": "after_completion",
	})
}

// waitForInputQueueResult stamps the command with an operation_id and the
// cached target, sends it, and waits for the reply carrying that id.
func (p *ChordProcess) waitForInputQueueResult(ctx context.Context, cmd map[string]any) (InputQueueResult, error) {
	target, known := p.inputQueueTargetSnapshot()
	if !known {
		return InputQueueResult{}, errInputQueueUnknown
	}
	operationID := "gwq-" + rand.Text()
	cmd["operation_id"] = operationID
	if _, supplied := cmd["target"]; !supplied {
		cmd["target"] = target
	}
	if _, supplied := cmd["expected_view_version"]; !supplied {
		p.mu.RLock()
		if queue := p.state.InputQueue; queue != nil && queue.ViewVersionCAS {
			cmd["expected_view_version"] = queue.ViewVersion
		}
		p.mu.RUnlock()
	}
	// Chord refuses an input_queue command whose binding_id does not match the
	// --gateway-instance it was started with, so a managed process must echo it
	// back or every mutation fails as if the binding had changed.
	if p.bindingInstanceID != "" {
		cmd["binding_id"] = p.bindingInstanceID
	}

	var zero InputQueueResult
	ch := make(chan InputQueueResult, 1)
	p.mu.Lock()
	p.queueWaiters.add(operationID, ch)
	p.mu.Unlock()
	// SendCommand applies the takeover guard, so a binding mid-transfer refuses
	// the command instead of queueing work nothing will consume.
	if err := p.SendCommand(cmd); err != nil {
		p.mu.Lock()
		p.queueWaiters.remove(operationID)
		p.mu.Unlock()
		return zero, err
	}
	select {
	case result := <-ch:
		return result, nil
	case <-ctx.Done():
		p.mu.Lock()
		p.queueWaiters.remove(operationID)
		p.mu.Unlock()
		return zero, ctx.Err()
	}
}

// inputQueueTargetSnapshot returns the target Chord last advertised, together
// with whether it still describes this process.
func (p *ChordProcess) inputQueueTargetSnapshot() (InputQueueTarget, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.state.InputQueue == nil {
		return InputQueueTarget{}, false
	}
	target := p.state.InputQueue.InputQueueTarget
	return target, p.inputQueueTargetMatches(target)
}

func (p *ChordProcess) inputQueueTargetMatches(target InputQueueTarget) bool {
	return target.RuntimeID == p.runtimeID && target.SessionID == p.state.SessionID && target.SessionEpoch == p.sessionEpoch && target.OwnerTaskID == "main"
}

// Called under p.mu. A nil projection is unknown, not a known empty queue.
func (p *ChordProcess) applyInputQueue(q *InputQueue) bool {
	if q == nil || !p.inputQueueTargetMatches(q.InputQueueTarget) || len(q.Inputs) > 128 || q.ViewVersion == 0 {
		return false
	}
	old := p.state.InputQueue
	if old != nil && old.RuntimeID == q.RuntimeID && old.SessionID == q.SessionID && old.SessionEpoch == q.SessionEpoch && old.ViewVersion >= q.ViewVersion {
		return false
	}
	seen := make(map[string]bool)
	for _, input := range q.Inputs {
		if !validPausedInputID(input.InputID) || seen[input.InputID] || input.Revision == 0 || input.Sequence <= 0 || input.Attachments < 0 || utf8.RuneCountInString(input.Preview) > 160 {
			return false
		}
		if input.Policy != "next_request" && input.Policy != "after_completion" {
			return false
		}
		if input.Stage != "queued" && input.Stage != "claimed" && input.Stage != "started" {
			return false
		}
		seen[input.InputID] = true
	}
	p.state.InputQueue = q.clone()
	return true
}

var errInputStatusUnknown = errors.New("pending input status could not be refreshed; use /status and retry")

// Main's queue and idle events cannot settle a previously observed worker input.
// Refresh the all-owner projection before retiring or replacing an idle session.
// Call without lifecycle/key locks: the stdout reader may need those locks.
func (p *ChordProcess) refreshPendingInputStatus(ctx context.Context) error {
	p.mu.RLock()
	needsRefresh := !p.state.Busy && p.state.PausedInputs != nil && p.state.PausedInputs.PendingInputCount > 0
	p.mu.RUnlock()
	if !needsRefresh {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", errInputStatusUnknown, err)
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := p.WaitStatus(wait); err != nil {
		return fmt.Errorf("%w: %w", errInputStatusUnknown, err)
	}
	return nil
}

// Existing evidence survives process death and every incoming takeover phase.
func (p *ChordProcess) guardedInputError() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.hasGuardedInputsLocked() {
		return errUnresolvedInputs
	}
	return nil
}

var errUnresolvedInputs = errors.New("input state remains unresolved; inspect the original session on the host before replacing it")

// The process owns this evidence until it is resolved. Automatic retirement
// or replacement must not silently discard it.
func (p *ChordProcess) hasGuardedInputsLocked() bool {
	s := p.state
	q := s.InputQueue
	return len(p.pendingInputs) != 0 || s.UnconfirmedInputs != 0 ||
		s.PausedInputs != nil && s.PausedInputs.PendingInputCount != 0 ||
		q != nil && (len(q.Inputs) != 0 || q.Blocked != "")
}

func formatInputQueueStatus(q *InputQueue) string {
	if q == nil {
		return ""
	}
	text := fmt.Sprintf("Input queue: %d pending", len(q.Inputs))
	if q.Blocked != "" {
		text += "\n⚠️ Queue storage is blocked. Inspect the original session and storage on the host."
	} else if len(q.Inputs) > 0 {
		text += "\nInspect or manage queued inputs with /queue."
	}
	return text
}

// inputQueueListLimit bounds what one IM message shows. Chord allows 128 queued
// inputs; listing them all would trip platform message limits and drown the
// part the operator is looking for.
const inputQueueListLimit = 10

// formatInputQueue renders the projection for the /queue command. The numbers
// are part of the interface, not cosmetics: they are the only handle /queue
// show, /queue edit and /queue delete have over an item, and Chord's input IDs
// are opaque.
func formatInputQueue(q *InputQueue) string {
	if q == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Input queue: %d pending", len(q.Inputs))
	if q.Blocked != "" {
		fmt.Fprintf(&b, "\n⚠️ Queue storage is blocked: %s", strings.ReplaceAll(q.Blocked, "\n", " "))
	}
	for i, input := range q.Inputs {
		if i == inputQueueListLimit {
			fmt.Fprintf(&b, "\n… and %d more", len(q.Inputs)-i)
			break
		}
		fmt.Fprintf(&b, "\n%d. [%s, %s] %s", i+1, input.Policy, input.Stage, input.Preview)
		if input.Attachments > 0 {
			fmt.Fprintf(&b, " (%d attachment(s))", input.Attachments)
		}
		if input.EditHeld {
			b.WriteString(" (locked for editing in the local TUI)")
		}
		if reason := afterCompletionWaitReason(q, input); reason != "" {
			fmt.Fprintf(&b, " (%s)", reason)
		}
	}
	if len(q.Inputs) > 0 {
		b.WriteString("\nReply /queue show <n> to read one in full, /queue edit <n> <text> to replace it, or /queue delete <n> to drop it.")
	}
	return b.String()
}

// afterCompletionWaitReason explains why an after-completion input has not
// fired, from the outcome of the work it waits on. Chord fires the input at the
// first idle after that work completes; a failed, cancelled or unknown outcome
// holds it until a newer task the operator sends completes. An empty result
// means nothing holds it beyond the next idle.
func afterCompletionWaitReason(q *InputQueue, input InputQueueItem) string {
	if input.WaitWorkID == "" {
		return ""
	}
	for _, work := range q.Works {
		if work.WorkID != input.WaitWorkID {
			continue
		}
		switch work.Outcome {
		case "active":
			return "waiting for the running task"
		case "completed":
			return ""
		case "failed":
			return "last task failed; send a message to run it next"
		case "cancelled":
			return "last task cancelled; send a message to run it next"
		default:
			return "last task outcome unknown; send a message to run it next"
		}
	}
	return ""
}
