package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTransferStatusPreservesRecoveryGuidance(t *testing.T) {
	s := TransferStatus{TransferID: "transfer", SessionID: "session", State: "save_failed", ErrorCode: "persistence_failed", Message: "Could not save session", RecoverableAction: "retry-save", WaitExpired: true}
	for range 8 {
		s.Blockers = append(s.Blockers, TransferBlocker{OwnerTaskID: "worker", Type: "tool", Description: strings.Repeat("waiting ", 40)})
	}
	text := formatTransferStatus(s)
	for _, want := range []string{"State: save_failed", "persistence_failed", "Next: retry-save", "Wait budget expired", "/takeover status", "more blockers", "on the host"} {
		requireContains(t, text, want)
	}
	if utf8.RuneCountInString(text) > maxNotificationRunes {
		t.Fatal("status exceeds the IM notification budget")
	}
}

func TestTransferStatusBlockerCounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total int
		page  int
		shown int
	}{
		{"short", 1, 1, 1},
		{"bounded preview", 8, 8, 3},
		{"paged response", 80, 3, 3},
		{"empty page", 80, 0, 0},
		{"count from entries", 0, 4, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise the actual JSON field from Chord's public response.
			var status TransferStatus
			if err := json.Unmarshal(fmt.Appendf(nil, `{"blocker_count":%d}`, tc.total), &status); err != nil {
				t.Fatal(err)
			}
			for range tc.page {
				status.Blockers = append(status.Blockers, TransferBlocker{OwnerTaskID: "worker", Type: "tool", Description: "Waiting for input"})
			}
			text := formatTransferStatus(status)
			requireContains(t, text, fmt.Sprintf("Blockers: %d", max(tc.total, tc.page)))
			if strings.Count(text, "\nWaiting:") != tc.shown {
				t.Fatalf("wrong preview count: %s", text)
			}
			if omitted := max(tc.total, tc.page) - tc.shown; omitted > 0 {
				requireContains(t, text, fmt.Sprintf("%d more blockers", omitted))
			} else if strings.Contains(text, "more blockers") {
				t.Fatal("complete preview claims omitted blockers")
			}
		})
	}
}

func TestTransferStatusUnicodeBudget(t *testing.T) {
	long := strings.Repeat("等待🙂\n", 300)
	for _, state := range []string{"save_failed", "ready_paused", "saved"} {
		s := TransferStatus{TransferID: long, SessionID: long, State: state, StartedAt: long, ErrorCode: long, Message: long, RecoverableAction: "retry-save " + long, WaitExpired: true, PendingInputs: int(^uint(0) >> 1), BlockerCount: int(^uint(0) >> 1)}
		for range 64 {
			s.Blockers = append(s.Blockers, TransferBlocker{OwnerTaskID: long, Type: long, Description: long})
		}
		text := formatTransferStatus(s)
		if !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxNotificationRunes {
			t.Fatalf("invalid or oversized Unicode status: %d runes", utf8.RuneCountInString(text))
		}
		for _, want := range []string{"State: " + state, "Next: retry-save", "Wait budget expired", "more blockers", "on the host."} {
			requireContains(t, text, want)
		}
		if state == "ready_paused" {
			requireContains(t, text, "Send an explicit continuation")
		}
		if state == "saved" {
			requireContains(t, text, "Resume explicitly")
		}
	}
}

func TestPausedTargetStatusClearsSourceBlockers(t *testing.T) {
	status := TransferStatus{State: "ready_paused", BlockerCount: 80, Blockers: []TransferBlocker{{Description: "Old source work"}}}
	text := formatPausedTargetStatus(status, ControlState{PausedInputs: &PausedInputs{Paused: true}})
	if strings.Contains(text, "Blockers:") || strings.Contains(text, "Old source work") {
		t.Fatal("ready target retained source blockers")
	}
	requireContains(t, text, "Restored and paused")
}
