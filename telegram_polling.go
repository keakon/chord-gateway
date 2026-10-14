package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/keakon/golog/log"
)

type telegramClock func() time.Time

var realTelegramClock telegramClock = time.Now

type telegramCursorStore interface {
	Cursor(string, string) (IngressCursor, error)
	CommitCursor(context.Context, string, string, uint64, IngressCursor) error
}

func (a *TelegramAdapter) loadCursor(ctx context.Context) (IngressCursor, error) {
	cursor, err := a.store.Cursor("telegram", a.accountID)
	if err != nil {
		return cursor, err
	}
	if cursor.Generation == 0 {
		cursor = IngressCursor{Kind: "telegram", Version: 1, Telegram: &TelegramIngressCursor{BotID: a.accountID, State: "empty"}}
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
		next := *cloneIngressCursor(&cursor)
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
func (a *TelegramAdapter) commitCursor(ctx context.Context, current *IngressCursor, next IngressCursor) error {
	next.Generation = current.Generation + 1
	if err := a.store.CommitCursor(ctx, "telegram", a.accountID, current.Generation, next); err != nil {
		return err
	}
	*current = next
	return nil
}
func (a *TelegramAdapter) pollRequest(ctx context.Context, cursor *IngressCursor, probe bool) ([]telegramUpdate, error) {
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		timeout := 25
		if probe || cursor.Telegram.PendingOffset != nil {
			timeout = 0
		}
		batch, err := a.api.GetUpdates(ctx, cursor.Telegram.PendingOffset, timeout)
		if err == nil {
			a.retry.succeeded(time.Now())
			a.captureBatch(batch)
			return batch, nil
		}
		delay, retry := a.retry.failed(err)
		if !retry {
			return nil, err
		}
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
	envelope    IngressEnvelope
	reason      string
}

// Freeze the entire response before readiness, checkpoint writes or admission
// can wait. Telegram returns at most 100 updates; only this bounded batch is
// retained. Identical redeliveries keep their first capture and ReceivedAt.
func (a *TelegramAdapter) captureBatch(batch []telegramUpdate) {
	captured := make(map[int64]telegramCapturedUpdate, len(batch))
	for i := range batch {
		u := &batch[i]
		fingerprint := u.receivedDigest
		if fingerprint == "" {
			raw, _ := json.Marshal(u)
			fingerprint = telegramDigest(string(raw))
		}
		input, exists := a.captured[u.UpdateID]
		if !exists || input.fingerprint != fingerprint {
			e, reason := a.normalizeUpdate(*u)
			if reason == "" && a.router != nil {
				// Polling owns the complete batch and can wait for the snapshot.
				// A transient busy refusal must not poison retained redeliveries.
				kind, target := a.router.CaptureIngress(e.Message)
				e.capture = &ingressCapture{kind: kind, target: target}
				if e.capture.kind == "control" {
					e.InputDigest = ""
				}
			}
			input = telegramCapturedUpdate{fingerprint, e, reason}
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

// poll confirms only the in-memory prefix of the current batch. Empty polls
// never synthesize an offset from a historical maximum update ID.
func (a *TelegramAdapter) poll(ctx context.Context, cursor IngressCursor, batch []telegramUpdate, hasResponse bool) error {
	if hasResponse {
		a.captureBatch(batch)
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !hasResponse {
			var err error
			batch, err = a.pollRequest(ctx, &cursor, false)
			if err != nil {
				return err
			}
		}
		hasResponse = false
		if cursor.Telegram.PendingOffset != nil {
			next := *cloneIngressCursor(&cursor)
			next.Telegram.PendingOffset = nil
			next.Telegram.PendingCreatedAt = time.Time{}
			next.Telegram.State = "confirmed"
			if err := a.commitCursor(ctx, &cursor, next); err != nil {
				return err
			}
		}
		failed := false
		// Process the actual server batch in ID order, without waiting for
		// missing integers or admitting a smaller ID behind a larger ACK.
		slices.SortStableFunc(batch, func(a, b telegramUpdate) int { return cmp.Compare(a.UpdateID, b.UpdateID) })
		for _, update := range batch {
			if update.UpdateID < 0 || update.UpdateID == math.MaxInt64 {
				return errors.New("telegram invalid update identity")
			}
			next := cloneIngressCursor(&cursor)
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
			var result ReceiveResult
			if reason != "" {
				result = a.reject(ctx, envelope, reason)
			} else {
				result = a.receive(ctx, envelope)
			}
			if result.Reason == "update_id_conflict" {
				log.Warnf("telegram: update_id_conflict; conflicting input quarantined")
			}
			// A permanent refusal counts only when a in-memory tombstone was returned.
			if (result.Disposition == ReceiveAccepted || result.Disposition == ReceiveDuplicate || result.Disposition == ReceiveRejected) && result.RecordID != "" {
				cursor = *next
			} else {
				failed = true
				break
			}
		}
		if failed {
			// Unconfirmed acceptance must settle before polling can advance.
			var err error
			cursor, err = a.loadCursor(ctx)
			if err != nil {
				return err
			}
			if !telegramWait(ctx, time.Second) {
				return ctx.Err()
			}
		}
	}
}

func telegramDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
func (a *TelegramAdapter) normalizeUpdate(update telegramUpdate) (IngressEnvelope, string) {
	received := a.now()
	msg := IncomingMessage{IMType: "telegram", AccountID: a.accountID, ChatID: "unsupported-update", MessageID: "update:" + strconv.FormatInt(update.UpdateID, 10)}
	e := IngressEnvelope{Message: msg, DeliveryID: msg.MessageID, ReceivedAt: received}
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
		e.SemanticID, _ = EncodeScopedID("message:", e.Message.ChatID, strconv.FormatInt(m.MessageID, 10))
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
			if suffixed && telegramCommandAlias(name, true) != name {
				return "", "invalid_command_entity"
			}
			return telegramCommandAlias(token, false) + text[end:], ""
		}
		if !strings.EqualFold(token[at+1:], username) || username == "" {
			return "", "other_bot_command"
		}
		return telegramCommandAlias(token[:at], false) + text[end:], ""
	}
	return text, ""
}

// Explicit aliases avoid rewriting arbitrary paths, model commands or args.
func telegramCommandAlias(command string, outbound bool) string {
	for _, canonical := range []string{"/handoff-deny", "/question-interact", "/question-decline", "/question-withdraw", "/question-replace"} {
		alias := strings.ReplaceAll(canonical, "-", "_")
		if outbound && strings.EqualFold(command, canonical) {
			return alias
		}
		if !outbound && strings.EqualFold(command, alias) {
			return canonical
		}
	}
	return command
}

var _ IMAdapter = (*TelegramAdapter)(nil)
