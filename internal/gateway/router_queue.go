package gateway

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// inputQueueRefreshTimeout bounds the status read /queue starts with, and
// inputQueueTimeout the mutation that follows.
//
// They are separate on purpose. A shared budget meant a slow status read — a
// busy turn draining ahead of it — could leave almost nothing for the mutation:
// the command was already written to stdin, but the expired budget made the
// caller report it unconfirmed, which is the one answer an operator must not get
// when the change is about to land.
const (
	inputQueueRefreshTimeout = 10 * time.Second
	inputQueueTimeout        = 15 * time.Second
)

const queueUsage = "Use /queue to list, /queue show <n>, /queue edit <n> <text>, /queue delete <n>, " +
	"/queue pause, /queue resume, /queue clear, or /queue after <text>."

// queueRequest is the parsed form of one /queue invocation.
type queueRequest struct {
	action string // list, show, edit, delete, pause, resume, clear, after
	index  int    // 1-based position in the listing; 0 when the action takes none
	text   string
}

type queueView struct {
	proc  *ChordProcess
	queue *InputQueue
}

func (r *NotificationRouter) showQueue(procKey, chatID string, proc *ChordProcess, queue *InputQueue, note string) {
	if r.sendTextForKey(procKey, chatID, formatInputQueue(queue)+note) {
		r.mu.Lock()
		if r.queueViews == nil {
			r.queueViews = make(map[string]queueView)
		}
		r.queueViews[procKey] = queueView{proc: proc, queue: queue.clone()}
		r.mu.Unlock()
	}
}

// parseQueueRequest splits the tail of /queue. Positions are 1-based to match
// formatInputQueue, so an operator can act on the item they just read without
// copying an opaque Chord input ID.
func parseQueueRequest(rest string) (queueRequest, bool) {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return queueRequest{action: "list"}, true
	}
	action := strings.ToLower(fields[0])
	tail := strings.TrimSpace(rest[len(fields[0]):])
	switch action {
	case "list":
		return queueRequest{action: "list"}, tail == ""
	case "pause", "resume", "clear":
		return queueRequest{action: action}, tail == ""
	case "show", "delete", "drop", "rm", "edit":
		name, arg, _ := strings.Cut(tail, " ")
		index, err := strconv.Atoi(strings.TrimSpace(name))
		if err != nil || index <= 0 {
			return queueRequest{}, false
		}
		text := strings.TrimSpace(arg)
		switch action {
		case "show":
			return queueRequest{action: "show", index: index}, text == ""
		case "edit":
			return queueRequest{action: "edit", index: index, text: text}, text != ""
		}
		return queueRequest{action: "delete", index: index}, text == ""
	case "after":
		if tail == "" {
			return queueRequest{}, false
		}
		return queueRequest{action: "after", text: tail}, true
	}
	return queueRequest{}, false
}

func queueItemAt(queue *InputQueue, index int) (InputQueueItem, bool) {
	if queue == nil || index <= 0 || index > len(queue.Inputs) {
		return InputQueueItem{}, false
	}
	return queue.Inputs[index-1], true
}

// unconfirmedQueueChange is the reply when Chord never answered. A mutation
// that timed out may still have been applied, so it must not be reported as a
// failure: retrying on that assumption could delete the wrong item after the
// queue shifted.
const unconfirmedQueueChange = "⚠️ Chord did not confirm the change. It may still have applied; check with /queue before retrying."

// queueCommandFailure separates the errors that prove nothing reached Chord
// from the ones that leave the outcome open.
func queueCommandFailure(err error) string {
	switch {
	case errors.Is(err, errInputQueueUnknown):
		return "⚠️ " + err.Error()
	case errors.Is(err, ErrSessionTransferred), errors.Is(err, io.ErrClosedPipe):
		return "⚠️ This binding is not accepting commands right now. Use /status to check it."
	}
	return unconfirmedQueueChange
}

// queueChangeRejected renders Chord's refusal. A conflict is a stale read and
// has a next step; a rejection is Chord declining on its own terms (a paused
// session, a resume while work is still running) and only Chord's reason
// explains it.
func queueChangeRejected(result InputQueueResult) string {
	reason := strings.TrimSpace(result.Reason)
	if reason == "" {
		reason = "Chord rejected the queue change."
	}
	switch result.Result {
	case inputQueueConflict:
		return "⚠️ The queue changed since it was read — " + reason + "\n/queue lists the current items."
	case inputQueueRejected:
		return "⚠️ Chord refused the change: " + reason
	}
	return "⚠️ " + reason
}

