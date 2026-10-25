package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
type InputQueueGate struct {
	Mode          string `json:"mode"`
	Reason        string `json:"reason,omitempty"`
	WaitingWorkID string `json:"waiting_work_id,omitempty"`
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
	WorkDir     string `json:"work_dir"`
	WorktreeID  string `json:"worktree_id,omitempty"`
	Preview     string `json:"preview"`
	Attachments int    `json:"attachments"`
	EditHeld    bool   `json:"edit_held"`
}
type InputQueue struct {
	RestoredNextInputsBlocked bool `json:"restored_next_inputs_blocked,omitempty"`
	InputQueueTarget
	ViewVersion   uint64           `json:"view_version"`
	QueueRevision uint64           `json:"queue_revision"`
	Gate          InputQueueGate   `json:"gate"`
	Works         []InputQueueWork `json:"works"`
	Inputs        []InputQueueItem `json:"inputs"`
	Blocked       string           `json:"blocked,omitempty"`
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
		q != nil && (len(q.Inputs) != 0 || q.Blocked != "" || q.RestoredNextInputsBlocked)
}

func formatInputQueueStatus(q *InputQueue) string {
	if q == nil {
		return ""
	}
	mode := q.Gate.Mode
	if mode == "" {
		mode = "unknown gate"
	}
	text := fmt.Sprintf("Input queue: %d pending; %s", len(q.Inputs), mode)
	if q.Blocked != "" {
		text += "\n⚠️ Queue storage is blocked. Inspect the original session and storage on the host."
	} else if q.RestoredNextInputsBlocked {
		text += "\nRestored inputs await explicit continuation in the original session."
	} else if q.Gate.Mode == "paused" || len(q.Inputs) > 0 {
		text += "\nInspect or manage queued inputs in the local Chord TUI before changing directories."
	}
	return text
}
