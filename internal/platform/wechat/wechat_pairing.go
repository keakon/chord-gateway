package wechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// WechatReplyContext caches one verified inbound reply context token. The
// gateway joint tests inspect entries through the ForTest accessors.
type WechatReplyContext struct {
	Token         string
	Used, Expires time.Time
	Bound         bool
	Committed     bool
	AccountEpoch  uint64
}

// The server ID is uint64 on the wire. json.Number accepts both numeric and
// quoted encodings without passing through float64. client_id is an outbound
// client-generated identifier, not the identity of an inbound delivery.
func (msg ILinkMessage) inboundMessageID() string {
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

func (a *WechatAdapter) publishToken(token *TokenData) {
	if a.host == nil {
		a.swapPollingToken(token)
		return
	}
	account := ""
	if token != nil {
		account = token.AccountID
	}
	// The host invalidates the paired platform state and closes the binding
	// gates under the config-store transaction lock; this callback swaps the
	// adapter credential inside that same span. The host's changed flag is the
	// authority for clearing account-scoped caches: after a session expiry the
	// adapter token may already be nil while the paired account is still known.
	a.host.ReplaceTransportAccount("wechat", account, func(changed bool) {
		if changed {
			a.mu.Lock()
			a.replyContexts = map[string]WechatReplyContext{}
			a.ingressPending = nil
			a.mu.Unlock()
		}
		a.swapPollingToken(token)
	})
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
		return im.MessageHTTPClient(a.httpClient).Do(req)
	}
	return a.httpClient.Do(req)
}

func (a *WechatAdapter) cachePairedContext(account, chat, token string, bound bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cachePairedContextLocked(account, chat, token, bound)
}

func (a *WechatAdapter) cachePairedContextLocked(account, chat, token string, bound bool) {
	now := time.Now()
	key := compositeKey(account, chat)
	if a.replyContexts == nil {
		a.replyContexts = map[string]WechatReplyContext{}
	}
	limit := 30
	if bound {
		limit = 256
	}
	count := 0
	oldestKey := ""
	var oldest time.Time
	for k, v := range a.replyContexts {
		if !v.Bound && !now.Before(v.Expires) {
			delete(a.replyContexts, k)
			continue
		}
		if v.Bound == bound {
			count++
			if oldestKey == "" || v.Used.Before(oldest) {
				oldestKey = k
				oldest = v.Used
			}
		}
	}
	if previous, exists := a.replyContexts[key]; (!exists || previous.Bound != bound) && count >= limit {
		delete(a.replyContexts, oldestKey)
	}
	a.replyContexts[key] = WechatReplyContext{Token: token, Used: now, Expires: now.Add(30 * time.Second), Bound: bound, Committed: true}
}

func (a *WechatAdapter) SendTextContext(ctx context.Context, chat, text string) error {
	token := a.token.Load()
	if token == nil {
		return errors.New("account unavailable")
	}
	if expected, ok := ctx.Value(im.ReplyAccountContextKey{}).(string); ok && token.AccountID != expected {
		return errors.New("account changed")
	}
	key := compositeKey(token.AccountID, chat)
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return errors.New("wechat adapter stopped")
	}
	adapterCtx := a.ctx
	reply, ok := a.replyContexts[key]
	if ok {
		reply.Used = time.Now()
		a.replyContexts[key] = reply
	}
	a.mu.Unlock()
	if !ok || (!reply.Bound && !time.Now().Before(reply.Expires)) {
		return errors.New("reply context unavailable; wait for the next inbound message")
	}
	ctx, cancel := context.WithCancel(ctx)
	if adapterCtx == nil {
		adapterCtx = context.Background()
	}
	stop := context.AfterFunc(adapterCtx, cancel)
	defer stop()
	defer cancel()
	if adapterCtx.Err() != nil {
		cancel()
	}
	for i, part := range splitText(text, ILinkMaxMessageLen) {
		body := ILinkSendMessageRequest{Msg: ILinkMessage{ToUserID: chat, ClientID: fmt.Sprintf("chord-gateway-%d-%d", time.Now().UnixNano(), i), MessageType: ILinkMessageTypeBot, MessageState: ILinkMessageStateFinish, ContextToken: reply.Token, ItemList: []ILinkItem{{Type: ILinkItemTypeText, TextItem: &ILinkTextItem{Text: part}}}}}
		a.mu.Lock()
		stopped := a.stopped
		a.mu.Unlock()
		if stopped {
			return errors.New("wechat adapter stopped")
		}
		if a.token.Load() != token {
			return errors.New("account changed")
		}
		if err := a.sendMessage(ctx, token, body); err != nil {
			return err
		}
	}
	return nil
}

// clearTokenSnapshot cannot erase a replacement credential when an old polling
// request discovers that its captured session expired. The host clears the
// pairing state under the config-store transaction lock while the callback
// erases the adapter credential inside that same span.
func (a *WechatAdapter) clearTokenSnapshot(expected *TokenData) bool {
	cleared := false
	clear := func() {
		a.mu.Lock()
		if expected == nil || a.token.Load() != expected {
			a.mu.Unlock()
			return
		}
		if a.pollCancel != nil {
			a.pollCancel()
			a.pollCancel, a.pollToken = nil, nil
		}
		a.token.Store(nil)
		a.mu.Unlock()
		_ = os.Remove(a.tokenFile)
		cleared = true
	}
	if a.host == nil {
		clear()
		return cleared
	}
	a.host.ExpireTransportAccount("wechat", clear)
	return cleared
}

// compositeKey joins parts with the canonical "|" separator used across the
// gateway for cache keys that are not parsed back into opaque parts.
func compositeKey(parts ...string) string {
	return strings.Join(parts, "|")
}