// handleQueueCommand answers the /queue family through Chord's input_queue
// protocol command.
//
// The gateway used to refuse /queue outright and tell the operator to manage
// the queue in the local TUI, but it also refuses /new, /resume and /bind while
// the queue holds unresolved input. On a remote host those two rules together
// left no way out of the state the gateway itself demanded someone fix.
func (r *NotificationRouter) handleQueueCommand(ws *config.Workspace, chatID string, cmd im.IMCommand, procKey string, proc *ChordProcess) {
	request, ok := parseQueueRequest(cmd.Content)
	if !ok {
		r.sendTextForKey(procKey, chatID, queueUsage)
		return
	}

	// Refresh to detect a changed view, without reinterpreting the user's
	// numeric address against the new list.
	refreshCtx, refreshCancel := context.WithTimeout(context.Background(), inputQueueRefreshTimeout)
	queue, refreshErr := r.refreshInputQueue(refreshCtx, proc)
	refreshCancel()
	if refreshErr != nil {
		log.Warnf("queue state refresh failed workspace=%v error=%v", ws.ID, refreshErr)
	}
	if queue == nil {
		// Either this Chord build predates the surface or the session has not
		// published a queue yet; both leave nothing to address.
		r.sendTextForKey(procKey, chatID, "⚠️ Chord has not reported an input queue for this session. Nothing is queued, or this Chord build does not expose queue management.")
		return
	}

	if request.action == "list" {
		note := ""
		if refreshErr != nil {
			note = "\n\n⚠️ Could not refresh from Chord; this is the last state it reported."
		}
		r.showQueue(procKey, chatID, proc, queue, note)
		return
	}
	if !queue.ViewVersionCAS {
		r.sendTextForKey(procKey, chatID, "⚠️ This Chord build does not support queue view checks. /queue can list the queue; manage it in the local Chord TUI.")
		return
	}
	if request.index > 0 {
		r.mu.RLock()
		view := r.queueViews[procKey]
		r.mu.RUnlock()
		if view.proc != proc || view.queue == nil {
			r.showQueue(procKey, chatID, proc, queue, "\n\n⚠️ Read this list before choosing an item number, then repeat the command.")
			return
		}
		if view.queue.InputQueueTarget != queue.InputQueueTarget || view.queue.ViewVersion != queue.ViewVersion {
			r.showQueue(procKey, chatID, proc, queue, "\n\n⚠️ The queue changed since the list was shown. No change was submitted; choose an item from this list.")
			return
		}
		queue = view.queue
	}
	if request.action == "after" {
		ctx, cancel := context.WithTimeout(context.Background(), inputQueueTimeout)
		defer cancel()
		r.submitAfterCompletion(ctx, ws, chatID, request.text, procKey, proc)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), inputQueueTimeout)
	defer cancel()

	var (
		result InputQueueResult
		err    error
		target string
	)
	switch request.action {
	case "show", "edit", "delete":
		item, found := queueItemAt(queue, request.index)
		if !found {
			r.sendTextForKey(procKey, chatID, fmt.Sprintf("⚠️ There is no queued input #%d. /queue lists the current items.", request.index))
			return
		}
		target = fmt.Sprintf("#%d/%s", request.index, item.Policy)
		switch request.action {
		case "show":
			result, err = proc.WaitInputQueue(ctx, "get", map[string]any{"input_id": item.InputID, "expected_view_version": queue.ViewVersion, "target": queue.InputQueueTarget})
		case "edit":
			if item.Attachments > 0 {
				// Chord refuses this too, and its reason ("Use the local TUI to
				// edit input with attachments") names a place the IM operator
				// cannot reach, so reject with the reason that matters here.
				r.sendTextForKey(procKey, chatID, fmt.Sprintf("⚠️ Queued input #%d carries %d attachment(s); editing attachments is only possible in the local Chord TUI. Delete and resend it instead.", request.index, item.Attachments))
				return
			}
			result, err = proc.WaitInputQueue(ctx, "update", map[string]any{
				"input_id":              item.InputID,
				"expected_revision":     item.Revision,
				"content":               request.text,
				"expected_view_version": queue.ViewVersion,
				"target":                queue.InputQueueTarget,
			})
		default:
			result, err = proc.WaitInputQueue(ctx, "delete", map[string]any{
				"input_id":              item.InputID,
				"expected_revision":     item.Revision,
				"expected_view_version": queue.ViewVersion,
				"target":                queue.InputQueueTarget,
			})
		}
	case "pause", "resume", "clear":
		target = request.action
		result, err = proc.WaitInputQueue(ctx, request.action, map[string]any{"expected_queue_revision": queue.QueueRevision, "expected_view_version": queue.ViewVersion, "target": queue.InputQueueTarget})
	default:
		r.sendTextForKey(procKey, chatID, queueUsage)
		return
	}

	if err != nil {
		log.Warnf("queue command did not complete workspace=%v action=%v target=%v error=%v", ws.ID, request.action, target, err)
		r.sendTextForKey(procKey, chatID, queueCommandFailure(err))
		return
	}
	if result.Result != inputQueueCommitted {
		log.Infof("queue.rejected workspace=%v chat_id=%v action=%v target=%v result=%v reason=%v", ws.ID, chatID, request.action, target, result.Result, result.Reason)
		r.sendTextForKey(procKey, chatID, queueChangeRejected(result))
		return
	}
	log.Infof("queue.%s workspace=%v chat_id=%v target=%v input_id=%v", request.action, ws.ID, chatID, target, result.InputID)

	switch request.action {
	case "show":
		text := result.Content
		if text == "" {
			text = "(empty)"
		}
		text = fmt.Sprintf("%d. %s", request.index, text)
		if result.Attachments > 0 {
			text += fmt.Sprintf("\n\n(%d attachment(s) stay with the item.)", result.Attachments)
		}
		r.sendTextForKey(procKey, chatID, text)
	case "edit":
		r.sendTextForKey(procKey, chatID, fmt.Sprintf("✏️ Replaced the text of queued input #%d.", request.index))
	case "delete":
		r.sendTextForKey(procKey, chatID, fmt.Sprintf("🗑️ Dropped queued input #%d.", request.index))
	case "pause":
		r.sendTextForKey(procKey, chatID, "⏸️ Paused the input queue. Queued items stay put until /queue resume.")
	case "resume":
		r.sendTextForKey(procKey, chatID, "▶️ Resumed the input queue.")
	case "clear":
		r.sendTextForKey(procKey, chatID, "🧹 Cleared the queued after-completion inputs. Chord keeps ordinary next-request inputs, and work already running is untouched.")
	}
}

