package wechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/internal/im"
)

const (
	WechatMaxBatchBytes    = 4 << 20
	WechatMaxBatchMessages = 1024
)

var (
	ErrWechatBatchTooLarge          = errors.New("batch_too_large")
	ErrWechatPollingIdentityChanged = errors.New("wechat polling identity changed")
)

// wechatStuckReportInterval bounds how often a retrying batch item repeats its
// warning. A retry keeps the item's position, so an input that never reaches a
// terminal disposition blocks every later message in the account; it must not
// retry silently.
const wechatStuckReportInterval = time.Minute

// UnmarshalJSON rejects a malformed business ID for that delivery rather than
// all the other deliveries in its polling batch. An explicit invalid top-level
// ID remains nonempty so inboundMessageID cannot fall back to an unrelated item
// ID.
func (msg *ILinkMessage) UnmarshalJSON(raw []byte) error {
	type plainMessage ILinkMessage
	var decoded plainMessage
	wire := struct {
		*plainMessage
		MessageID json.RawMessage `json:"message_id"`
	}{plainMessage: &decoded}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	if len(wire.MessageID) != 0 {
		var id json.Number
		if err := json.Unmarshal(wire.MessageID, &id); err != nil || id == "" {
			decoded.MessageID = "invalid"
		} else {
			decoded.MessageID = id
		}
	}
	*msg = ILinkMessage(decoded)
	return nil
}

// swapPollingToken closes the old credential scope before exposing its replacement.
// Registration and replacement share mu so a newly registered old request cannot
// escape cancellation between a separate cancel and token.Store.
func (a *WechatAdapter) swapPollingToken(token *TokenData) {
	a.mu.Lock()
	if a.pollCancel != nil {
		a.pollCancel()
		a.pollCancel = nil
		a.pollToken = nil
	}
	a.token.Store(token)
	a.mu.Unlock()
}

func (a *WechatAdapter) pollingContext(ctx context.Context, token *TokenData) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if a.token.Load() != token {
		cancel()
	} else {
		if a.pollCancel != nil {
			a.pollCancel()
		}
		a.pollToken, a.pollCancel = token, cancel
	}
	a.mu.Unlock()
	return ctx, cancel
}

func (a *WechatAdapter) currentPollingEpoch(token *TokenData) (uint64, bool) {
	if a.host == nil || token == nil || a.token.Load() != token {
		return 0, false
	}
	return a.host.PairingEpoch("wechat", token.AccountID)
}

type WechatPollingBatch struct {
	Response  *ILinkGetUpdatesResponse
	Position  int
	Pending   im.ReceiveResult
	Uncertain bool
}

// Advance handles one item at a time. A retry keeps the same position; an
// uncertain item is queried instead of being accepted a second time.
func (b *WechatPollingBatch) Advance(ctx context.Context, receive, resolve func(context.Context, ILinkMessage, im.ReceiveResult) im.ReceiveResult) bool {
	for b.Position < len(b.Response.Msgs) && ctx.Err() == nil {
		msg := b.Response.Msgs[b.Position]
		if b.Uncertain {
			b.Pending = resolve(ctx, msg, b.Pending)
		} else {
			b.Pending = receive(ctx, msg, b.Pending)
		}
		switch b.Pending.Disposition {
		case im.ReceiveAccepted, im.ReceiveDuplicate, im.ReceiveTransportIgnored:
			b.Position++
			b.Uncertain = false
		case im.ReceiveRejected:
			if b.Pending.RecordID == "" {
				return false // A nonpersisted refusal cannot certify batch progress.
			}
			b.Position++
			b.Uncertain = false
		case im.ReceiveUncertain:
			b.Uncertain = true
			return false
		default:
			b.Uncertain = false
			return false
		}
	}
	return b.Position == len(b.Response.Msgs)
}

