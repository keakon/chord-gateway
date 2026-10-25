package gateway

import (
	"strings"
	"testing"
	"time"
)

func transferNoticeStatus(state string) *TransferStatus {
	return &TransferStatus{TransferID: "transfer-1", SessionID: "session-1", State: state}
}

func TestTransferEventVisibleSuppressesRepeatedProgress(t *testing.T) {
	r := NewNotificationRouter(nil)
	key := "ws|feishu|chat"
	now := time.Date(2026, 10, 15, 14, 56, 0, 0, time.UTC)

	if !r.transferEventVisible(key, transferNoticeStatus("preparing"), "started", now) {
		t.Fatal("first transfer event must be reported")
	}
	// A checkpoint update republishes an unchanged status verbatim.
	if r.transferEventVisible(key, transferNoticeStatus("draining"), "started", now.Add(time.Second)) {
		t.Fatal("identical text must be suppressed")
	}
}

// Chord emits a status per blocker change while draining. Within the interval
// only the first change is reported; the rest would be near-identical dumps.
func TestTransferEventVisibleRateLimitsChangedProgress(t *testing.T) {
	r := NewNotificationRouter(nil)
	key := "ws|feishu|chat"
	now := time.Date(2026, 10, 15, 14, 56, 0, 0, time.UTC)

	if !r.transferEventVisible(key, transferNoticeStatus("draining"), "progress-1", now) {
		t.Fatal("first progress must be reported")
	}
	if r.transferEventVisible(key, transferNoticeStatus("draining"), "progress-2", now.Add(10*time.Second)) {
		t.Fatal("changed progress inside the interval must be suppressed")
	}
	if !r.transferEventVisible(key, transferNoticeStatus("draining"), "progress-3", now.Add(transferProgressInterval)) {
		t.Fatal("changed progress after the interval must be reported")
	}
}

// The rate limit must survive past the first interval. If the updated
// ProgressAt is not written back, every change after the first minute is
// reported and a long drain floods the chat.
func TestTransferEventVisibleRateLimitPersistsAcrossIntervals(t *testing.T) {
	r := NewNotificationRouter(nil)
	key := "ws|feishu|chat"
	now := time.Date(2026, 10, 15, 14, 56, 0, 0, time.UTC)

	if !r.transferEventVisible(key, transferNoticeStatus("draining"), "progress-1", now) {
		t.Fatal("first progress must be reported")
	}
	if r.transferEventVisible(key, transferNoticeStatus("draining"), "progress-2", now.Add(10*time.Second)) {
		t.Fatal("changed progress inside the interval must be suppressed")
	}
	if !r.transferEventVisible(key, transferNoticeStatus("draining"), "progress-3", now.Add(transferProgressInterval)) {
		t.Fatal("changed progress after the interval must be reported")
	}
	if r.transferEventVisible(key, transferNoticeStatus("draining"), "progress-4", now.Add(transferProgressInterval+time.Second)) {
		t.Fatal("the interval must restart after a reported progress update")
	}
}

// A binding can start a new transfer after the previous one ended. Nothing
// reported for the old transfer, including its terminal state, may suppress
// the new one's first event.
func TestTransferEventVisibleResetsOnNewTransfer(t *testing.T) {
	r := NewNotificationRouter(nil)
	key := "ws|feishu|chat"
	now := time.Date(2026, 10, 15, 14, 56, 0, 0, time.UTC)

	if !r.transferEventVisible(key, transferNoticeStatus("saved"), "saved", now) {
		t.Fatal("terminal state must be reported")
	}
	next := &TransferStatus{TransferID: "transfer-2", SessionID: "session-1", State: "preparing"}
	if !r.transferEventVisible(key, next, "started", now.Add(time.Second)) {
		t.Fatal("a new transfer must report its own first event")
	}
}

// saved and released both close one transfer and render identical text, so
// only one message may reach the chat.
func TestTransferEventVisibleCollapsesTerminalPair(t *testing.T) {
	r := NewNotificationRouter(nil)
	key := "ws|feishu|chat"
	now := time.Date(2026, 10, 15, 14, 56, 0, 0, time.UTC)

	if !r.transferEventVisible(key, transferNoticeStatus("saved"), "saved", now) {
		t.Fatal("saved must be reported")
	}
	if r.transferEventVisible(key, transferNoticeStatus("released"), "saved", now.Add(time.Second)) {
		t.Fatal("released must not repeat an already reported outcome")
	}
}

// A failure carries the recovery action. It must never be rate limited away by
// an unfinished-progress report that arrived moments earlier.
func TestTransferEventVisibleAlwaysReportsFailure(t *testing.T) {
	r := NewNotificationRouter(nil)
	key := "ws|feishu|chat"
	now := time.Date(2026, 10, 15, 14, 56, 0, 0, time.UTC)

	r.transferEventVisible(key, transferNoticeStatus("draining"), "progress", now)
	failed := &TransferStatus{TransferID: "transfer-1", SessionID: "session-1", State: "save_failed", ErrorCode: "persistence_failed", Message: "disk full"}
	if !r.transferEventVisible(key, failed, "failed", now.Add(time.Second)) {
		t.Fatal("a failure must always be reported")
	}
	// The eventual success after a retry is reported too.
	if !r.transferEventVisible(key, transferNoticeStatus("saved"), "saved", now.Add(2*time.Second)) {
		t.Fatal("success after a reported failure must still be reported")
	}
}

