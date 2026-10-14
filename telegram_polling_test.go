package main

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"
)

type telegramPollingAPI struct {
	updates func(context.Context, *int64, int) ([]telegramUpdate, error)
	me      telegramUser
	webhook telegramWebhookInfo
}

func (a *telegramPollingAPI) GetMe(context.Context) (telegramUser, error) { return a.me, nil }
func (a *telegramPollingAPI) GetWebhookInfo(context.Context) (telegramWebhookInfo, error) {
	return a.webhook, nil
}
func (a *telegramPollingAPI) GetUpdates(ctx context.Context, offset *int64, timeout int) ([]telegramUpdate, error) {
	return a.updates(ctx, offset, timeout)
}
func (*telegramPollingAPI) SendMessage(context.Context, int64, string) (telegramMessage, error) {
	return telegramMessage{}, nil
}

func telegramTestUpdate(id int64) telegramUpdate {
	return telegramUpdate{UpdateID: id, Message: &telegramMessage{MessageID: id + 1, From: &telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}, Date: time.Now().Unix(), Text: "task " + strconv.FormatInt(id, 10)}}
}
func telegramTestPoller(t *testing.T) (*TelegramAdapter, *IngressStore, context.Context, context.CancelFunc) {
	t.Helper()
	store := testIngressStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	a := &TelegramAdapter{accountID: "99", username: "test_bot", store: store, now: realTelegramClock, ctx: ctx, cancel: cancel}
	accept := func(ctx context.Context, e IngressEnvelope, reason string) ReceiveResult {
		msg := e.Message
		rec := IngressRecord{Platform: msg.IMType, AccountID: msg.AccountID, ChatID: msg.ChatID, SenderID: msg.SenderID, BusinessID: msg.MessageID, SemanticID: e.SemanticID, InputDigest: e.InputDigest, Kind: "task", Text: msg.Text, SourceTime: e.SourceTime, ReceivedAt: e.ReceivedAt}
		if reason != "" {
			rec.State = "rejected"
			rec.Reason = reason
		} else {
			route, err := store.EnsureRoute(ctx, rec.Identity(), "auth", "workspace", "session")
			if err != nil {
				return ReceiveResult{Disposition: ReceiveRetryable}
			}
			rec.RouteRevision = route.Revision
			rec.AuthorizationDigest = route.AuthorizationDigest
			rec.WorkspaceID = route.WorkspaceID
			rec.SessionID = route.SessionID
		}
		return store.AcceptRecord(ctx, rec, e.Cursor)
	}
	a.receive = func(ctx context.Context, e IngressEnvelope) ReceiveResult { return accept(ctx, e, "") }
	a.reject = accept
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
	a.api = &telegramPollingAPI{updates: func(_ context.Context, offset *int64, timeout int) ([]telegramUpdate, error) {
		calls++
		switch calls {
		case 1:
			telegramAssertOffset(t, offset, telegramOffset(21))
			if timeout != 0 {
				t.Fatal(timeout)
			}
			return []telegramUpdate{telegramTestUpdate(30)}, nil
		case 2:
			telegramAssertOffset(t, offset, telegramOffset(31))
			return nil, nil
		case 3:
			telegramAssertOffset(t, offset, nil)
			if timeout != 25 {
				t.Fatal(timeout)
			}
			return []telegramUpdate{telegramTestUpdate(2)}, nil
		case 4:
			telegramAssertOffset(t, offset, telegramOffset(3))
			return nil, nil
		default:
			telegramAssertOffset(t, offset, nil)
			cancel()
			return nil, context.Canceled
		}
	}}
	cursor, err := a.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.poll(ctx, cursor, []telegramUpdate{telegramTestUpdate(10), telegramTestUpdate(12), telegramTestUpdate(20)}, true)
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
	receive := a.receive
	rejectedOnce := false
	deliveries := map[string]int{}
	a.receive = func(ctx context.Context, e IngressEnvelope) ReceiveResult {
		if e.Message.MessageID == "update:12" && !rejectedOnce {
			rejectedOnce = true
			return ReceiveResult{Disposition: ReceiveRetryable}
		}
		r := receive(ctx, e)
		if r.Disposition == ReceiveAccepted {
			deliveries[e.Message.MessageID]++
		}
		return r
	}
	calls := 0
	a.api = &telegramPollingAPI{updates: func(_ context.Context, offset *int64, timeout int) ([]telegramUpdate, error) {
		calls++
		switch calls {
		case 1:
			telegramAssertOffset(t, offset, telegramOffset(11))
			return []telegramUpdate{telegramTestUpdate(12), telegramTestUpdate(20)}, nil
		case 2:
			telegramAssertOffset(t, offset, telegramOffset(21))
			return nil, nil
		default:
			telegramAssertOffset(t, offset, nil)
			cancel()
			return nil, context.Canceled
		}
	}}
	cursor, err := a.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.poll(ctx, cursor, []telegramUpdate{telegramTestUpdate(10), telegramTestUpdate(12), telegramTestUpdate(20)}, true)
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
	receive := a.receive
	delivered := 0
	a.receive = func(ctx context.Context, e IngressEnvelope) ReceiveResult {
		result := receive(ctx, e)
		if result.Disposition == ReceiveAccepted {
			delivered++
		}
		return ReceiveResult{Disposition: ReceiveUncertain}
	}
	a.api = &telegramPollingAPI{updates: func(_ context.Context, offset *int64, _ int) ([]telegramUpdate, error) {
		telegramAssertOffset(t, offset, telegramOffset(11))
		cancel()
		return nil, context.Canceled
	}}
	cursor, err := a.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.poll(ctx, cursor, []telegramUpdate{telegramTestUpdate(10)}, true)
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
			a.now = func() time.Time { return now }
			next := IngressCursor{Kind: "telegram", Version: 1, Generation: 1, Telegram: &TelegramIngressCursor{BotID: "99", PendingOffset: telegramOffset(12345), PendingCreatedAt: now.Add(-age), ObservedAt: now.Add(-age), State: "pending"}}
			if err := store.CommitCursor(ctx, "telegram", "99", 0, next); err != nil {
				t.Fatal(err)
			}
			cursor, err := a.loadCursor(ctx)
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
	a.api = &telegramPollingAPI{updates: func(_ context.Context, offset *int64, _ int) ([]telegramUpdate, error) {
		calls++
		switch calls {
		case 1:
			telegramAssertOffset(t, offset, telegramOffset(11))
			return nil, &telegramAPIError{Category: "network"}
		case 2:
			telegramAssertOffset(t, offset, telegramOffset(11))
			return []telegramUpdate{telegramTestUpdate(12)}, nil
		case 3:
			telegramAssertOffset(t, offset, telegramOffset(13))
			return nil, nil
		default:
			telegramAssertOffset(t, offset, nil)
			cancel()
			return nil, context.Canceled
		}
	}}
	cursor, err := a.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.poll(ctx, cursor, []telegramUpdate{telegramTestUpdate(10)}, true)
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
	first := telegramTestUpdate(10)
	duplicate := telegramTestUpdate(10)
	duplicate.Message.Text = "conflicting text"
	results := []ReceiveResult{}
	receive := a.receive
	a.receive = func(ctx context.Context, e IngressEnvelope) ReceiveResult {
		r := receive(ctx, e)
		results = append(results, r)
		return r
	}
	calls := 0
	a.api = &telegramPollingAPI{updates: func(_ context.Context, offset *int64, _ int) ([]telegramUpdate, error) {
		calls++
		switch calls {
		case 1:
			telegramAssertOffset(t, offset, telegramOffset(11))
			return []telegramUpdate{duplicate, {UpdateID: 12}}, nil
		case 2:
			telegramAssertOffset(t, offset, telegramOffset(13))
			return nil, nil
		default:
			cancel()
			return nil, context.Canceled
		}
	}}
	cursor, err := a.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.poll(ctx, cursor, []telegramUpdate{first}, true)
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
			a.api = &telegramPollingAPI{updates: func(ctx context.Context, _ *int64, _ int) ([]telegramUpdate, error) {
				close(entered)
				if rateLimit {
					return nil, &telegramAPIError{Category: "rate_limited", RetryAfter: time.Hour}
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			cursor, _ := a.loadCursor(ctx)
			done := make(chan error, 1)
			go func() { done <- a.poll(ctx, cursor, nil, false) }()
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
	a.now = func() time.Time { return now }
	stored := IngressCursor{Kind: "telegram", Version: 1, Generation: 1, Telegram: &TelegramIngressCursor{BotID: a.accountID, PendingOffset: telegramOffset(999999), PendingCreatedAt: now, ObservedAt: now, State: "pending"}}
	if err := store.CommitCursor(ctx, "telegram", a.accountID, 0, stored); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.api = &telegramPollingAPI{updates: func(_ context.Context, offset *int64, timeout int) ([]telegramUpdate, error) {
		calls++
		if calls == 1 {
			telegramAssertOffset(t, offset, telegramOffset(999999))
			if timeout != 0 {
				t.Fatal(timeout)
			}
			now = now.Add(25 * time.Hour)
			return nil, &telegramAPIError{Category: "network"}
		}
		telegramAssertOffset(t, offset, nil)
		if timeout != 25 {
			t.Fatalf("expired pending retry timeout=%d", timeout)
		}
		return nil, nil
	}}
	cursor, err := a.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.pollRequest(ctx, &cursor, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || cursor.Telegram.PendingOffset != nil {
		t.Fatal("expired pending offset survived retry", calls, cursor)
	}
	durable, err := store.Cursor("telegram", a.accountID)
	if err != nil || durable.Telegram.PendingOffset != nil || durable.Generation != 2 {
		t.Fatal(durable, err)
	}
}

func TestTelegramUnorderedBatchSortsBeforePrefixAcknowledgment(t *testing.T) {
	a, _, ctx, cancel := telegramTestPoller(t)
	receive := a.receive
	order := []string{}
	a.receive = func(ctx context.Context, e IngressEnvelope) ReceiveResult {
		order = append(order, e.Message.MessageID)
		if e.Message.MessageID == "update:12" {
			return ReceiveResult{Disposition: ReceiveRetryable}
		}
		return receive(ctx, e)
	}
	a.api = &telegramPollingAPI{updates: func(_ context.Context, offset *int64, timeout int) ([]telegramUpdate, error) {
		telegramAssertOffset(t, offset, telegramOffset(11))
		if timeout != 0 {
			t.Fatal(timeout)
		}
		cancel()
		return nil, context.Canceled
	}}
	cursor, err := a.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = a.poll(ctx, cursor, []telegramUpdate{telegramTestUpdate(20), telegramTestUpdate(12), telegramTestUpdate(10)}, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"update:10", "update:12"}) {
		t.Fatalf("admission order=%v", order)
	}
}
