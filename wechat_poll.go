package main

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
)

const (
	wechatMaxBatchBytes    = 4 << 20
	wechatMaxBatchMessages = 1024
)

var (
	errWechatBatchTooLarge          = errors.New("batch_too_large")
	errWechatPollingIdentityChanged = errors.New("wechat polling identity changed")
)

// wechatStuckReportInterval bounds how often a retrying batch item repeats its
// warning. A retry keeps the item's position, so an input that never reaches a
// terminal disposition blocks every later message in the account; it must not
// retry silently.
const wechatStuckReportInterval = time.Minute

// A malformed business ID invalidates that delivery, rather than all the other
// deliveries in its polling batch. An explicit invalid top-level ID remains
// nonempty so inboundMessageID cannot fall back to an unrelated item ID.
func (msg *ilinkMessage) UnmarshalJSON(raw []byte) error {
	type plainMessage ilinkMessage
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
	*msg = ilinkMessage(decoded)
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
	r := a.pairingRouter()
	if r == nil || r.admission == nil || token == nil || a.token.Load() != token {
		return 0, false
	}
	p := r.admission.pairing
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.platforms["wechat"]
	if s == nil || s.account != token.AccountID || s.changed || (s.ctx != nil && s.ctx.Err() != nil) {
		return 0, false
	}
	return s.epoch, true
}

type wechatPollingBatch struct {
	response  *ilinkGetUpdatesResponse
	position  int
	pending   ReceiveResult
	uncertain bool
}

// advance handles one item at a time. A retry keeps the same position; an
// uncertain item is queried instead of being accepted a second time.
func (b *wechatPollingBatch) advance(ctx context.Context, receive, resolve func(context.Context, ilinkMessage, ReceiveResult) ReceiveResult) bool {
	for b.position < len(b.response.Msgs) && ctx.Err() == nil {
		msg := b.response.Msgs[b.position]
		if b.uncertain {
			b.pending = resolve(ctx, msg, b.pending)
		} else {
			b.pending = receive(ctx, msg, b.pending)
		}
		switch b.pending.Disposition {
		case ReceiveAccepted, ReceiveDuplicate, ReceiveTransportIgnored:
			b.position++
			b.uncertain = false
		case ReceiveRejected:
			if b.pending.RecordID == "" {
				return false // A nonpersisted refusal cannot certify batch progress.
			}
			b.position++
			b.uncertain = false
		case ReceiveUncertain:
			b.uncertain = true
			return false
		default:
			b.uncertain = false
			return false
		}
	}
	return b.position == len(b.response.Msgs)
}

func (a *WechatAdapter) monitorLoop(ctx context.Context) error {
	var prepared *TokenData
	var cursor IngressCursor
	consecutiveFailures := 0
	for ctx.Err() == nil {
		token := a.token.Load()
		if token == nil {
			if err := a.login(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				a.notifySessionExpired()
				sleepCtx(ctx, ilinkBackoffDelay)
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
			if errors.Is(err, errWechatBatchTooLarge) {
				return fmt.Errorf("wechat ingress requires local inspection: %w", err)
			}
			log.Warnf("wechat ilink: getupdates request failed")
			sleepCtx(ctx, wechatPollingBackoff(&consecutiveFailures))
			continue
		}
		if resp.statusKind() == ilinkGetUpdatesStatusSessionExpired {
			cancel()
			a.clearTokenSnapshot(token)
			continue
		}
		if resp.Ret != 0 || resp.ErrCode != 0 {
			cancel()
			sleepCtx(ctx, wechatPollingBackoff(&consecutiveFailures))
			continue
		}
		consecutiveFailures = 0
		r := a.pairingRouter()
		if r == nil || r.admission == nil || r.ingressStore == nil {
			cancel()
			return errIngressClosed
		}
		a.capturePollingMessages(resp, token)
		p := r.admission.pairing
		if err := lockIngressMutex(pollCtx, &p.store.txMu); err != nil {
			cancel()
			continue
		}
		epochNow, stillKnown := a.currentPollingEpoch(token)
		if a.token.Load() != token || (knownEpoch && (!stillKnown || epochBefore != epochNow)) {
			p.store.txMu.Unlock()
			cancel()
			continue
		}
		err = p.PlatformReady("wechat", token.AccountID)
		epoch, valid := a.currentPollingEpoch(token)
		p.store.txMu.Unlock()
		if err != nil || !valid {
			cancel()
			log.Warnf("wechat: paired access unavailable; check account identity or terminal pairing setup")
			sleepCtx(ctx, ilinkRetryDelay)
			continue
		}
		batch := wechatPollingBatch{response: resp}
		receive := func(ctx context.Context, msg ilinkMessage, _ ReceiveResult) ReceiveResult {
			if currentEpoch, ok := a.currentPollingEpoch(token); !ok || currentEpoch != epoch {
				return ReceiveResult{Disposition: ReceiveRetryable, Reason: "transport_identity_changed"}
			}
			return a.receiveIncomingMessage(ctx, msg, token)
		}
		resolve := func(ctx context.Context, msg ilinkMessage, result ReceiveResult) ReceiveResult {
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
			if batch.advance(pollCtx, receive, resolve) {
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
				log.Warnf("wechat: input not acknowledged, retrying message_id=%v disposition=%v reason=%v consecutive_failures=%v", batch.response.Msgs[batch.position].MessageID, batch.pending.Disposition, batch.pending.Reason, stuckFailures)
				lastStuckWarn = time.Now()
			}
			r.ObserveIngress("wechat", "acceptance_unconfirmed", batch.pending.Reason)
			sleepCtx(pollCtx, ilinkRetryDelay)
		}
		if pollCtx.Err() != nil || batch.position != len(resp.Msgs) {
			cancel()
			continue
		}
		next, err := nextWechatCursor(cursor, token.AccountID, resp.GetUpdatesBuf)
		if err == nil {
			err = a.commitPollingCursor(pollCtx, token, epoch, cursor, next)
		}
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, errWechatPollingIdentityChanged) || a.token.Load() != token {
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
			sleepCtx(ctx, ilinkPollInterval)
		}
	}
	return nil
}

