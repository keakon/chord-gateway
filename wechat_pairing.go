package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type wechatReplyContext struct {
	token         string
	used, expires time.Time
	bound         bool
}

// The server ID is uint64 on the wire. json.Number accepts both numeric and
// quoted encodings without passing through float64. client_id is an outbound
// client-generated identifier, not the identity of an inbound delivery.
func (msg ilinkMessage) inboundMessageID() string {
	if msg.MessageID != "" {
		id, err := strconv.ParseUint(msg.MessageID.String(), 10, 64)
		if err != nil || id == 0 {
			return ""
		}
		return strconv.FormatUint(id, 10)
	}
	for _, item := range msg.ItemList {
		if id := strings.TrimSpace(item.MessageID); id != "" {
			return id
		}
	}
	return ""
}

func (a *WechatAdapter) pairingRouter() *NotificationRouter {
	r, _ := a.msgRouter.(*NotificationRouter)
	return r
}

func (a *WechatAdapter) publishToken(token *TokenData) {
	r := a.pairingRouter()
	if r == nil || r.admission == nil {
		a.token.Store(token)
		return
	}
	p := r.admission.pairing
	p.store.txMu.Lock()
	p.mu.Lock()
	state := p.platforms["wechat"]
	changed := state != nil && token != nil && token.AccountID != "" && state.account != token.AccountID
	if state != nil {
		state.ready = false
		if changed {
			if state.cancel != nil {
				state.cancel()
			}
			state.epoch++
			state.changed = true
			state.nonce = [16]byte{}
		}
	}
	p.mu.Unlock()
	if changed {
		r.admission.mu.Lock()
		for id, g := range r.admission.gates {
			if id.Platform == "wechat" {
				lease, _, _ := g.snapshot()
				if lease != nil {
					lease.Close()
				}
			}
		}
		r.admission.mu.Unlock()
		a.mu.Lock()

		a.replyContexts = map[string]wechatReplyContext{}
		a.mu.Unlock()
	}
	a.token.Store(token)
	p.store.txMu.Unlock()
	if changed {
		go r.stopPairedPlatform("wechat")
	}
}

func (r *NotificationRouter) stopPairedPlatform(platform string) {
	r.admission.mu.Lock()
	var gates []*ChatGate
	for id, g := range r.admission.gates {
		if id.Platform == platform {
			gates = append(gates, g)
		}
	}
	r.admission.mu.Unlock()
	for _, g := range gates {
		g.opMu.Lock()
		lease, _, _ := g.snapshot()
		if lease != nil {
			key := (processKey{workspaceID: lease.WorkspaceID, imType: platform, chatID: lease.Identity.ChatID}).String()
			r.stopReminder(key)
			if p := r.mgr.GetProcessForKey(key); p != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = p.StopAndWait(ctx, 2*time.Second)
				cancel()
			}
		}
		g.mu.Lock()
		g.phase = "blocked"
		g.mu.Unlock()
		g.opMu.Unlock()
	}
}

func (a *WechatAdapter) apiPostSnapshot(ctx context.Context, token *TokenData, path string, body any) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if token == nil {
		return nil, errors.New("account unavailable")
	}
	base := strings.TrimRight(token.BaseURL, "/")
	if a.imCfg.Wechat != nil && a.imCfg.Wechat.BaseURL != "" {
		base = strings.TrimRight(a.imCfg.Wechat.BaseURL, "/")
	}
	if base == "" {
		return nil, errors.New("account endpoint unavailable")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", randomWechatUIN())
	if token.Token != "" {
		req.Header.Set("Authorization", "Bearer "+token.Token)
	}
	if path == "ilink/bot/sendmessage" {
		return messageHTTPClient(a.httpClient).Do(req)
	}
	return a.httpClient.Do(req)
}

func (a *WechatAdapter) rememberPairedContext(msg IncomingMessage, token string) {
	if token == "" || msg.ticket == nil {
		return
	}
	r := a.pairingRouter()
	if r == nil || r.admission == nil || !r.admission.Valid(*msg.ticket) {
		return
	}
	im := r.getConfig().IMConfigByType("wechat")
	owner := im.OwnerIdentity()
	// Bind requests from an already bound owner must not demote the task's
	// reply context, even when the bind is idempotent, invalid, or busy.
	bound := owner != nil && owner.AccountID == msg.AccountID && owner.UserID == msg.SenderID && im.Bindings()[msg.ChatID] != ""
	a.cachePairedContext(msg.AccountID, msg.ChatID, token, bound)
}

func (a *WechatAdapter) cachePairedContext(account, chat, token string, bound bool) {
	now := time.Now()
	key := compositeKey(account, chat)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.replyContexts == nil {
		a.replyContexts = map[string]wechatReplyContext{}
	}
	limit := 30
	if bound {
		limit = 256
	}
	count := 0
	oldestKey := ""
	var oldest time.Time
	for k, v := range a.replyContexts {
		if !v.bound && !now.Before(v.expires) {
			delete(a.replyContexts, k)
			continue
		}
		if v.bound == bound {
			count++
			if oldestKey == "" || v.used.Before(oldest) {
				oldestKey = k
				oldest = v.used
			}
		}
	}
	if previous, exists := a.replyContexts[key]; (!exists || previous.bound != bound) && count >= limit {
		delete(a.replyContexts, oldestKey)
	}
	a.replyContexts[key] = wechatReplyContext{token: token, used: now, expires: now.Add(30 * time.Second), bound: bound}
}

func (a *WechatAdapter) SendTextContext(ctx context.Context, chat, text string) error {
	token := a.token.Load()
	if token == nil {
		return errors.New("account unavailable")
	}
	if expected, ok := ctx.Value(replyAccountContextKey{}).(string); ok && token.AccountID != expected {
		return errors.New("account changed")
	}
	key := compositeKey(token.AccountID, chat)
	a.mu.Lock()
	reply, ok := a.replyContexts[key]
	if ok {
		reply.used = time.Now()
		a.replyContexts[key] = reply
	}
	a.mu.Unlock()
	if !ok || (!reply.bound && !time.Now().Before(reply.expires)) {
		return errors.New("reply context unavailable; wait for the next inbound message")
	}
	for i, part := range splitText(text, ilinkMaxMessageLen) {
		body := ilinkSendMessageRequest{Msg: ilinkMessage{ToUserID: chat, ClientID: fmt.Sprintf("chord-gateway-%d-%d", time.Now().UnixNano(), i), MessageType: ilinkMessageTypeBot, MessageState: ilinkMessageStateFinish, ContextToken: reply.token, ItemList: []ilinkItem{{Type: ilinkItemTypeText, TextItem: &ilinkTextItem{Text: part}}}}}
		if a.token.Load() != token {
			return errors.New("account changed")
		}
		if err := a.sendMessage(ctx, token, body); err != nil {
			return err
		}
	}
	return nil
}

func (a *WechatAdapter) savePairedSyncBuf(expected *TokenData) {
	r := a.pairingRouter()
	if r == nil || r.admission == nil {
		return
	}
	r.admission.pairing.store.txMu.Lock()
	defer r.admission.pairing.store.txMu.Unlock()
	if expected == nil || a.token.Load() != expected {
		return
	}
	a.saveSyncBuf()
}
