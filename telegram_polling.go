package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/internal/im"
)

type telegramClock func() time.Time

// telegramStuckReportInterval bounds how often a permanently unacknowledged
// update repeats its warning before the next escalation. Time-based, not
// count-based: a long-polling loop and a tight retry loop must warn at the same
// cadence.
const telegramStuckReportInterval = time.Minute

// telegramStuckRetryDelay paces retries of an unacknowledged update. It is a
// variable so tests can exercise the escalation without one real second per
// retry; production keeps one second.
var telegramStuckRetryDelay = time.Second

const (
	// Telegram caps the server-side long poll at 25 seconds; a longer request
	// is rejected. The client read bound adds an allowance on top of it.
	telegramLongPollSeconds = 25
	// getUpdates returns at most 100 updates per call and rejects a larger
	// limit, so the cap is sent explicitly instead of relying on the default.
	telegramPollLimit = 100
	// Without allowed_updates Telegram also delivers edited messages and
	// channel posts, which this adapter refuses to process. Naming the one
	// supported update type keeps refused input off the wire entirely.
	telegramAllowedUpdates = "message"
)

var realTelegramClock telegramClock = time.Now

type telegramCursorStore interface {
	Cursor(string, string) (im.IngressCursor, error)
	CommitCursor(context.Context, string, string, uint64, im.IngressCursor) error
}

func (a *TelegramAdapter) loadCursor(ctx context.Context) (im.IngressCursor, error) {
	cursor, err := a.store.Cursor("telegram", a.accountID)
	if err != nil {
		return cursor, err
	}
	if cursor.Generation == 0 {
		cursor = im.IngressCursor{Kind: "telegram", Version: 1, Telegram: &im.TelegramIngressCursor{BotID: a.accountID, State: "empty"}}
	}
	if cursor.Telegram == nil {
		return cursor, errors.New("telegram invalid stored cursor")
	}
	t := cursor.Telegram
	now := a.now()
	created := t.PendingCreatedAt
	// Legacy checkpoints cannot establish when the acknowledgement was created.
	untrusted := t.BotID != a.accountID || created.IsZero() || created.After(now) || now.Sub(created) >= 24*time.Hour
	if t.PendingOffset != nil && untrusted {
		next := *im.CloneIngressCursor(&cursor)
		next.Telegram.PendingOffset = nil
		next.Telegram.PendingCreatedAt = time.Time{}
		next.Telegram.State = "empty"
		next.Telegram.BotID = a.accountID
		if err := a.commitCursor(ctx, &cursor, next); err != nil {
			return cursor, err
		}
	}
	return cursor, nil
}
func (a *TelegramAdapter) commitCursor(ctx context.Context, current *im.IngressCursor, next im.IngressCursor) error {
	next.Generation = current.Generation + 1
	if err := a.store.CommitCursor(ctx, "telegram", a.accountID, current.Generation, next); err != nil {
		return err
	}
	*current = next
	return nil
}
func (a *TelegramAdapter) pollRequest(ctx context.Context, cursor *im.IngressCursor, probe bool) ([]telegramUpdate, error) {
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		timeout := telegramLongPollSeconds
		if probe || cursor.Telegram.PendingOffset != nil {
			timeout = 0
		}
		batch, err := a.api.GetUpdates(ctx, cursor.Telegram.PendingOffset, timeout)
		if err == nil {
			a.retry.succeeded(time.Now())
			a.retryWarn = time.Time{}
			if len(batch) > 0 {
				var offset int64
				if cursor.Telegram.PendingOffset != nil {
					offset = *cursor.Telegram.PendingOffset
				}
				log.Debugf("telegram: received updates count=%v from_offset=%v first_id=%v", len(batch), offset, batch[0].UpdateID)
			}
			a.captureBatch(batch)
			return batch, nil
		}
		delay, retry := a.retry.failed(err)
		if !retry {
			return nil, err
		}
		a.warnRetry(err, delay)
		if !telegramWait(ctx, delay) {
			return nil, ctx.Err()
		}
		// Re-evaluate a pending acknowledgement after every failed request:
		// transport uncertainty must not keep an expired offset alive forever.
		refreshed, loadErr := a.loadCursor(ctx)
		if loadErr != nil {
			return nil, loadErr
		}
		*cursor = refreshed
	}
}