func wechatPollingBackoff(failures *int) time.Duration {
	*failures = *failures + 1
	if *failures >= ilinkMaxRetries {
		*failures = 0
		return ilinkBackoffDelay
	}
	return ilinkRetryDelay
}

func wechatCursorBuf(cursor IngressCursor) string {
	if cursor.Wechat == nil {
		return ""
	}
	return cursor.Wechat.Buf
}

func nextWechatCursor(current IngressCursor, account, buf string) (IngressCursor, error) {
	if current.Generation == ^uint64(0) {
		return IngressCursor{}, errIngressCursorConflict
	}
	version := uint64(1)
	if current.Wechat != nil {
		if current.Wechat.BatchVersion == ^uint64(0) {
			return IngressCursor{}, errIngressCursorConflict
		}
		version = current.Wechat.BatchVersion + 1
	}
	if buf == "" {
		buf = wechatCursorBuf(current)
	}
	next := IngressCursor{Kind: "wechat", Version: 1, Generation: current.Generation + 1, Wechat: &WechatIngressCursor{AccountID: account, Buf: buf, BatchVersion: version}}
	return next, validateIngressCursor("wechat", &next)
}

func equalWechatCursor(a, b IngressCursor) bool {
	return a.Kind == b.Kind && a.Version == b.Version && a.Generation == b.Generation && a.Telegram == nil && b.Telegram == nil && a.Wechat != nil && b.Wechat != nil && *a.Wechat == *b.Wechat
}

func (a *WechatAdapter) commitPollingCursor(ctx context.Context, token *TokenData, epoch uint64, previous, next IngressCursor) error {
	r := a.pairingRouter()
	p := r.admission.pairing
	if err := lockIngressMutex(ctx, &p.store.txMu); err != nil {
		return err
	}
	valid := func() bool {
		currentEpoch, valid := a.currentPollingEpoch(token)
		return valid && currentEpoch == epoch && a.token.Load() == token
	}
	if !valid() {
		p.store.txMu.Unlock()
		return errWechatPollingIdentityChanged
	}
	err := r.ingressStore.commitCursorGuarded(ctx, "wechat", token.AccountID, previous.Generation, next, valid)
	p.store.txMu.Unlock()
	if errors.Is(err, errIngressTransportChanged) {
		return errWechatPollingIdentityChanged
	}
	if err == nil {
		return nil
	}
	return err
}

func (a *WechatAdapter) getUpdates() (*ilinkGetUpdatesResponse, error) {
	return a.getUpdatesSnapshot(a.adapterContext(), a.token.Load(), a.syncBuf)
}

func (a *WechatAdapter) getUpdatesSnapshot(ctx context.Context, token *TokenData, buf string) (*ilinkGetUpdatesResponse, error) {
	req := ilinkGetUpdatesRequest{GetUpdatesBuf: buf, BaseInfo: ilinkBaseInfo{ChannelVersion: ilinkChannelVersion}}
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

func decodeWechatUpdates(reader io.Reader) (*ilinkGetUpdatesResponse, error) {
	receivedAt := time.Now()
	raw, err := io.ReadAll(io.LimitReader(reader, wechatMaxBatchBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read polling response: %w", err)
	}
	if len(raw) > wechatMaxBatchBytes {
		return nil, errWechatBatchTooLarge
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
	result := &ilinkGetUpdatesResponse{Ret: wire.Ret, ErrCode: wire.ErrCode, GetUpdatesBuf: wire.GetUpdatesBuf, LongPollTimeoutMs: wire.LongPollTimeoutMs}
	if len(wire.Msgs) == 0 || bytes.Equal(wire.Msgs, []byte("null")) {
		return result, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(wire.Msgs))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('[') {
		return nil, errors.New("invalid polling message list")
	}
	for decoder.More() {
		if len(result.Msgs) >= wechatMaxBatchMessages {
			return nil, errWechatBatchTooLarge
		}
		var msg ilinkMessage
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
