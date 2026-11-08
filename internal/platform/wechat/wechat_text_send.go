package wechat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// wechatSendSnapshot is one SendTextContext call's frozen send identity: the
// credential pointer, target and reply context captured before the shared
// sender queues the output. A queued or mid-output send keeps this snapshot
// even when the adapter later rotates its token or caches a fresher reply
// context, so old output never continues under new credentials or a renewed
// pairing permission.
type wechatSendSnapshot struct {
	token *TokenData
	chat  string
	reply WechatReplyContext
}

// wechatSendSnapshotKey carries the snapshot to the shared sender's
// SendSegment, whose signature has no room for adapter state. It is
// package-private: values are set only by SendTextContext.
type wechatSendSnapshotKey struct{}

// wechatSendRefusal marks a definite local non-delivery so partial-send
// diagnostics keep a bounded, actionable category instead of delivery_failed.
type wechatSendRefusal struct {
	category string
	cause    error
}

func (e *wechatSendRefusal) Error() string                     { return e.cause.Error() }
func (e *wechatSendRefusal) Unwrap() error                     { return e.cause }
func (e *wechatSendRefusal) IMTextSendFailureCategory() string { return e.category }

// wechatTextSender lazily builds the adapter's single shared text sender. One
// instance per adapter keeps target ordering slots and cumulative metrics
// intact across calls; creating one per call would split both.
func (a *WechatAdapter) wechatTextSender() (*im.IMTextSender, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.textSender == nil {
		sender, err := im.NewIMTextSender(im.IMTextSendOptions{
			Platform: "wechat",
			Budget:   ILinkMaxMessageLen,
			SendSegment: func(ctx context.Context, _ string, text string) error {
				return a.sendTextSegment(ctx, text)
			},
		})
		if err != nil {
			return nil, err
		}
		a.textSender = sender
	}
	return a.textSender, nil
}

// sendTextSegment sends one rendered segment under the call's frozen snapshot.
// The shared sender rechecks cancellation and slot state before every call;
// this guard adds the adapter-local evidence: supervisor stop, credential
// rotation and unbound reply-context expiry. All are definite refusals —
// nothing was sent for this segment.
func (a *WechatAdapter) sendTextSegment(ctx context.Context, text string) error {
	snapshot, ok := ctx.Value(wechatSendSnapshotKey{}).(*wechatSendSnapshot)
	if !ok || snapshot == nil {
		return &wechatSendRefusal{"local_error", errors.New("wechat send snapshot unavailable")}
	}
	a.mu.Lock()
	stopped := a.stopped
	a.mu.Unlock()
	if stopped {
		return &wechatSendRefusal{"connection_unavailable", errors.New("wechat adapter stopped")}
	}
	if a.token.Load() != snapshot.token {
		return &wechatSendRefusal{"identity_mismatch", errors.New("account changed")}
	}
	if !snapshot.reply.Bound && !time.Now().Before(snapshot.reply.Expires) {
		// local_error keeps this inside the shared sender's bounded category
		// vocabulary; the cause carries the specific expiry evidence.
		return &wechatSendRefusal{"local_error", errors.New("reply context expired while queued")}
	}
	body := ILinkSendMessageRequest{Msg: ILinkMessage{
		ToUserID:     snapshot.chat,
		ClientID:     fmt.Sprintf("chord-gateway-%d", time.Now().UnixNano()),
		MessageType:  ILinkMessageTypeBot,
		MessageState: ILinkMessageStateFinish,
		ContextToken: snapshot.reply.Token,
		ItemList:     []ILinkItem{{Type: ILinkItemTypeText, TextItem: &ILinkTextItem{Text: text}}},
	}}
	return a.sendMessage(ctx, snapshot.token, body)
}

// TextSendMetrics forwards shared sender diagnostics for multi-adapter metrics.
func (a *WechatAdapter) TextSendMetrics() im.IMTextMetricsSnapshot {
	a.mu.Lock()
	sender := a.textSender
	a.mu.Unlock()
	if sender == nil {
		return im.IMTextMetricsSnapshot{Platform: "wechat"}
	}
	return sender.TextSendMetrics()
}

// TextSendTargetSnapshot reports the bounded per-target partial-failure
// snapshot the shared sender keeps for /status.
func (a *WechatAdapter) TextSendTargetSnapshot(target string) im.IMTextTargetSnapshot {
	a.mu.Lock()
	sender := a.textSender
	a.mu.Unlock()
	if sender == nil {
		return im.IMTextTargetSnapshot{}
	}
	return sender.TextSendTargetSnapshot(target)
}

var _ im.IMTextMetricsProvider = (*WechatAdapter)(nil)