// submitAfterCompletion queues text that only starts once the current work
// finishes, instead of becoming the next request.
//
// Chord answers a send with delivery_policy through input_queue_result, not
// input_result: its policy path returns before the ordinary receipt is emitted.
// Registering a pendingInputs entry or a receipt observer for it, the way
// SendUserMessageWithRequestID does, would wait forever for a reply that never
// comes and leave the process permanently holding an unconfirmed input.
func (r *NotificationRouter) submitAfterCompletion(ctx context.Context, ws *config.Workspace, chatID, text, procKey string, proc *ChordProcess) {
	// SendCommand reads a "/new" payload as a session replacement and applies
	// the unresolved-input guard to it. Chord refuses /new after completion on
	// its own, so answer here rather than let a queued submit trip a guard meant
	// for a real session switch.
	if strings.TrimSpace(text) == "/new" {
		r.sendTextForKey(procKey, chatID, "⚠️ /new cannot be scheduled after completion.")
		return
	}
	result, err := proc.WaitAfterCompletion(ctx, text, "im-queue-"+rand.Text())
	if err != nil {
		log.Warnf("after_completion submit did not complete workspace=%v error=%v", ws.ID, err)
		r.sendTextForKey(procKey, chatID, queueCommandFailure(err))
		return
	}
	if result.Result != inputQueueCommitted {
		log.Infof("queue.after_rejected workspace=%v chat_id=%v result=%v reason=%v", ws.ID, chatID, result.Result, result.Reason)
		r.sendTextForKey(procKey, chatID, queueChangeRejected(result))
		return
	}
	log.Infof("queue.after workspace=%v chat_id=%v input_id=%v", ws.ID, chatID, result.InputID)
	r.sendTextForKey(procKey, chatID, "🕓 Queued to run after the current work completes. /queue shows it until then.")
}

// refreshInputQueue asks Chord for a fresh projection and falls back to the
// cached one when the round trip fails, so a listing is still possible while
// the process is slow to answer. Mutations treat a stale fallback as best
// effort: Chord's revision check turns a wrong address into a refusal.
func (r *NotificationRouter) refreshInputQueue(ctx context.Context, proc *ChordProcess) (*InputQueue, error) {
	state, err := proc.WaitStatus(ctx)
	if err != nil {
		return proc.State().InputQueue, err
	}
	return state.InputQueue, nil
}
