package wechat

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/keakon/chord-gateway/internal/im"
)

// WechatIngressPending tracks one submitted input whose acceptance has not
// reached a terminal disposition yet. The gateway joint tests construct and
// inspect entries through the ForTest accessors.
type WechatIngressPending struct {
	Future <-chan im.ReceiveResult
	Result im.ReceiveResult
	// Protected by the adapter mutex. A prepared replacement must not revoke
	// the reply context of work that was already durably accepted.
	Prepared     bool
	ReplyToken   string
	AccountEpoch uint64
	Bound        bool
	Connection   context.Context
}

func WechatIngressBusinessID(sender, id string) string {
	encoded, _ := json.Marshal([]string{sender, id})
	return "message:" + string(encoded)
}

func normalizeWechatIncoming(msg ILinkMessage, token *TokenData) (im.IncomingMessage, bool) {
	if token == nil || token.AccountID == "" || msg.MessageType != ILinkMessageTypeUser || msg.FromUserID == "" {
		return im.IncomingMessage{}, false
	}
	id := msg.inboundMessageID()
	if id == "" {
		return im.IncomingMessage{}, false
	}
	var parts []string
	for _, item := range msg.ItemList {
		if item.Type == ILinkItemTypeText && item.TextItem != nil && item.TextItem.Text != "" {
			parts = append(parts, item.TextItem.Text)
		}
	}
	if len(parts) == 0 {
		return im.IncomingMessage{}, false
	}
	incoming := im.IncomingMessage{IMType: "wechat", AccountID: token.AccountID, ChatKind: "private", ChatID: msg.FromUserID, SenderID: msg.FromUserID, MessageID: WechatIngressBusinessID(msg.FromUserID, id), Text: strings.Join(parts, "\n")}
	for _, value := range []string{incoming.AccountID, incoming.ChatID, incoming.MessageID} {
		if len(value) > im.MaxScopedIDBytes || !utf8.ValidString(value) {
			return im.IncomingMessage{}, false
		}
	}
	return incoming, true
}

func (a *WechatAdapter) receiveIncomingMessage(ctx context.Context, msg ILinkMessage, token *TokenData) im.ReceiveResult {
	if token == nil || a.token.Load() != token {
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "transport_identity_changed"}
	}
	incoming, supported := normalizeWechatIncoming(msg, token)
	if !supported {
		return im.ReceiveResult{Disposition: im.ReceiveTransportIgnored, Reason: "transport_ignored"}
	}
	key := compositeKey(token.AccountID, incoming.MessageID)
	a.mu.Lock()
	connection := a.ctx
	if !a.ingressConnectionCurrentLocked(connection) {
		a.mu.Unlock()
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "transport_stopped"}
	}
	pendingPrior := a.ingressPending[key]
	if pendingPrior != nil {
		previous := pendingPrior.Result
		a.mu.Unlock()
		return a.resolveIncomingMessage(ctx, msg, token, previous)
	}
	if len(a.ingressPending) >= 1024 {
		a.mu.Unlock()
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "capacity"}
	}
	a.mu.Unlock()
	if a.host == nil {
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "ingress_unavailable"}
	}
	pending := &WechatIngressPending{Connection: connection}
	preparation := &im.IngressPreparation{
		Valid: func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.ingressConnectionCurrentLocked(connection) && a.token.Load() == token
		},
		Prepare: func(auth im.AdmissionResult) bool {
			if !a.host.ValidTicket(auth.Ticket) {
				return false
			}
			imCfg := a.host.CurrentConfig().IMConfigByType("wechat")
			owner := imCfg.OwnerIdentity()
			bound := owner != nil && owner.AccountID == token.AccountID && owner.UserID == incoming.SenderID && imCfg.Bindings()[incoming.ChatID] != ""
			a.mu.Lock()
			defer a.mu.Unlock()
			if !a.ingressConnectionCurrentLocked(connection) || a.token.Load() != token {
				return false
			}
			if msg.ContextToken != "" {
				key := compositeKey(token.AccountID, incoming.ChatID)
				reply := a.replyContexts[key]
				if !bound || !reply.Bound || !reply.Committed || reply.AccountEpoch != auth.Ticket.AccountEpoch {
					a.cachePairedContextLocked(token.AccountID, incoming.ChatID, msg.ContextToken, bound)
					reply = a.replyContexts[key]
					reply.Committed = false
					reply.AccountEpoch = auth.Ticket.AccountEpoch
					a.replyContexts[key] = reply
				}
				pending.Prepared, pending.ReplyToken = true, reply.Token
				pending.AccountEpoch, pending.Bound = auth.Ticket.AccountEpoch, bound
			}
			return true
		},
	}
	wait, cancel := context.WithTimeout(ctx, im.IngressAcceptBudget)
	defer cancel()
	future, err := a.host.SubmitIngress(wait, im.IngressEnvelope{Message: incoming, Capture: msg.captureRef, Preparation: preparation, ReceivedAt: msg.receivedAt})
	if err != nil {
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "ingress_unavailable"}
	}
	a.mu.Lock()
	pending.Future = future
	if a.ingressConnectionCurrentLocked(connection) && a.token.Load() == token {
		if a.ingressPending == nil {
			a.ingressPending = make(map[string]*WechatIngressPending)
		}
		a.ingressPending[key] = pending
	}
	a.mu.Unlock()
	result := im.WaitIngressNormalization(wait, future)
	a.mu.Lock()
	pending.Result = result
	if result.Disposition != im.ReceiveUncertain && a.ingressPending[key] == pending {
		delete(a.ingressPending, key)
	}
	a.mu.Unlock()
	a.confirmIncomingContext(msg, token, result, pending)
	return committedWechatReceiveResult(result)
}

