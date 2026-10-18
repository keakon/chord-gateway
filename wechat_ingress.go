package main

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// ReceiveTransportIgnored is local to the WeChat transport. It provides no
// in-memory refusal and is never an admission result for a business message.
const ReceiveTransportIgnored ReceiveDisposition = "transport_ignored"

type wechatIngressPending struct {
	future <-chan ReceiveResult
	result ReceiveResult
	// Protected by the adapter mutex. A prepared replacement must not revoke
	// the reply context of work that was already durably accepted.
	prepared     bool
	replyToken   string
	accountEpoch uint64
	bound        bool
	connection   context.Context
}

func wechatIngressBusinessID(sender, id string) string {
	encoded, _ := json.Marshal([]string{sender, id})
	return "message:" + string(encoded)
}

func normalizeWechatIncoming(msg ilinkMessage, token *TokenData) (IncomingMessage, bool) {
	if token == nil || token.AccountID == "" || msg.MessageType != ilinkMessageTypeUser || msg.FromUserID == "" {
		return IncomingMessage{}, false
	}
	id := msg.inboundMessageID()
	if id == "" {
		return IncomingMessage{}, false
	}
	var parts []string
	for _, item := range msg.ItemList {
		if item.Type == ilinkItemTypeText && item.TextItem != nil && item.TextItem.Text != "" {
			parts = append(parts, item.TextItem.Text)
		}
	}
	if len(parts) == 0 {
		return IncomingMessage{}, false
	}
	incoming := IncomingMessage{IMType: "wechat", AccountID: token.AccountID, ChatKind: "private", ChatID: msg.FromUserID, SenderID: msg.FromUserID, MessageID: wechatIngressBusinessID(msg.FromUserID, id), Text: strings.Join(parts, "\n")}
	for _, value := range []string{incoming.AccountID, incoming.ChatID, incoming.MessageID} {
		if len(value) > maxScopedIDBytes || !utf8.ValidString(value) {
			return IncomingMessage{}, false
		}
	}
	return incoming, true
}

func (a *WechatAdapter) receiveIncomingMessage(ctx context.Context, msg ilinkMessage, token *TokenData) ReceiveResult {
	if token == nil || a.token.Load() != token {
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "transport_identity_changed"}
	}
	incoming, supported := normalizeWechatIncoming(msg, token)
	if !supported {
		return ReceiveResult{Disposition: ReceiveTransportIgnored, Reason: "transport_ignored"}
	}
	key := compositeKey(token.AccountID, incoming.MessageID)
	a.mu.Lock()
	connection := a.ctx
	if !a.ingressConnectionCurrentLocked(connection) {
		a.mu.Unlock()
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "transport_stopped"}
	}
	pendingPrior := a.ingressPending[key]
	if pendingPrior != nil {
		previous := pendingPrior.result
		a.mu.Unlock()
		return a.resolveIncomingMessage(ctx, msg, token, previous)
	}
	if len(a.ingressPending) >= 1024 {
		a.mu.Unlock()
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "capacity"}
	}
	a.mu.Unlock()
	r := a.pairingRouter()
	if r == nil || r.admission == nil {
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "ingress_unavailable"}
	}
	pending := &wechatIngressPending{connection: connection}
	preparation := &ingressPreparation{
		valid: func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.ingressConnectionCurrentLocked(connection) && a.token.Load() == token
		},
		prepare: func(auth AdmissionResult) bool {
			if !r.admission.Valid(auth.Ticket) {
				return false
			}
			im := r.getConfig().IMConfigByType("wechat")
			owner := im.OwnerIdentity()
			bound := owner != nil && owner.AccountID == token.AccountID && owner.UserID == incoming.SenderID && im.Bindings()[incoming.ChatID] != ""
			a.mu.Lock()
			defer a.mu.Unlock()
			if !a.ingressConnectionCurrentLocked(connection) || a.token.Load() != token {
				return false
			}
			if msg.ContextToken != "" {
				key := compositeKey(token.AccountID, incoming.ChatID)
				reply := a.replyContexts[key]
				if !bound || !reply.bound || !reply.committed || reply.accountEpoch != auth.Ticket.AccountEpoch {
					a.cachePairedContextLocked(token.AccountID, incoming.ChatID, msg.ContextToken, bound)
					reply = a.replyContexts[key]
					reply.committed = false
					reply.accountEpoch = auth.Ticket.AccountEpoch
					a.replyContexts[key] = reply
				}
				pending.prepared, pending.replyToken = true, reply.token
				pending.accountEpoch, pending.bound = auth.Ticket.AccountEpoch, bound
			}
			return true
		},
	}
	wait, cancel := context.WithTimeout(ctx, ingressAcceptBudget)
	defer cancel()
	future, err := r.SubmitIngress(wait, IngressEnvelope{Message: incoming, capture: msg.ingressCapture, preparation: preparation, ReceivedAt: msg.receivedAt})
	if err != nil {
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "ingress_unavailable"}
	}
	a.mu.Lock()
	pending.future = future
	if a.ingressConnectionCurrentLocked(connection) && a.token.Load() == token {
		if a.ingressPending == nil {
			a.ingressPending = make(map[string]*wechatIngressPending)
		}
		a.ingressPending[key] = pending
	}
	a.mu.Unlock()
	result := WaitIngressNormalization(wait, future)
	a.mu.Lock()
	pending.result = result
	if result.Disposition != ReceiveUncertain && a.ingressPending[key] == pending {
		delete(a.ingressPending, key)
	}
	a.mu.Unlock()
	a.confirmIncomingContext(msg, token, result, pending)
	return committedWechatReceiveResult(result)
}