type telegramCapturedUpdate struct {
	fingerprint string
	envelope    im.IngressEnvelope
	reason      string
}

// telegramCaptureKind reports the ingress kind frozen for this update, or ""
// when the update was refused before capture.
func telegramCaptureKind(e *im.IngressEnvelope) string {
	if e == nil || e.Capture == nil {
		return ""
	}
	return e.Capture.Kind
}

// Freeze the entire response before readiness, checkpoint writes or admission
// can wait. Telegram returns at most 100 updates; only this bounded batch is
// retained. Identical redeliveries keep their first capture and ReceivedAt.
func (a *TelegramAdapter) captureBatch(batch []telegramUpdate) {
	captured := make(map[int64]telegramCapturedUpdate, len(batch))
	for i := range batch {
		u := &batch[i]
		input, exists := a.captured[u.UpdateID]
		if !exists || input.fingerprint != u.receivedDigest {
			e, reason := a.normalizeUpdate(*u)
			if reason == "" && a.router != nil {
				// Polling owns the complete batch and can wait for the snapshot.
				// A transient busy refusal must not poison retained redeliveries.
				kind, target := a.router.CaptureIngress(e.Message)
				e.Capture = &im.IngressCapture{Kind: kind, Target: target}
				if e.Capture.Kind == "control" {
					e.InputDigest = ""
				}
			}
			// Without this, a message the gateway never accepts is
			// indistinguishable from one Telegram never delivered.
			log.Debugf("telegram: captured update id=%v chat_id=%v chat_kind=%v reason=%v kind=%v", u.UpdateID, e.Message.ChatID, e.Message.ChatKind, reason, telegramCaptureKind(&e))
			input = telegramCapturedUpdate{u.receivedDigest, e, reason}
		}
		captured[u.UpdateID] = input
		u.captured = &input
	}
	a.captured = captured
}
func telegramWait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// telegramLastObservedUpdateID names the last update the cursor recorded, so a
// reported batch loss points at input instead of vanishing without an identity.
func telegramLastObservedUpdateID(cursor im.IngressCursor) string {
	if cursor.Telegram == nil || cursor.Telegram.LastObservedUpdateID == nil {
		return "unknown"
	}
	return strconv.FormatInt(*cursor.Telegram.LastObservedUpdateID, 10)
}