// An uncommitted refusal must never let the transport advance its checkpoint.
func committedWechatReceiveResult(result im.ReceiveResult) im.ReceiveResult {
	if result.Disposition == im.ReceiveRejected && result.RecordID == "" {
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "rejection_not_committed"}
	}
	return result
}

func (a *WechatAdapter) resolveIncomingMessage(ctx context.Context, msg ILinkMessage, token *TokenData, previous im.ReceiveResult) im.ReceiveResult {
	incoming, supported := normalizeWechatIncoming(msg, token)
	if !supported {
		return previous
	}
	key := compositeKey(token.AccountID, incoming.MessageID)
	a.mu.Lock()
	pending := a.ingressPending[key]
	if pending != nil {
		select {
		case result, ok := <-pending.Future:
			if ok {
				pending.Result = result
			}
		default:
		}
		previous = pending.Result
	}
	a.mu.Unlock()
	if previous.Disposition != im.ReceiveUncertain {
		a.mu.Lock()
		if a.ingressPending[key] == pending {
			delete(a.ingressPending, key)
		}
		a.mu.Unlock()
		a.confirmIncomingContext(msg, token, previous, pending)
	}
	return committedWechatReceiveResult(previous)
}

func (a *WechatAdapter) confirmIncomingContext(msg ILinkMessage, token *TokenData, result im.ReceiveResult, pending *WechatIngressPending) {
	if token == nil || pending == nil || msg.ContextToken == "" || (result.Disposition != im.ReceiveAccepted && result.Disposition != im.ReceiveDuplicate) || result.State == im.IngressRejected || result.State == im.IngressExpired {
		return
	}
	if a.host == nil {
		return
	}
	epoch, ok := a.host.PairingEpoch("wechat", token.AccountID)
	if !ok {
		return
	}
	a.mu.Lock()
	key := compositeKey(token.AccountID, msg.FromUserID)
	reply, exists := a.replyContexts[key]
	if !a.ingressConnectionCurrentLocked(pending.Connection) || a.token.Load() != token || !pending.Prepared || pending.AccountEpoch != epoch || !exists || reply.Token != pending.ReplyToken || reply.AccountEpoch != epoch {
		a.mu.Unlock()
		return
	}
	a.cachePairedContextLocked(token.AccountID, msg.FromUserID, msg.ContextToken, pending.Bound)
	reply = a.replyContexts[key]
	reply.AccountEpoch = epoch
	a.replyContexts[key] = reply
	a.mu.Unlock()
	a.WakeIngressReadiness()
}

// Each Connect creates a new context even when it reuses the same credentials.
// A callback from a completed connection must never regain its validity.
func (a *WechatAdapter) ingressConnectionCurrentLocked(connection context.Context) bool {
	return !a.stopped && a.ctx == connection && (connection == nil || connection.Err() == nil)
}

func (a *WechatAdapter) IngressDeliveryReadiness(id im.ChatIdentity) im.IngressDeliveryReadiness {
	token := a.token.Load()
	if id.Platform != "wechat" || token == nil || token.AccountID != id.AccountID {
		return im.IngressDeliveryReadiness{Reason: im.IngressWaitingAccount}
	}
	a.mu.Lock()
	stopped := !a.ingressConnectionCurrentLocked(a.ctx)
	reply, exists := a.replyContexts[compositeKey(id.AccountID, id.ChatID)]
	a.mu.Unlock()
	if stopped {
		return im.IngressDeliveryReadiness{Reason: im.IngressWaitingAccount}
	}
	if !exists || reply.Token == "" || !reply.Bound || !reply.Committed {
		return im.IngressDeliveryReadiness{Reason: im.IngressWaitingReplyContext}
	}
	if a.host != nil {
		epoch, ok := a.host.PairingEpoch("wechat", id.AccountID)
		if !ok || epoch != reply.AccountEpoch {
			return im.IngressDeliveryReadiness{Reason: im.IngressWaitingReplyContext}
		}
	}
	return im.IngressDeliveryReadiness{Ready: true}
}

var _ im.IngressDeliveryReadinessProvider = (*WechatAdapter)(nil)
var _ im.IngressDeliveryReadinessNotifier = (*WechatAdapter)(nil)
