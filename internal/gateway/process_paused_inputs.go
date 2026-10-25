package gateway

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxPausedInputSummaries         = 8
	maxPausedInputPreview           = 160
	maxPausedInputIDBytes           = 128
	maxPausedInputNotificationRunes = 700
)

// PausedInputs is a read-only projection from the owning headless runtime.
// Full input and attachment data remain in the Chord session, never in guards.
type PausedInputs struct {
	Paused            bool                 `json:"paused"`
	PendingInputCount int                  `json:"pending_input_count"`
	Inputs            []PausedInputSummary `json:"inputs"`
}

type PausedInputSummary struct {
	InputID         string `json:"input_id"`
	OwnerTaskID     string `json:"owner_task_id"`
	Preview         string `json:"preview"`
	AttachmentCount int    `json:"attachment_count"`
}

// Validate the complete bounded page before publishing any part of it. A
// missing projection is distinct from a known empty queue.
func normalizePausedInputs(value *PausedInputs) (*PausedInputs, error) {
	if value == nil {
		return nil, nil
	}
	if value.PendingInputCount < 0 || len(value.Inputs) != min(value.PendingInputCount, maxPausedInputSummaries) {
		return nil, errors.New("invalid paused input count or summary page")
	}
	out := *value
	out.Inputs = make([]PausedInputSummary, len(value.Inputs))
	seen := make(map[string]bool, len(value.Inputs))
	for i, input := range value.Inputs {
		if !validPausedInputID(input.InputID) || !validPausedInputID(input.OwnerTaskID) || seen[input.InputID] || input.AttachmentCount < 0 {
			return nil, errors.New("invalid paused input identity or attachment count")
		}
		seen[input.InputID] = true
		preview := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, input.Preview)
		input.Preview = truncateRunes(strings.Join(strings.Fields(preview), " "), maxPausedInputPreview, ellipsis)
		out.Inputs[i] = input
	}
	return &out, nil
}

func validPausedInputID(id string) bool {
	if id == "" || len(id) > maxPausedInputIDBytes {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func formatPausedInputs(value *PausedInputs) string {
	if value == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nPending inputs: %d", value.PendingInputCount)
	b.WriteString(formatPausedInputDetails(value))
	return b.String()
}

func formatPausedInputDetails(value *PausedInputs) string {
	rows := make([]string, len(value.Inputs))
	fixedRunes := 0
	for i, input := range value.Inputs {
		rows[i] = fmt.Sprintf("\n  %s · %s", shortID(input.OwnerTaskID), shortID(input.InputID))
		if input.AttachmentCount > 0 {
			rows[i] += fmt.Sprintf(" (%d attachments)", input.AttachmentCount)
		}
		fixedRunes += utf8.RuneCountInString(rows[i]) + len(": ")
	}
	footer := ""
	if remaining := value.PendingInputCount - len(value.Inputs); remaining > 0 {
		footer = fmt.Sprintf("\n  … %d more queued inputs", remaining)
	}
	previewRunes := maxPausedInputPreview
	if len(rows) > 0 {
		previewRunes = max(0, min(previewRunes, (maxPausedInputNotificationRunes-fixedRunes-utf8.RuneCountInString(footer))/len(rows)))
	}
	var b strings.Builder
	for i, input := range value.Inputs {
		b.WriteString(rows[i])
		if input.Preview != "" && previewRunes > 0 {
			b.WriteString(": " + truncateRunes(input.Preview, previewRunes, ellipsis))
		}
	}
	b.WriteString(footer)
	return b.String()
}

func appendPausedInputProjection(prefix, projection string) string {
	// Reserve room for every displayed owner and the omitted-input count,
	// rather than cutting the last inputs off at the general IM message limit.
	return truncateRunes(prefix, maxNotificationRunes-utf8.RuneCountInString(projection), "...") + projection
}

func formatPausedTargetStatus(status TransferStatus, state ControlState) string {
	// Use the restored runtime's projection rather than the source's count.
	if state.PausedInputs == nil {
		return formatTransferStatus(status)
	}
	status.PendingInputs = state.PausedInputs.PendingInputCount
	status.Blockers = nil
	status.BlockerCount = 0
	status.ErrorCode = ""
	status.Message = ""
	status.RecoverableAction = ""
	status.WaitExpired = false
	return appendPausedInputProjection(formatTransferStatus(status), formatPausedInputDetails(state.PausedInputs))
}
