package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/telegram"
)

type telegramPollingAPI struct {
	updates func(context.Context, *int64, int) ([]telegram.Update, error)
	me      telegram.User
	webhook telegram.WebhookInfo
}

func (a *telegramPollingAPI) GetMe(context.Context) (telegram.User, error) { return a.me, nil }
func (a *telegramPollingAPI) GetWebhookInfo(context.Context) (telegram.WebhookInfo, error) {
	return a.webhook, nil
}
func (a *telegramPollingAPI) GetUpdates(ctx context.Context, offset *int64, timeout int) ([]telegram.Update, error) {
	return a.updates(ctx, offset, timeout)
}
func (*telegramPollingAPI) SendMessage(context.Context, int64, string) (telegram.Message, error) {
	return telegram.Message{}, nil
}

func telegramTestUpdate(id int64) telegram.Update {
	return telegram.Update{UpdateID: id, Message: &telegram.Message{MessageID: id + 1, From: &telegram.User{ID: 7}, Chat: telegram.Chat{ID: 7, Type: "private"}, Date: time.Now().Unix(), Text: "task " + strconv.FormatInt(id, 10)}}
}

// The fingerprint that freezes a capture is taken from the bytes Telegram sent,
// so a test that redelivers an update with a different body has to build both
// copies from wire payloads instead of mutating a decoded struct in place.
func telegramWireUpdate(t *testing.T, id int64, text string) telegram.Update {
	t.Helper()
	data := fmt.Sprintf(`{"update_id":%d,"message":{"message_id":%d,"chat":{"id":7,"type":"private"},"from":{"id":7},"date":%d,"text":%q}}`, id, id+1, time.Now().Unix(), text)
	var update telegram.Update
	if err := json.Unmarshal([]byte(data), &update); err != nil {
		t.Fatal(err)
	}
	return update
}
func telegramTestPoller(t *testing.T) (*telegram.TelegramAdapter, *IngressStore, context.Context, context.CancelFunc) {
	t.Helper()
	// These tests assert retry ordering, not pacing; keep the same transitions
	// without one real second per stuck or failed request.
	swapTestValue(t, &telegram.TelegramStuckRetryDelay, time.Millisecond)
	swapTestValue(t, &telegram.TelegramRetryBaseDelay, time.Millisecond)
	store := testIngressStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	a := telegram.NewTelegramAdapterForTest("99", "test_bot", nil, ctx, cancel)
	a.SetCursorStoreForTest(store)
	accept := func(ctx context.Context, e im.IngressEnvelope, reason string) im.ReceiveResult {
		msg := e.Message
		rec := IngressRecord{Platform: msg.IMType, AccountID: msg.AccountID, ChatID: msg.ChatID, SenderID: msg.SenderID, BusinessID: msg.MessageID, SemanticID: e.SemanticID, InputDigest: e.InputDigest, Kind: "task", Text: msg.Text, SourceTime: e.SourceTime, ReceivedAt: e.ReceivedAt}
		if reason != "" {
			rec.State = "rejected"
			rec.Reason = reason
		} else {
			route, err := store.EnsureRoute(ctx, rec.Identity(), "auth", "workspace", "session")
			if err != nil {
				return im.ReceiveResult{Disposition: im.ReceiveRetryable}
			}
			rec.RouteRevision = route.Revision
			rec.AuthorizationDigest = route.AuthorizationDigest
			rec.WorkspaceID = route.WorkspaceID
			rec.SessionID = route.SessionID
		}
		return store.AcceptRecord(ctx, rec, e.Cursor)
	}
	a.SetReceiveForTest(func(ctx context.Context, e im.IngressEnvelope) im.ReceiveResult { return accept(ctx, e, "") })
	a.SetRejectForTest(accept)
	return a, store, ctx, cancel
}
func telegramAssertOffset(t *testing.T, got *int64, want *int64) {
	t.Helper()
	if got == nil && want == nil {
		return
	}
	if got == nil || want == nil || *got != *want {
		t.Fatalf("offset=%v want=%v", got, want)
	}
}
func telegramOffset(n int64) *int64 { return &n }

