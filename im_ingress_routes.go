package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

func (r *NotificationRouter) ingressRouteIdentity(id ChatIdentity) (digest, workspace, session string, err error) {
	cfg := r.getConfig()
	if cfg == nil {
		return "", "", "", errPairingDenied
	}
	im := cfg.IMConfigByType(id.Platform)
	if im == nil || im.OwnerIdentity() == nil || im.OwnerIdentity().AccountID != id.AccountID {
		return "", "", "", errPairingDenied
	}
	ws, e := cfg.ResolveWorkspace(id.Platform, id.ChatID)
	if e != nil || ws == nil {
		return "", "", "", errPairingDenied
	}
	path, _, e := canonicalBindingDirectory(ws.Path)
	if e != nil {
		return "", "", "", e
	}
	data, _ := json.Marshal([]string{id.Platform, id.AccountID, im.OwnerIdentity().UserID, id.ChatID, ws.ID, path})
	sum := sha256.Sum256(data)
	key := (processKey{workspaceID: ws.ID, imType: id.Platform, chatID: id.ChatID}).String()
	if r.mgr.pins != nil {
		session = r.mgr.pins.Get(pinKeyForBinding(key))
	}
	return hex.EncodeToString(sum[:]), ws.ID, session, nil
}

// beginIngressRouteChange runs under the same gate as config mutation and stdin.
// Failed changes retain the new revision; a changing route refuses new tasks
// until it settles.
func (r *NotificationRouter) beginIngressRouteChange(ctx context.Context, id ChatIdentity) (func() error, error) {
	if r.ingressStore == nil {
		return func() error { return nil }, nil
	}
	route, err := r.ingressStore.GetRoute(id)
	if errors.Is(err, errIngressMissing) {
		return func() error { return nil }, nil
	}
	if err != nil {
		return nil, err
	}
	if route.Revision == "" {
		return func() error { return nil }, nil
	}
	route, err = r.ingressStore.BeginRouteChange(ctx, id)
	if err != nil {
		return nil, err
	}
	return func() error {
		digest, workspace, session, err := r.ingressRouteIdentity(id)
		if err != nil {
			im := r.getConfig().IMConfigByType(id.Platform)
			if im == nil || im.Bindings()[id.ChatID] != "" || im.OwnerIdentity() != nil && im.OwnerIdentity().AccountID != id.AccountID {
				return err
			}
			// A failed first bind still has an authoritative unbound target.
			// Settle that target with the new revision so pairing can be retried.
		}
		return r.ingressStore.FinishRouteChange(ctx, id, route.Revision, digest, workspace, session)
	}, nil
}

func (r *NotificationRouter) enqueueIngressReply(msg IncomingMessage, text string) outboundEnqueueResult {
	recordDrop := func(reason string) {
		if r.ingressStore != nil {
			r.ingressStore.metrics.record(msg.IMType, "reply_dropped", reason)
		}
	}
	if r.outbound == nil {
		recordDrop("outbound_closed")
		return outboundClosed
	}
	if msg.ticket == nil || r.admission == nil || !r.admission.Valid(*msg.ticket) {
		recordDrop("authorization_changed")
		return outboundClosed
	}
	// The producer may reuse its message/ticket after enqueue returns.
	// Every deferred permission check uses this operation's frozen ticket.
	ticketSnapshot := *msg.ticket
	msg.ticket = &ticketSnapshot
	deadline := time.Now().Add(IMControlSendTimeout)
	var send func(context.Context) error
	if msg.ticket.Kind == "owner_unbound" || msg.ticket.Kind == "paired_bind" {
		ticket := *msg.ticket
		expires := ticket.ReceivedAt.Add(30 * time.Second)
		if expires.Before(deadline) {
			deadline = expires
		}
		permit := ReplyPermit{Identity: ticket.Identity, SenderID: ticket.SenderID, AccountEpoch: ticket.AccountEpoch, RequestID: msg.MessageID, Kind: "owner", ExpiresAt: deadline, ticket: ticket}
		send = func(ctx context.Context) error { return r.SendFlowReply(ctx, permit, text) }
	} else {
		ws, err := r.getConfig().ResolveWorkspace(msg.IMType, msg.ChatID)
		if err != nil || ws == nil {
			recordDrop("authorization_changed")
			return outboundClosed
		}
		target, ok := r.targetForKey((processKey{workspaceID: ws.ID, imType: msg.IMType, chatID: msg.ChatID}).String())
		if !ok || target.Identity.AccountID != msg.AccountID {
			recordDrop("authorization_changed")
			return outboundClosed
		}
		send = func(ctx context.Context) error { return r.SendBound(ctx, target, text) }
	}
	result := r.outbound.enqueue(compositeKey(msg.IMType, msg.AccountID, msg.ChatID), func() {
		if !r.admission.Valid(*msg.ticket) {
			recordDrop("authorization_changed")
			return
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		if ctx.Err() != nil {
			recordDrop("reply_expired")
			return
		}
		if send(ctx) != nil {
			recordDrop("delivery_failed")
		}
	})
	switch result {
	case outboundFull:
		recordDrop("outbound_full")
	case outboundClosed:
		recordDrop("outbound_closed")
	}
	return result
}

var errIngressRouteChanged = errors.New("ingress route changed")
