package gateway

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// A takeover reaches the chat as a short progression rather than one status
// dump per event. Chord republishes a transfer status every time its blocking
// set changes, and a busy session changes it several times per second, so
// projecting each event onto its own message turned one transfer into a dozen
// near-identical notifications. What a user follows is three facts: that the
// transfer started, roughly how long it has been running, and that it
// finished. Full coordinator status stays available through /takeover status
// and the host-side query.
const (
	// transferProgressInterval bounds how often an unfinished transfer reports
	// progress. The gate is what keeps a long drain from flooding the chat.
	transferProgressInterval = time.Minute
	// transferProgressBlockers bounds how many waiting items one repeating
	// progress report names; the rest are counted for /takeover status.
	transferProgressBlockers = 2
)

// transferNotice is what one binding's chat was last told about its current
// transfer. It gates repetition only; it never decides whether a transfer
// succeeded.
type transferNotice struct {
	TransferID string
	ProgressAt time.Time
	LastText   string
}

// transferEventVisible reports whether a session transfer event carries news
// the chat has not already been told. Chord publishes a status event on every
// blocker change while draining, and republishes an unchanged status when a
// checkpoint is recorded, so an unchanged report and a burst of changed ones
// must both be suppressed. Failures and terminal states are never suppressed:
// they carry the recovery action and the outcome.
func (r *NotificationRouter) transferEventVisible(key string, status *TransferStatus, text string, now time.Time) bool {
	if status == nil || status.TransferID == "" || text == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	notice := r.transferNotices[key]
	if notice.TransferID != status.TransferID {
		// A different transfer owns this binding now. Nothing already reported
		// describes it, including a terminal state from the previous transfer.
		r.transferNotices[key] = transferNotice{TransferID: status.TransferID, ProgressAt: now, LastText: text}
		return true
	}
	if text == notice.LastText {
		// A checkpoint update republishes the same visible status verbatim.
		return false
	}
	switch {
	case isTransferTerminalState(status.State) || status.ErrorCode != "":
		// saved and released both close one transfer, and a reported failure
		// must still reach the chat when a retry later succeeds.
		notice.ProgressAt = now
	case now.Sub(notice.ProgressAt) < transferProgressInterval:
		// Unfinished progress is rate limited rather than deduplicated: its
		// elapsed time changes every second, so only the interval can bound it.
		return false
	default:
		notice.ProgressAt = now
	}
	notice.LastText = text
	r.transferNotices[key] = notice
	return true
}

// isTransferTerminalState reports whether a state closes the transfer. Both
// saved and released are terminal, and the progression text is identical for
// both, so one chat message reports the outcome. ready_paused is a process
// readiness state, not a transfer event: it never arrives on the transfer
// path, so it is deliberately not terminal here.
func isTransferTerminalState(state string) bool {
	return state == "saved" || state == "released"
}

// formatTransferNotice renders one session transfer event for chat. Errors
// keep the full coordinator status, because recovery needs the error code, the
// wait outcome and the next action.
func formatTransferNotice(status TransferStatus, now time.Time, hints ...func(string) string) string {
	switch status.State {
	case "preparing", "waiting_source":
		return fmt.Sprintf("🔄 Session takeover started: %s\nThis chat stays blocked until the session is saved.", truncateLine(status.SessionID, 128))
	case "prepare_failed", "save_failed", "restore_failed", "status_unknown":
		return formatTransferStatus(status)
	case "saved", "released":
		// The source is blocked until an explicit replacement, which is what
		// the user has to do next. The transfer ID identifies the same
		// transfer for the host-side recovery commands.
		return fmt.Sprintf("✅ Session saved for takeover: %s (transfer %s)\nThis chat stays blocked; use %s or %s to continue here.", truncateLine(status.SessionID, 128), truncateLine(status.TransferID, 128), commandHint("/new", hints), commandHint("/resume", hints))
	default:
		return formatTransferProgress(status, now, hints)
	}
}

// formatTransferProgress reports an unfinished transfer. Chord publishes no
// completion estimate, so the wait is reported as observed elapsed time plus
// what is currently blocking, never as a prediction.
func formatTransferProgress(status TransferStatus, now time.Time, hints []func(string) string) string {
	total := max(status.BlockerCount, len(status.Blockers))
	var b strings.Builder
	b.WriteString("⏳ Session takeover in progress")
	if elapsed, ok := transferElapsed(status.StartedAt, now); ok {
		fmt.Fprintf(&b, " · waiting %s", elapsed)
	}
	if total == 0 {
		b.WriteString("\nFinishing the work that is already running.")
		return b.String()
	}
	fmt.Fprintf(&b, "\nWaiting on %d:", total)
	// The footer counts blockers the message did not name: without it a report
	// that shows 2 of 5 blockers reads as if only 2 were waiting. Reserve its
	// width so the rune budget cannot drop it. The in-loop print already covers
	// the cap, so skip the footer when both rows were shown.
	footer := func(shown, remaining int) string {
		if remaining == 0 || shown == transferProgressBlockers {
			return ""
		}
		return fmt.Sprintf("\n... %d more; use %s for the full list.", remaining, commandHint("/takeover status", hints))
	}
	used := utf8.RuneCountInString(b.String())
	shown := 0
	for _, blocker := range status.Blockers {
		if shown == transferProgressBlockers {
			fmt.Fprintf(&b, "\n… %d more; use %s for the full list.", total-shown, commandHint("/takeover status", hints))
			break
		}
		row := "\nWaiting: " + truncateLine(blocker.OwnerTaskID, 32) + " " + truncateLine(blocker.Type, 32) + " — " + truncateLine(blocker.Description, 96)
		n := utf8.RuneCountInString(row)
		if used+n+utf8.RuneCountInString(footer(shown, total-shown-1)) > maxNotificationRunes {
			break
		}
		b.WriteString(row)
		used += n
		shown++
	}
	b.WriteString(footer(shown, total-shown))
	return b.String()
}

// transferElapsed renders how long a transfer has been running. StartedAt is
// the moment the source froze, so it is an observed lower bound on the wait,
// not a deadline.
func transferElapsed(startedAt string, now time.Time) (string, bool) {
	start, err := time.Parse(time.RFC3339, startedAt)
	if err != nil || start.IsZero() {
		return "", false
	}
	elapsed := now.Sub(start)
	if elapsed < 0 {
		return "", false
	}
	return elapsed.Truncate(time.Second).String(), true
}