func TestTelegramPollingBatchConfirmationResponseAndRandomReset(t *testing.T) {
	a, store, ctx, cancel := telegramTestPoller(t)
	calls := 0
	a.SetAPIForTest(&telegramPollingAPI{updates: func(_ context.Context, offset *int64, timeout int) ([]telegram.Update, error) {
		calls++
		switch calls {
		case 1:
			telegramAssertOffset(t, offset, telegramOffset(21))
			if timeout != 0 {
				t.Fatal(timeout)
			}
			return []telegram.Update{telegramTestUpdate(30)}, nil
		case 2:
			telegramAssertOffset(t, offset, telegramOffset(31))
			return nil, nil
		case 3:
			telegramAssertOffset(t, offset, nil)
			if timeout != 25 {
				t.Fatal(timeout)
			}
			return []telegram.Update{telegramTestUpdate(2)}, nil
		case 4:
			telegramAssertOffset(t, offset, telegramOffset(3))
			return nil, nil
		default:
			telegramAssertOffset(t, offset, nil)
			cancel()
			return nil, context.Canceled
		}
	}})
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.PollForTest(ctx, cursor, []telegram.Update{telegramTestUpdate(10), telegramTestUpdate(12), telegramTestUpdate(20)}, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	records, err := store.listAll()
	if err != nil || len(records) != 5 {
		t.Fatalf("records=%v err=%v", records, err)
	}
}

func TestTelegramPollingRetryableDoesNotConfirmPastFailedPrefix(t *testing.T) {
	a, store, ctx, cancel := telegramTestPoller(t)
	receive := a.ReceiveForTest()
	rejectedOnce := false
	deliveries := map[string]int{}
	a.SetReceiveForTest(func(ctx context.Context, e im.IngressEnvelope) im.ReceiveResult {
		if e.Message.MessageID == "update:12" && !rejectedOnce {
			rejectedOnce = true
			return im.ReceiveResult{Disposition: im.ReceiveRetryable}
		}
		r := receive(ctx, e)
		if r.Disposition == im.ReceiveAccepted {
			deliveries[e.Message.MessageID]++
		}
		return r
	})
	calls := 0
	a.SetAPIForTest(&telegramPollingAPI{updates: func(_ context.Context, offset *int64, timeout int) ([]telegram.Update, error) {
		calls++
		switch calls {
		case 1:
			telegramAssertOffset(t, offset, telegramOffset(11))
			return []telegram.Update{telegramTestUpdate(12), telegramTestUpdate(20)}, nil
		case 2:
			telegramAssertOffset(t, offset, telegramOffset(21))
			return nil, nil
		default:
			telegramAssertOffset(t, offset, nil)
			cancel()
			return nil, context.Canceled
		}
	}})
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.PollForTest(ctx, cursor, []telegram.Update{telegramTestUpdate(10), telegramTestUpdate(12), telegramTestUpdate(20)}, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	records, _ := store.listAll()
	if len(records) != 3 || !reflect.DeepEqual(deliveries, map[string]int{"update:10": 1, "update:12": 1, "update:20": 1}) {
		t.Fatalf("records=%v deliveries=%v", records, deliveries)
	}
}

func TestTelegramPollingUncertainCommitReconcilesBeforeConfirmation(t *testing.T) {
	a, store, ctx, cancel := telegramTestPoller(t)
	receive := a.ReceiveForTest()
	delivered := 0
	a.SetReceiveForTest(func(ctx context.Context, e im.IngressEnvelope) im.ReceiveResult {
		result := receive(ctx, e)
		if result.Disposition == im.ReceiveAccepted {
			delivered++
		}
		return im.ReceiveResult{Disposition: im.ReceiveUncertain}
	})
	a.SetAPIForTest(&telegramPollingAPI{updates: func(_ context.Context, offset *int64, _ int) ([]telegram.Update, error) {
		telegramAssertOffset(t, offset, telegramOffset(11))
		cancel()
		return nil, context.Canceled
	}})
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.PollForTest(ctx, cursor, []telegram.Update{telegramTestUpdate(10)}, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	stored, _ := store.Cursor("telegram", "99")
	if delivered != 1 || *stored.Telegram.PendingOffset != 11 {
		t.Fatal(delivered, stored)
	}
}

func TestTelegramCursorRestartFreshExpiredAndClockRollback(t *testing.T) {
	for _, age := range []time.Duration{time.Hour, 24 * time.Hour, -time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			a, store, ctx, _ := telegramTestPoller(t)
			now := time.Now()
			a.SetClockForTest(func() time.Time { return now })
			next := im.IngressCursor{Kind: "telegram", Version: 1, Generation: 1, Telegram: &im.TelegramIngressCursor{BotID: "99", PendingOffset: telegramOffset(12345), PendingCreatedAt: now.Add(-age), ObservedAt: now.Add(-age), State: "pending"}}
			if err := store.CommitCursor(ctx, "telegram", "99", 0, next); err != nil {
				t.Fatal(err)
			}
			cursor, err := a.LoadCursorForTest(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := telegramOffset(12345)
			if age >= 24*time.Hour || age < 0 {
				want = nil
			}
			telegramAssertOffset(t, cursor.Telegram.PendingOffset, want)
		})
	}
}

func TestTelegramPollingLostAcknowledgmentRetainsPendingAndNewResponse(t *testing.T) {
	a, store, ctx, cancel := telegramTestPoller(t)
	calls := 0
	a.SetAPIForTest(&telegramPollingAPI{updates: func(_ context.Context, offset *int64, _ int) ([]telegram.Update, error) {
		calls++
		switch calls {
		case 1:
			telegramAssertOffset(t, offset, telegramOffset(11))
			return nil, &telegram.APIError{Category: "network"}
		case 2:
			telegramAssertOffset(t, offset, telegramOffset(11))
			return []telegram.Update{telegramTestUpdate(12)}, nil
		case 3:
			telegramAssertOffset(t, offset, telegramOffset(13))
			return nil, nil
		default:
			telegramAssertOffset(t, offset, nil)
			cancel()
			return nil, context.Canceled
		}
	}})
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.PollForTest(ctx, cursor, []telegram.Update{telegramTestUpdate(10)}, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	records, _ := store.listAll()
	if len(records) != 2 {
		t.Fatal(records)
	}
}

func TestTelegramPollingUnsupportedAndConflictAreDurable(t *testing.T) {
	a, store, ctx, cancel := telegramTestPoller(t)
	first := telegramWireUpdate(t, 10, "task 10")
	duplicate := telegramWireUpdate(t, 10, "conflicting text")
	results := []im.ReceiveResult{}
	receive := a.ReceiveForTest()
	a.SetReceiveForTest(func(ctx context.Context, e im.IngressEnvelope) im.ReceiveResult {
		r := receive(ctx, e)
		results = append(results, r)
		return r
	})
	calls := 0
	a.SetAPIForTest(&telegramPollingAPI{updates: func(_ context.Context, offset *int64, _ int) ([]telegram.Update, error) {
		calls++
		switch calls {
		case 1:
			telegramAssertOffset(t, offset, telegramOffset(11))
			return []telegram.Update{duplicate, {UpdateID: 12}}, nil
		case 2:
			telegramAssertOffset(t, offset, telegramOffset(13))
			return nil, nil
		default:
			cancel()
			return nil, context.Canceled
		}
	}})
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.PollForTest(ctx, cursor, []telegram.Update{first}, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(results) != 2 || results[1].Reason != "update_id_conflict" {
		t.Fatal(results)
	}
	records, _ := store.listAll()
	if len(records) != 3 {
		t.Fatal(records)
	}
	for _, r := range records {
		if r.State == "rejected" && r.Text != "" {
			t.Fatal("rejected text persisted", r)
		}
	}
}

func TestTelegramPollingRateLimitAndLongPollCancellation(t *testing.T) {
	for _, rateLimit := range []bool{false, true} {
		t.Run(strconv.FormatBool(rateLimit), func(t *testing.T) {
			a, _, ctx, cancel := telegramTestPoller(t)
			entered := make(chan struct{})
			a.SetAPIForTest(&telegramPollingAPI{updates: func(ctx context.Context, _ *int64, _ int) ([]telegram.Update, error) {
				close(entered)
				if rateLimit {
					return nil, &telegram.APIError{Category: "rate_limited", RetryAfter: time.Hour}
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}})
			cursor, _ := a.LoadCursorForTest(ctx)
			done := make(chan error, 1)
			go func() { done <- a.PollForTest(ctx, cursor, nil, false) }()
			<-entered
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("poller did not stop")
			}
		})
	}
}

func TestTelegramPendingExpiresDuringNetworkRetryWithoutOffset(t *testing.T) {
	a, store, ctx, _ := telegramTestPoller(t)
	now := time.Now()
	a.SetClockForTest(func() time.Time { return now })
	stored := im.IngressCursor{Kind: "telegram", Version: 1, Generation: 1, Telegram: &im.TelegramIngressCursor{BotID: a.AccountIDForTest(), PendingOffset: telegramOffset(999999), PendingCreatedAt: now, ObservedAt: now, State: "pending"}}
	if err := store.CommitCursor(ctx, "telegram", a.AccountIDForTest(), 0, stored); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.SetAPIForTest(&telegramPollingAPI{updates: func(_ context.Context, offset *int64, timeout int) ([]telegram.Update, error) {
		calls++
		if calls == 1 {
			telegramAssertOffset(t, offset, telegramOffset(999999))
			if timeout != 0 {
				t.Fatal(timeout)
			}
			now = now.Add(25 * time.Hour)
			return nil, &telegram.APIError{Category: "network"}
		}
		telegramAssertOffset(t, offset, nil)
		if timeout != 25 {
			t.Fatalf("expired pending retry timeout=%d", timeout)
		}
		return nil, nil
	}})
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.PollRequestForTest(ctx, &cursor, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || cursor.Telegram.PendingOffset != nil {
		t.Fatal("expired pending offset survived retry", calls, cursor)
	}
	durable, err := store.Cursor("telegram", a.AccountIDForTest())
	if err != nil || durable.Telegram.PendingOffset != nil || durable.Generation != 2 {
		t.Fatal(durable, err)
	}
}

func TestTelegramUnorderedBatchSortsBeforePrefixAcknowledgment(t *testing.T) {
	a, _, ctx, cancel := telegramTestPoller(t)
	receive := a.ReceiveForTest()
	order := []string{}
	a.SetReceiveForTest(func(ctx context.Context, e im.IngressEnvelope) im.ReceiveResult {
		order = append(order, e.Message.MessageID)
		if e.Message.MessageID == "update:12" {
			return im.ReceiveResult{Disposition: im.ReceiveRetryable}
		}
		return receive(ctx, e)
	})
	a.SetAPIForTest(&telegramPollingAPI{updates: func(_ context.Context, offset *int64, timeout int) ([]telegram.Update, error) {
		telegramAssertOffset(t, offset, telegramOffset(11))
		if timeout != 0 {
			t.Fatal(timeout)
		}
		cancel()
		return nil, context.Canceled
	}})
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.PollForTest(ctx, cursor, []telegram.Update{telegramTestUpdate(20), telegramTestUpdate(12), telegramTestUpdate(10)}, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"update:10", "update:12"}) {
		t.Fatalf("admission order=%v", order)
	}
}

// A non-terminal disposition must retry the same update instead of advancing
// the offset past unacknowledged input. Before the escalation warning this
// loop was indistinguishable from an idle poller, which is how a stuck
// Telegram platform stayed invisible.
func TestTelegramUnacknowledgedUpdateRetriesWithoutAdvancingOffset(t *testing.T) {
	a, _, ctx, cancel := telegramTestPoller(t)
	var attempts []string
	a.SetReceiveForTest(func(_ context.Context, e im.IngressEnvelope) im.ReceiveResult {
		attempts = append(attempts, e.Message.MessageID)
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "route_busy"}
	})
	var offsets []int64
	a.SetAPIForTest(&telegramPollingAPI{updates: func(_ context.Context, offset *int64, timeout int) ([]telegram.Update, error) {
		if offset != nil {
			offsets = append(offsets, *offset)
		}
		if len(attempts) >= 3 {
			cancel()
			return nil, context.Canceled
		}
		return []telegram.Update{telegramTestUpdate(10)}, nil
	}})
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.PollForTest(ctx, cursor, []telegram.Update{telegramTestUpdate(10)}, true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// The retry must re-offer the same update, never a later one.
	if len(attempts) < 2 {
		t.Fatalf("unacknowledged update was not retried: %v", attempts)
	}
	for _, id := range attempts {
		if id != "update:10" {
			t.Fatalf("retry advanced past unacknowledged update: %v", attempts)
		}
	}
}

// The retry warning is the only signal that distinguishes a wedged poller from
// an idle one, so it must actually be emitted rather than silently retried.
func TestTelegramUnacknowledgedUpdateWarnsOnce(t *testing.T) {
	a, _, ctx, cancel := telegramTestPoller(t)
	attempts := 0
	a.SetReceiveForTest(func(_ context.Context, _ im.IngressEnvelope) im.ReceiveResult {
		attempts++
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "route_busy"}
	})
	a.SetAPIForTest(&telegramPollingAPI{updates: func(_ context.Context, _ *int64, _ int) ([]telegram.Update, error) {
		if attempts >= 2 {
			cancel()
			return nil, context.Canceled
		}
		return []telegram.Update{telegramTestUpdate(10)}, nil
	}})
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var warnings []string
	restore := log.Warnf
	log.Warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { log.Warnf = restore })
	if err = a.PollForTest(ctx, cursor, []telegram.Update{telegramTestUpdate(10)}, true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(warnings) == 0 {
		t.Fatal("retried an unacknowledged update without any warning")
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "not acknowledged") || !strings.Contains(joined, "update_id=10") {
		t.Fatalf("warning lacks the stuck update identity: %q", joined)
	}
}

// Stopping the poller discards everything the current batch had not dispatched
// yet, so the stop must be reported with the update that was reached instead of
// looking like an idle poller.
func TestTelegramPollingStopAfterDispatchIsReported(t *testing.T) {
	a, _, ctx, _ := telegramTestPoller(t)
	a.SetAPIForTest(&telegramPollingAPI{updates: func(_ context.Context, offset *int64, _ int) ([]telegram.Update, error) {
		telegramAssertOffset(t, offset, telegramOffset(11))
		return nil, &telegram.APIError{Method: "getUpdates", Status: 400, Category: "method_rejected"}
	}})
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var warnings []string
	restore := log.Warnf
	log.Warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { log.Warnf = restore })
	if err = a.PollForTest(ctx, cursor, []telegram.Update{telegramTestUpdate(10)}, true); err == nil {
		t.Fatal("terminal method rejection was retried")
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "polling stopped") || !strings.Contains(joined, "update_id=10") {
		t.Fatalf("batch loss was not reported with its update id: %q", joined)
	}
}

// A poison batch element must not discard the rest of the batch silently. It is
// reported and then treated like the other non-terminal outcomes, so the offset
// stays behind it and the batch is re-offered instead of dropped.
func TestTelegramPollingInvalidBatchElementIsReportedAndRetried(t *testing.T) {
	a, _, ctx, cancel := telegramTestPoller(t)
	var warnings []string
	restore := log.Warnf
	log.Warnf = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
		// Reporting happens before the retry wait, so cancelling here makes the
		// re-offer deterministic without a second request.
		cancel()
	}
	t.Cleanup(func() { log.Warnf = restore })
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	poison := []telegram.Update{{UpdateID: math.MaxInt64}}
	if err = a.PollForTest(ctx, cursor, poison, true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "invalid_update_identity") || !strings.Contains(joined, "batch element rejected") {
		t.Fatalf("invalid batch element was not reported: %q", joined)
	}
	if !strings.Contains(joined, "not acknowledged") {
		t.Fatalf("invalid batch element was not treated as a non-terminal outcome: %q", joined)
	}
}

// A poller that keeps failing without logging is indistinguishable from an idle
// one, so the retry must be reported with the method and the error category.
func TestTelegramPollRequestRetryWarningNamesMethodAndCategory(t *testing.T) {
	a, _, ctx, cancel := telegramTestPoller(t)
	var warnings []string
	restore := log.Warnf
	log.Warnf = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
		// The warning is emitted before the retry wait, so cancelling here keeps
		// the loop deterministic without a second request.
		cancel()
	}
	t.Cleanup(func() { log.Warnf = restore })
	a.SetAPIForTest(&telegramPollingAPI{updates: func(_ context.Context, offset *int64, timeout int) ([]telegram.Update, error) {
		telegramAssertOffset(t, offset, nil)
		if timeout != telegram.LongPollSecondsForTest() {
			t.Fatalf("timeout = %d", timeout)
		}
		return nil, &telegram.APIError{Method: "getUpdates", Status: 502, Category: "invalid_response"}
	}})
	cursor, err := a.LoadCursorForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.PollRequestForTest(ctx, &cursor, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "getUpdates") || !strings.Contains(joined, "invalid_response") || !strings.Contains(joined, "retrying in") {
		t.Fatalf("retry was not reported: %q", joined)
	}
}