func TestTransferEventVisibleRejectsUnidentifiedStatus(t *testing.T) {
	r := NewNotificationRouter(nil)
	now := time.Now()
	if r.transferEventVisible("key", nil, "text", now) {
		t.Fatal("a nil transfer must not be reported")
	}
	if r.transferEventVisible("key", &TransferStatus{State: "draining"}, "text", now) {
		t.Fatal("a transfer without an identity must not be reported")
	}
	if r.transferEventVisible("key", transferNoticeStatus("draining"), "", now) {
		t.Fatal("an empty message must not be reported")
	}
}

func TestFormatTransferNoticeReportsUserVisibleMilestones(t *testing.T) {
	now := time.Date(2026, 10, 15, 14, 56, 30, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		status TransferStatus
		want   []string
	}{
		{"start", TransferStatus{TransferID: "transfer-1", SessionID: "session-1", State: "preparing"}, []string{"takeover started", "session-1", "blocked"}},
		{"saved", TransferStatus{TransferID: "transfer-1", SessionID: "session-1", State: "saved"}, []string{"saved", "transfer-1", "/new", "/resume"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := formatTransferNotice(tc.status, now)
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Fatalf("%q missing from %q", want, text)
				}
			}
		})
	}
}

// Chord publishes no completion estimate, so the wait must be reported as
// observed elapsed time and must never read as a prediction.
func TestFormatTransferProgressReportsElapsedNotEstimate(t *testing.T) {
	started := time.Date(2026, 10, 15, 14, 56, 1, 0, time.UTC)
	status := TransferStatus{TransferID: "transfer-1", SessionID: "session-1", State: "draining", StartedAt: started.Format(time.RFC3339), BlockerCount: 1, Blockers: []TransferBlocker{{Type: "llm_result", Description: "waiting for model result processing"}}}

	text := formatTransferProgress(status, started.Add(90*time.Second), nil)
	for _, want := range []string{"in progress", "waiting 1m30s", "Waiting on 1:", "llm_result"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q missing from %q", want, text)
		}
	}
	for _, unwanted := range []string{"about", "estimate", "approximately"} {
		if strings.Contains(strings.ToLower(text), unwanted) {
			t.Fatalf("progress must not predict a duration: %q", text)
		}
	}
}

// StartedAt is only set once the source froze. Before that the wait is unknown
// and must be omitted rather than rendered as a zero or bogus duration.
func TestFormatTransferProgressOmitsUnknownElapsed(t *testing.T) {
	now := time.Date(2026, 10, 15, 14, 56, 30, 0, time.UTC)
	for _, started := range []string{"", "0001-01-01T00:00:00Z", "not-a-time"} {
		status := TransferStatus{State: "draining", StartedAt: started, BlockerCount: 0}
		text := formatTransferProgress(status, now, nil)
		if strings.Contains(text, "waiting 0s") || strings.Contains(text, "waiting 0") {
			t.Fatalf("unknown wait must be omitted, got %q", text)
		}
		if !strings.Contains(text, "in progress") {
			t.Fatalf("progress must still be reported, got %q", text)
		}
	}
}

// The repeating progress report must stay small; the full list is one command
// away.
func TestFormatTransferProgressBoundsRepeatingReport(t *testing.T) {
	var blockers []TransferBlocker
	for range 9 {
		blockers = append(blockers, TransferBlocker{OwnerTaskID: "worker", Type: "tool", Description: strings.Repeat("waiting ", 40)})
	}
	status := TransferStatus{State: "draining", BlockerCount: len(blockers), Blockers: blockers}

	text := formatTransferProgress(status, time.Now(), nil)
	if got := strings.Count(text, "\nWaiting:"); got != transferProgressBlockers {
		t.Fatalf("preview count = %d, want %d", got, transferProgressBlockers)
	}
	if !strings.Contains(text, "7 more") || !strings.Contains(text, "/takeover status") {
		t.Fatalf("omitted blockers must be counted and retrievable: %q", text)
	}
	if len([]rune(text)) > maxNotificationRunes {
		t.Fatalf("progress report exceeds the IM budget: %d runes", len([]rune(text)))
	}
}

// A failure must keep the full coordinator status, because recovery depends on
// the error code and the next action.
func TestFormatTransferNoticeKeepsFailureDetail(t *testing.T) {
	status := TransferStatus{TransferID: "transfer-1", SessionID: "session-1", State: "save_failed", ErrorCode: "persistence_failed", Message: "Could not save session", RecoverableAction: "retry-save", WaitExpired: true}
	text := formatTransferNotice(status, time.Now())
	for _, want := range []string{"State: save_failed", "persistence_failed", "Next: retry-save", "Wait budget expired", "/takeover status"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q missing from failure notice %q", want, text)
		}
	}
}
