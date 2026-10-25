package wechat

// Test-only seams for the gateway joint tests, which live in the root package
// and cannot reach unexported members across the package boundary. Production
// code must not use anything in this file.

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// NewAdapterForTest builds an adapter wired to a temp state dir and a live
// context, mirroring what the root tests previously constructed inline. The
// adapter holds no host; tests attach one via SetHostForTest.
func NewAdapterForTest(t *testing.T) *WechatAdapter {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	storageDir := filepath.Join(dir, "wechat")
	a := &WechatAdapter{
		imCfg:         config.IMAdapterConfig{Wechat: &config.WechatConfig{}},
		storageDir:    storageDir,
		tokenFile:     filepath.Join(storageDir, "token.json"),
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		replyContexts: make(map[string]WechatReplyContext),
		ctx:           ctx,
		cancel:        cancel,
	}
	t.Cleanup(a.closeIngressAccount)
	a.token.Store(&TokenData{Token: "sample-token", AccountID: "bot"})
	return a
}

func (a *WechatAdapter) StoreTokenForTest(token *TokenData) { a.token.Store(token) }

func (a *WechatAdapter) TokenForTest() *TokenData { return a.token.Load() }

// SetBaseURLForTest points the adapter's configured endpoint at a test server.
func (a *WechatAdapter) SetBaseURLForTest(baseURL string) {
	if a.imCfg.Wechat == nil {
		a.imCfg.Wechat = &config.WechatConfig{}
	}
	a.imCfg.Wechat.BaseURL = baseURL
}

func (a *WechatAdapter) SetIMConfigForTest(imCfg config.IMAdapterConfig) { a.imCfg = imCfg }

func (a *WechatAdapter) SetHostForTest(host Host) { a.host, a.notifier = host, host }

func (a *WechatAdapter) SetTransportForTest(transport http.RoundTripper) {
	if a.httpClient == nil {
		a.httpClient = &http.Client{}
	}
	a.httpClient.Transport = transport
}

func (a *WechatAdapter) SyncBufForTest() string { return a.syncBuf }

// SetConnectionForTest replaces the adapter connection context. cancel may be
// nil when the caller owns the cancellation directly.
func (a *WechatAdapter) SetConnectionForTest(ctx context.Context, cancel context.CancelFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ctx, a.cancel = ctx, cancel
}

func (a *WechatAdapter) SetStoppedForTest(stopped bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopped = stopped
}

func (a *WechatAdapter) StoppedForTest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stopped
}

func (a *WechatAdapter) ReplyContextCountForTest() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.replyContexts)
}

func (a *WechatAdapter) IngressPendingCountForTest() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.ingressPending)
}

func (a *WechatAdapter) ReplyContextForTest(key string) (WechatReplyContext, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	reply, ok := a.replyContexts[key]
	return reply, ok
}

func (a *WechatAdapter) SetReplyContextForTest(key string, reply WechatReplyContext) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.replyContexts == nil {
		a.replyContexts = map[string]WechatReplyContext{}
	}
	a.replyContexts[key] = reply
}

// ReplaceReplyContextsForTest swaps the whole reply-context cache; nil resets it.
func (a *WechatAdapter) ReplaceReplyContextsForTest(contexts map[string]WechatReplyContext) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replyContexts = contexts
}

func (a *WechatAdapter) IngressPendingForTest(key string) *WechatIngressPending {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ingressPending[key]
}

// ReplaceIngressPendingForTest swaps the whole pending-input map; nil resets it.
func (a *WechatAdapter) ReplaceIngressPendingForTest(pending map[string]*WechatIngressPending) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ingressPending = pending
}

func (a *WechatAdapter) PublishTokenForTest(token *TokenData) { a.publishToken(token) }

func (a *WechatAdapter) SaveTokenForTest() { a.saveToken() }

func (a *WechatAdapter) LoadTokenForTest() *TokenData { return a.loadToken() }

func (a *WechatAdapter) ConfirmIncomingContextForTest(msg ILinkMessage, token *TokenData, result im.ReceiveResult, pending *WechatIngressPending) {
	a.confirmIncomingContext(msg, token, result, pending)
}

func (a *WechatAdapter) AccountLockForTest() *im.InstanceLocks { return a.accountLock }

func (a *WechatAdapter) CloseIngressAccountForTest() { a.closeIngressAccount() }

func (a *WechatAdapter) PrepareIngressAccountForTest(ctx context.Context, token *TokenData) (im.IngressCursor, error) {
	return a.prepareIngressAccount(ctx, token)
}

func (a *WechatAdapter) PollingContextForTest(ctx context.Context, token *TokenData) (context.Context, context.CancelFunc) {
	return a.pollingContext(ctx, token)
}

func (a *WechatAdapter) SwapPollingTokenForTest(token *TokenData) { a.swapPollingToken(token) }

func (a *WechatAdapter) CurrentPollingEpochForTest(token *TokenData) (uint64, bool) {
	return a.currentPollingEpoch(token)
}

func (a *WechatAdapter) CommitPollingCursorForTest(ctx context.Context, token *TokenData, epoch uint64, previous, next im.IngressCursor) error {
	return a.commitPollingCursor(ctx, token, epoch, previous, next)
}

func (a *WechatAdapter) ReceiveIncomingMessageForTest(ctx context.Context, msg ILinkMessage, token *TokenData) im.ReceiveResult {
	return a.receiveIncomingMessage(ctx, msg, token)
}

func (a *WechatAdapter) ResolveIncomingMessageForTest(ctx context.Context, msg ILinkMessage, token *TokenData, previous im.ReceiveResult) im.ReceiveResult {
	return a.resolveIncomingMessage(ctx, msg, token, previous)
}

func (a *WechatAdapter) CapturePollingMessagesForTest(response *ILinkGetUpdatesResponse, token *TokenData) {
	a.capturePollingMessages(response, token)
}

func (a *WechatAdapter) CachePairedContextForTest(account, chat, token string, bound bool) {
	a.cachePairedContext(account, chat, token, bound)
}

func (a *WechatAdapter) SendMessageForTest(ctx context.Context, account *TokenData, body ILinkSendMessageRequest) error {
	return a.sendMessage(ctx, account, body)
}

func (a *WechatAdapter) GetUpdatesForTest() (*ILinkGetUpdatesResponse, error) { return a.getUpdates() }

func (a *WechatAdapter) GetUpdatesSnapshotForTest(ctx context.Context, token *TokenData, buf string) (*ILinkGetUpdatesResponse, error) {
	return a.getUpdatesSnapshot(ctx, token, buf)
}

func (a *WechatAdapter) MonitorLoopForTest(ctx context.Context) error { return a.monitorLoop(ctx) }

func (a *WechatAdapter) ConnectILinkForTest(ctx context.Context) error { return a.connectILink(ctx) }

func (a *WechatAdapter) AdapterContextForTest() context.Context { return a.adapterContext() }

// EncryptILinkImageForTest exposes the pure media cipher to the root joint
// image tests; the production caller is SendImageContext.
func EncryptILinkImageForTest(data, key []byte) ([]byte, error) {
	return encryptILinkImage(data, key)
}

// BaseURLForTest exposes the resolved account endpoint.
func (a *WechatAdapter) BaseURLForTest() string { return a.baseURL() }