// An uncommitted refusal must never let the transport advance its checkpoint.
func committedWechatReceiveResult(result ReceiveResult) ReceiveResult {
	if result.Disposition == ReceiveRejected && result.RecordID == "" {
		return ReceiveResult{Disposition: ReceiveRetryable, Reason: "rejection_not_committed"}
	}
	return result
}

func (a *WechatAdapter) resolveIncomingMessage(ctx context.Context, msg ilinkMessage, token *TokenData, previous ReceiveResult) ReceiveResult {
	incoming, supported := normalizeWechatIncoming(msg, token)
	if !supported {
		return previous
	}
	key := compositeKey(token.AccountID, incoming.MessageID)
	a.mu.Lock()
	pending := a.ingressPending[key]
	if pending != nil {
		select {
		case result, ok := <-pending.future:
			if ok {
				pending.result = result
			}
		default:
		}
		previous = pending.result
	}
	a.mu.Unlock()
	if previous.Disposition != ReceiveUncertain {
		a.mu.Lock()
		if a.ingressPending[key] == pending {
			delete(a.ingressPending, key)
		}
		a.mu.Unlock()
		a.confirmIncomingContext(msg, token, previous, pending)
	}
	return committedWechatReceiveResult(previous)
}

func (a *WechatAdapter) confirmIncomingContext(msg ilinkMessage, token *TokenData, result ReceiveResult, pending *wechatIngressPending) {
	if token == nil || pending == nil || msg.ContextToken == "" || (result.Disposition != ReceiveAccepted && result.Disposition != ReceiveDuplicate) || result.State == ingressRejected || result.State == ingressExpired {
		return
	}
	r := a.pairingRouter()
	if r == nil || r.admission == nil {
		return
	}
	p := r.admission.pairing
	p.mu.Lock()
	state := p.platforms["wechat"]
	if state == nil || state.changed || state.account != token.AccountID {
		p.mu.Unlock()
		return
	}
	epoch := state.epoch
	p.mu.Unlock()
	a.mu.Lock()
	key := compositeKey(token.AccountID, msg.FromUserID)
	reply, exists := a.replyContexts[key]
	if !a.ingressConnectionCurrentLocked(pending.connection) || a.token.Load() != token || !pending.prepared || pending.accountEpoch != epoch || !exists || reply.token != pending.replyToken || reply.accountEpoch != epoch {
		a.mu.Unlock()
		return
	}
	a.cachePairedContextLocked(token.AccountID, msg.FromUserID, msg.ContextToken, pending.bound)
	reply = a.replyContexts[key]
	reply.accountEpoch = epoch
	a.replyContexts[key] = reply
	a.mu.Unlock()
	a.wakeIngressReadiness()
}

// Each Connect creates a new context even when it reuses the same credentials.
// A callback from a completed connection must never regain its validity.
func (a *WechatAdapter) ingressConnectionCurrentLocked(connection context.Context) bool {
	return !a.stopped && a.ctx == connection && (connection == nil || connection.Err() == nil)
}

func (a *WechatAdapter) IngressDeliveryReadiness(id ChatIdentity) IngressDeliveryReadiness {
	token := a.token.Load()
	if id.Platform != "wechat" || token == nil || token.AccountID != id.AccountID {
		return IngressDeliveryReadiness{Reason: IngressWaitingAccount}
	}
	a.mu.Lock()
	stopped := !a.ingressConnectionCurrentLocked(a.ctx)
	reply, exists := a.replyContexts[compositeKey(id.AccountID, id.ChatID)]
	a.mu.Unlock()
	if stopped {
		return IngressDeliveryReadiness{Reason: IngressWaitingAccount}
	}
	if !exists || reply.token == "" || !reply.bound || !reply.committed {
		return IngressDeliveryReadiness{Reason: IngressWaitingReplyContext}
	}
	if r := a.pairingRouter(); r != nil && r.admission != nil {
		p := r.admission.pairing
		p.mu.Lock()
		state := p.platforms["wechat"]
		valid := state != nil && !state.changed && state.account == id.AccountID && state.epoch == reply.accountEpoch
		p.mu.Unlock()
		if !valid {
			return IngressDeliveryReadiness{Reason: IngressWaitingReplyContext}
		}
	}
	return IngressDeliveryReadiness{Ready: true}
}

var _ IngressDeliveryReadinessProvider = (*WechatAdapter)(nil)
var _ IngressDeliveryReadinessNotifier = (*WechatAdapter)(nil)