// poll confirms only the in-memory prefix of the current batch. Empty polls
// never synthesize an offset from a historical maximum update ID.
func (a *TelegramAdapter) poll(ctx context.Context, cursor im.IngressCursor, batch []telegramUpdate, hasResponse bool) error {
	if hasResponse {
		a.captureBatch(batch)
	}
	// Consecutive unacknowledged retries for the same stuck update. Declared
	// outside the loop so the escalation state actually accumulates instead of
	// restarting at one and warning on every pass. A new update ID resets the
	// count so one poison update cannot misattribute its history to the next.
	failures := 0
	var stuckUpdateID int64
	var stuckReason string
	var lastStuckWarn time.Time
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !hasResponse {
			var err error
			batch, err = a.pollRequest(ctx, &cursor, false)
			if err != nil {
				// Everything not yet dispatched by this call is lost, so the
				// stop must be visible instead of looking like an idle poller.
				log.Warnf("telegram: polling stopped update_id=%v error=%v", telegramLastObservedUpdateID(cursor), err)
				return err
			}
		}
		hasResponse = false
		if cursor.Telegram.PendingOffset != nil {
			next := *im.CloneIngressCursor(&cursor)
			next.Telegram.PendingOffset = nil
			next.Telegram.PendingCreatedAt = time.Time{}
			next.Telegram.State = "confirmed"
			if err := a.commitCursor(ctx, &cursor, next); err != nil {
				log.Warnf("telegram: batch abandoned update_id=%v error=%v", telegramLastObservedUpdateID(cursor), err)
				return err
			}
		}
		failed := false
		// Process the actual server batch in ID order, without waiting for
		// missing integers or admitting a smaller ID behind a larger ACK.
		slices.SortStableFunc(batch, func(a, b telegramUpdate) int { return cmp.Compare(a.UpdateID, b.UpdateID) })
		for index, update := range batch {
			if update.UpdateID < 0 || update.UpdateID == math.MaxInt64 {
				// A malformed element must not discard the rest of the batch
				// silently: report it and fall through to the same non-terminal
				// path as an unacknowledged update, so the offset stays behind
				// this update and the batch is re-offered instead of dropped.
				log.Warnf("telegram: batch element rejected batch_index=%v batch_size=%v reason=invalid_update_identity", index, len(batch))
				stuckUpdateID, stuckReason = update.UpdateID, "invalid_update_identity"
				failed = true
				break
			}
			next := im.CloneIngressCursor(&cursor)
			next.Generation = cursor.Generation + 1
			offset := update.UpdateID + 1
			next.Telegram.PendingOffset = &offset
			next.Telegram.PendingCreatedAt = a.now()
			next.Telegram.ObservedAt = next.Telegram.PendingCreatedAt
			next.Telegram.LastObservedAt = next.Telegram.PendingCreatedAt
			observed := update.UpdateID
			next.Telegram.LastObservedUpdateID = &observed
			next.Telegram.BotID = a.accountID
			next.Telegram.State = "pending"
			envelope, reason := update.captured.envelope, update.captured.reason
			next.Telegram.LastUpdateSourceTime = envelope.SourceTime
			envelope.Cursor = next
			var result im.ReceiveResult
			if reason != "" {
				result = a.reject(ctx, envelope, reason)
			} else {
				result = a.receive(ctx, envelope)
			}
			if result.Reason == "update_id_conflict" {
				log.Warnf("telegram: update_id_conflict; conflicting input quarantined")
			}
			// A non-terminal disposition is retried forever; make it visible.
			log.Debugf("telegram: dispatch update id=%v disposition=%v reason=%v record_id=%v", update.UpdateID, result.Disposition, result.Reason, result.RecordID)
			// A permanent refusal counts only when a in-memory tombstone was returned.
			if (result.Disposition == im.ReceiveAccepted || result.Disposition == im.ReceiveDuplicate || result.Disposition == im.ReceiveRejected) && result.RecordID != "" {
				cursor = *next
				failures = 0
			} else {
				if update.UpdateID != stuckUpdateID {
					failures = 0
					stuckUpdateID, stuckReason = update.UpdateID, result.Reason
				} else {
					stuckReason = result.Reason
				}
				failed = true
				break
			}
		}
		if failed {
			// Unconfirmed acceptance must settle before polling can advance:
			// advancing the offset past an unconfirmed update would silently
			// drop it, so retrying in order is correct. But an update that never
			// reaches a terminal disposition blocks the whole intake behind one
			// poison update, so escalate loudly instead of appearing idle.
			// Ordering is preserved by design; use /status and the gateway log
			// to clear the blocker (pairing, capacity, binding) rather than
			// expecting the gateway to skip ahead.
			failures++
			if failures == 1 || time.Since(lastStuckWarn) >= telegramStuckReportInterval {
				log.Warnf("telegram: update not acknowledged, retrying update_id=%v consecutive_failures=%v reason=%v", stuckUpdateID, failures, stuckReason)
				lastStuckWarn = time.Now()
			}
			if a.router != nil {
				a.router.ObserveIngress("telegram", "acceptance_unconfirmed", stuckReason)
			}
			var err error
			cursor, err = a.loadCursor(ctx)
			if err != nil {
				log.Warnf("telegram: batch abandoned update_id=%v error=%v", stuckUpdateID, err)
				return err
			}
			if !telegramWait(ctx, telegramStuckRetryDelay) {
				return ctx.Err()
			}
		}
	}
}

func telegramDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
func (a *TelegramAdapter) normalizeUpdate(update telegramUpdate) (im.IngressEnvelope, string) {
	received := a.now()
	msg := im.IncomingMessage{IMType: "telegram", AccountID: a.accountID, ChatID: "unsupported-update", MessageID: "update:" + strconv.FormatInt(update.UpdateID, 10)}
	e := im.IngressEnvelope{Message: msg, DeliveryID: msg.MessageID, ReceivedAt: received}
	if update.Message == nil {
		return e, "unsupported_update"
	}
	m := update.Message
	if m.Chat.ID != 0 {
		e.Message.ChatID = strconv.FormatInt(m.Chat.ID, 10)
	}
	if m.Date > 0 {
		e.SourceTime = time.Unix(m.Date, 0)
	}
	if m.Chat.ID != 0 && m.MessageID > 0 {
		// A dropped SemanticID would silently degrade semantic dedup to
		// business-ID dedup, so refuse the message instead.
		semanticID, err := im.EncodeScopedID("message:", e.Message.ChatID, strconv.FormatInt(m.MessageID, 10))
		if err != nil {
			return e, "invalid_target"
		}
		e.SemanticID = semanticID
	}
	if m.From != nil {
		e.Message.SenderID = strconv.FormatInt(m.From.ID, 10)
	}
	switch m.Chat.Type {
	case "private":
		e.Message.ChatKind = "private"
	case "group", "supergroup":
		e.Message.ChatKind = "group"
	default:
		return e, "unsupported_chat"
	}
	if m.MessageThreadID != 0 || m.hasMessageThreadID || m.IsTopicMessage {
		return e, "unsupported_topic"
	}
	if m.SenderChat != nil {
		return e, "unsupported_sender_chat"
	}
	if m.From == nil || m.From.ID <= 0 || m.From.IsBot || m.MessageID <= 0 || m.Date <= 0 || m.Chat.ID == 0 {
		return e, "invalid_identity"
	}
	if e.Message.ChatKind == "private" && (m.Chat.ID <= 0 || m.Chat.ID != m.From.ID) || e.Message.ChatKind == "group" && m.Chat.ID >= 0 {
		return e, "invalid_identity"
	}
	// A media update also carries its caption in `text`, so content presence is
	// checked before the text is admitted as a task.
	if m.hasMedia {
		return e, "unsupported_content"
	}
	if m.Text == "" {
		return e, "unsupported_content"
	}
	if !utf8.ValidString(m.Text) {
		return e, "invalid_text"
	}
	text, reason := telegramNormalizeCommand(m.Text, m.Entities, a.username)
	if reason != "" {
		return e, reason
	}
	e.Message.Text = text
	// Only ordinary text has a in-memory body fingerprint. Commands (including
	// rejected or unsupported inputs) must never persist parameter hashes.
	trimmed := strings.TrimSpace(m.Text)
	if !strings.HasPrefix(trimmed, "/") && !strings.HasPrefix(trimmed, "!") && !strings.HasPrefix(trimmed, "！") {
		e.InputDigest = telegramDigest(m.Text)
	}
	return e, ""
}

// Entity offsets count UTF-16 units. A boundary inside a surrogate pair is
// invalid and must never be converted into a byte offset.
func telegramUTF16Boundary(s string, units int) (int, bool) {
	if units < 0 {
		return 0, false
	}
	count := 0
	for index, r := range s {
		if count == units {
			return index, true
		}
		width := 1
		if r > 0xffff {
			width = 2
		}
		count += width
		if count > units {
			return 0, false
		}
	}
	return len(s), count == units
}
func telegramNormalizeCommand(text string, entities []telegramEntity, username string) (string, string) {
	for _, entity := range entities {
		if entity.Type != "bot_command" || entity.Offset != 0 {
			continue
		}
		end, valid := telegramUTF16Boundary(text, entity.Length)
		if !valid || end == 0 || text[0] != '/' {
			return "", "invalid_command_entity"
		}
		token := text[:end]
		if strings.ContainsAny(token, " \t\r\n") {
			return "", "invalid_command_entity"
		}
		at := strings.IndexByte(token, '@')
		if at < 0 {
			// A legacy hyphenated command has only its prefix marked as a
			// Telegram command. Reject suffixed forms rather than forwarding
			// them as model input; generated hints use supported aliases.
			word, _, _ := strings.Cut(text, " ")
			name, _, suffixed := strings.Cut(word, "@")
			if suffixed && im.TelegramCommandAlias(name, true) != name {
				return "", "invalid_command_entity"
			}
			return im.TelegramCommandAlias(token, false) + text[end:], ""
		}
		if !strings.EqualFold(token[at+1:], username) || username == "" {
			return "", "other_bot_command"
		}
		// Telegram marks the whole command, suffix included, as one entity, so
		// the suffixed shape reaches this branch too. The legacy hyphenated
		// form is still not an alias here and must not become a real command.
		if im.TelegramCommandAlias(token[:at], true) != token[:at] {
			return "", "invalid_command_entity"
		}
		return im.TelegramCommandAlias(token[:at], false) + text[end:], ""
	}
	return text, ""
}

var _ im.IMAdapter = (*TelegramAdapter)(nil)