func (a *WechatAdapter) monitorLoop(ctx context.Context) error {
	var prepared *TokenData
	var cursor im.IngressCursor
	consecutiveFailures := 0
	for ctx.Err() == nil {
		token := a.token.Load()
		if token == nil {
			if err := a.login(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				a.notifySessionExpired()
				im.SleepCtx(ctx, ILinkBackoffDelay)
			}
			continue
		}
		pollCtx, cancel := a.pollingContext(ctx, token)
		if prepared != token {
			var err error
			cursor, err = a.prepareIngressAccount(pollCtx, token)
			if err != nil {
				cancel()
				if ctx.Err() != nil {
					return nil
				}
				if a.token.Load() != token {
					continue
				}
				return fmt.Errorf("prepare Wechat ingress: %w", err)
			}
			prepared = token
			a.syncAccount, a.syncBuf = token.AccountID, wechatCursorBuf(cursor)
		}
		epochBefore, knownEpoch := a.currentPollingEpoch(token)
		resp, err := a.getUpdatesSnapshot(pollCtx, token, wechatCursorBuf(cursor))
		if a.token.Load() != token {
			cancel()
			continue
		}
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrWechatBatchTooLarge) {
				return fmt.Errorf("wechat ingress requires local inspection: %w", err)
			}
			log.Warnf("wechat ilink: getupdates request failed")
			im.SleepCtx(ctx, wechatPollingBackoff(&consecutiveFailures))
			continue
		}
		if resp.statusKind() == ilinkGetUpdatesStatusSessionExpired {
			cancel()
			a.clearTokenSnapshot(token)
			continue
		}
		if resp.Ret != 0 || resp.ErrCode != 0 {
			cancel()
			im.SleepCtx(ctx, wechatPollingBackoff(&consecutiveFailures))
			continue
		}
		consecutiveFailures = 0
		if a.host == nil {
			cancel()
			return im.ErrIngressClosed
		}
		a.capturePollingMessages(resp, token)
		if a.token.Load() != token {
			cancel()
			continue
		}
		epochNow, stillKnown := a.currentPollingEpoch(token)
		if knownEpoch && (!stillKnown || epochBefore != epochNow) {
			cancel()
			continue
		}
		// Publish readiness under the config-store transaction lock; the
		// checkpoint write below reacquires the same lock so a pairing change
		// cannot interleave between its epoch check and cursor commit.
		if err := a.host.PublishPlatformReadinessUnderConfigTx(pollCtx, "wechat", token.AccountID); err != nil {
			cancel()
			log.Warnf("wechat: paired access unavailable; check account identity or terminal pairing setup")
			im.SleepCtx(ctx, ILinkRetryDelay)
			continue
		}
		epoch, valid := a.currentPollingEpoch(token)
		if !valid {
			cancel()
			log.Warnf("wechat: paired access unavailable; check account identity or terminal pairing setup")
			im.SleepCtx(ctx, ILinkRetryDelay)
			continue
		}
		batch := WechatPollingBatch{Response: resp}
		receive := func(ctx context.Context, msg ILinkMessage, _ im.ReceiveResult) im.ReceiveResult {
			if currentEpoch, ok := a.currentPollingEpoch(token); !ok || currentEpoch != epoch {
				return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "transport_identity_changed"}
			}
			return a.receiveIncomingMessage(ctx, msg, token)
		}
		resolve := func(ctx context.Context, msg ILinkMessage, result im.ReceiveResult) im.ReceiveResult {
			if currentEpoch, ok := a.currentPollingEpoch(token); !ok || currentEpoch != epoch {
				return result
			}
			return a.resolveIncomingMessage(ctx, msg, token, result)
		}
		var lastStuckWarn time.Time
		stuckFailures := 0
		for pollCtx.Err() == nil {
			currentEpoch, ok := a.currentPollingEpoch(token)
			if !ok || currentEpoch != epoch {
				break
			}
			if batch.Advance(pollCtx, receive, resolve) {
				break
			}
			if pollCtx.Err() != nil {
				// A shutdown or credential change interrupted the batch between
				// items; that is not a stuck acceptance and must not be reported
				// as one while the loop is already stopping.
				break
			}
			// Retrying in order is correct: skipping ahead would silently drop
			// an input whose outcome is not terminal. Escalate loudly instead
			// of blocking the whole account behind one item without evidence.
			stuckFailures++
			if stuckFailures == 1 || time.Since(lastStuckWarn) >= wechatStuckReportInterval {
				log.Warnf("wechat: input not acknowledged, retrying message_id=%v disposition=%v reason=%v consecutive_failures=%v", batch.Response.Msgs[batch.Position].MessageID, batch.Pending.Disposition, batch.Pending.Reason, stuckFailures)
				lastStuckWarn = time.Now()
			}
			a.host.ObserveIngress("wechat", "acceptance_unconfirmed", batch.Pending.Reason)
			im.SleepCtx(pollCtx, ILinkRetryDelay)
		}
		if pollCtx.Err() != nil || batch.Position != len(resp.Msgs) {
			cancel()
			continue
		}
		next, err := NextWechatCursor(cursor, token.AccountID, resp.GetUpdatesBuf)
		if err == nil {
			err = a.commitPollingCursor(pollCtx, token, epoch, cursor, next)
		}
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrWechatPollingIdentityChanged) || a.token.Load() != token {
				continue
			}
			return fmt.Errorf("commit Wechat polling checkpoint: %w", err)
		}
		if currentEpoch, valid := a.currentPollingEpoch(token); !valid || currentEpoch != epoch {
			continue
		}
		previousBuf := wechatCursorBuf(cursor)
		cursor = next
		a.syncBuf = wechatCursorBuf(cursor)
		if len(resp.Msgs) == 0 || previousBuf == wechatCursorBuf(next) {
			im.SleepCtx(ctx, ILinkPollInterval)
		}
	}
	return nil
}

func wechatPollingBackoff(failures *int) time.Duration {
	*failures = *failures + 1
	if *failures >= ILinkMaxRetries {
		*failures = 0
		return ILinkBackoffDelay
	}
	return ILinkRetryDelay
}

func wechatCursorBuf(cursor im.IngressCursor) string {
	if cursor.Wechat == nil {
		return ""
	}
	return cursor.Wechat.Buf
}

func NextWechatCursor(current im.IngressCursor, account, buf string) (im.IngressCursor, error) {
	if current.Generation == ^uint64(0) {
		return im.IngressCursor{}, im.ErrIngressCursorConflict
	}
	version := uint64(1)
	if current.Wechat != nil {
		if current.Wechat.BatchVersion == ^uint64(0) {
			return im.IngressCursor{}, im.ErrIngressCursorConflict
		}
		version = current.Wechat.BatchVersion + 1
	}
	if buf == "" {
		buf = wechatCursorBuf(current)
	}
	next := im.IngressCursor{Kind: "wechat", Version: 1, Generation: current.Generation + 1, Wechat: &im.WechatIngressCursor{AccountID: account, Buf: buf, BatchVersion: version}}
	return next, im.ValidateIngressCursor("wechat", &next)
}

func EqualWechatCursor(a, b im.IngressCursor) bool {
	return a.Kind == b.Kind && a.Version == b.Version && a.Generation == b.Generation && a.Telegram == nil && b.Telegram == nil && a.Wechat != nil && b.Wechat != nil && *a.Wechat == *b.Wechat
}

func (a *WechatAdapter) commitPollingCursor(ctx context.Context, token *TokenData, epoch uint64, previous, next im.IngressCursor) error {
	valid := func() bool {
		currentEpoch, valid := a.currentPollingEpoch(token)
		return valid && currentEpoch == epoch && a.token.Load() == token
	}
	if !valid() {
		return ErrWechatPollingIdentityChanged
	}
	err := a.host.CommitIngressCursorUnderConfigTx(ctx, "wechat", token.AccountID, previous.Generation, next, valid)
	if errors.Is(err, im.ErrIngressTransportChanged) {
		return ErrWechatPollingIdentityChanged
	}
	return err
}

func (a *WechatAdapter) getUpdates() (*ILinkGetUpdatesResponse, error) {
	return a.getUpdatesSnapshot(a.adapterContext(), a.token.Load(), a.syncBuf)
}

func (a *WechatAdapter) getUpdatesSnapshot(ctx context.Context, token *TokenData, buf string) (*ILinkGetUpdatesResponse, error) {
	req := ILinkGetUpdatesRequest{GetUpdatesBuf: buf, BaseInfo: ILinkBaseInfo{ChannelVersion: ILinkChannelVersion}}
	resp, err := a.apiPostSnapshot(ctx, token, "ilink/bot/getupdates", req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return decodeWechatUpdates(resp.Body)
}

func decodeWechatUpdates(reader io.Reader) (*ILinkGetUpdatesResponse, error) {
	receivedAt := time.Now()
	raw, err := io.ReadAll(io.LimitReader(reader, WechatMaxBatchBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read polling response: %w", err)
	}
	if len(raw) > WechatMaxBatchBytes {
		return nil, ErrWechatBatchTooLarge
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("invalid polling response object")
	}
	var wire struct {
		Ret               int             `json:"ret"`
		ErrCode           int             `json:"errcode"`
		GetUpdatesBuf     string          `json:"get_updates_buf"`
		LongPollTimeoutMs int             `json:"longpolling_timeout_ms"`
		Msgs              json.RawMessage `json:"msgs"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("decode polling response: %w", err)
	}
	result := &ILinkGetUpdatesResponse{Ret: wire.Ret, ErrCode: wire.ErrCode, GetUpdatesBuf: wire.GetUpdatesBuf, LongPollTimeoutMs: wire.LongPollTimeoutMs}
	if len(wire.Msgs) == 0 || bytes.Equal(wire.Msgs, []byte("null")) {
		return result, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(wire.Msgs))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('[') {
		return nil, errors.New("invalid polling message list")
	}
	for decoder.More() {
		if len(result.Msgs) >= WechatMaxBatchMessages {
			return nil, ErrWechatBatchTooLarge
		}
		var msg ILinkMessage
		if err := decoder.Decode(&msg); err != nil {
			return nil, fmt.Errorf("decode polling message: %w", err)
		}
		msg.receivedAt = receivedAt
		result.Msgs = append(result.Msgs, msg)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("close polling message list: %w", err)
	}
	return result, nil
}
