package wechat

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/internal/im"
)

const ILinkItemTypeImage = 2
const ILinkCDNBaseURL = "https://novac2c.cdn.weixin.qq.com/c2c"

// CDN uploads are keyed by filekey, so repeating one is safe; message sends are
// never replayed. Retries only cover transient failures, and the upload slot
// already serializes them, so a fixed backoff is enough.
const (
	ILinkCDNUploadAttempts  = 3
	ILinkMediaUploadTimeout = 3 * time.Minute
)

// ILinkCDNUploadBackoff is variable so the upload retry test does not wait out
// real backoff windows. Production keeps the value below.
var ILinkCDNUploadBackoff = 500 * time.Millisecond

type ILinkCDNMedia struct {
	EncryptQueryParam string `json:"encrypt_query_param"`
	AESKey            string `json:"aes_key"`
	EncryptType       int    `json:"encrypt_type"`
}
type ILinkImageItem struct {
	Media ILinkCDNMedia `json:"media"`
	// The reference client's decoder prefers this 32-hex key over media.aes_key.
	AESKey  string `json:"aeskey,omitempty"`
	MidSize int    `json:"mid_size"`
}

// mediaClient returns a client without the adapter-wide request timeout. The
// long-poll budget must not cap a multi-megabyte media upload; the caller bounds
// that request with a context deadline instead.
func (a *WechatAdapter) mediaClient() *http.Client {
	if a.httpClient == nil {
		return &http.Client{Transport: im.ProxiedTransport(a.proxyFunc)}
	}
	client := *a.httpClient
	client.Timeout = 0
	// The CDN request never carries the bot's Authorization header, and a
	// redirect must not forward the ciphertext to another host.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

// iLink media uploads encrypt the original with raw AES-ECB and PKCS#7 padding,
// carry the raw key as base64 in media and as hex on the image item, and take
// the download query parameter from the upload's x-encrypted-param header.
//
// The padding is appended in place so an image of tens of megabytes does not
// need a second full-size buffer. When the input has no spare capacity append
// reallocates, so the memory saving depends on the input's spare capacity.
// The caller must not read data as plaintext afterwards.
func encryptILinkImage(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create media cipher: %w", err)
	}
	padding := aes.BlockSize - len(data)%aes.BlockSize
	padded := append(data, make([]byte, padding)...)
	for i := len(data); i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	for offset := 0; offset < len(padded); offset += aes.BlockSize {
		block.Encrypt(padded[offset:offset+aes.BlockSize], padded[offset:offset+aes.BlockSize])
	}
	return padded, nil
}

func (a *WechatAdapter) SendImage(chatID string, image *im.ReceivedImage) error {
	return a.SendImageContext(a.adapterContext(), chatID, image)
}
func (a *WechatAdapter) SendImageContext(ctx context.Context, chatID string, image *im.ReceivedImage) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	stopped, connection := a.stopped, a.ctx
	a.mu.Unlock()
	if stopped {
		return context.Canceled
	}
	ctx, cancelConnection := context.WithCancel(ctx)
	defer cancelConnection()
	if connection != nil {
		stop := context.AfterFunc(connection, cancelConnection)
		defer stop()
		if connection.Err() != nil {
			cancelConnection()
		}
	}
	account := a.token.Load()
	if account == nil {
		return fmt.Errorf("account unavailable")
	}
	if expected, ok := ctx.Value(im.ReplyAccountContextKey{}).(string); ok && account.AccountID != expected {
		return fmt.Errorf("account changed")
	}
	release, err := im.AcquireImageUpload(ctx)
	if err != nil {
		return err
	}
	defer release()
	data, err := im.ReadReceivedImage(image)
	if err != nil {
		return err
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	fileKey := make([]byte, 16)
	_, _ = rand.Read(fileKey)
	keyHex := hex.EncodeToString(key)
	// Digest the plaintext first: the cipher grows data in place, so reading it
	// afterwards would hash ciphertext.
	digest := md5.Sum(data)
	ciphertext, err := encryptILinkImage(data, key)
	if err != nil {
		return err
	}
	post := func(path string, body any) (*http.Response, error) {
		if a.token.Load() != account {
			return nil, fmt.Errorf("account changed")
		}
		return a.apiPostSnapshot(ctx, account, path, body)
	}
	resp, err := post("ilink/bot/getuploadurl", map[string]any{"filekey": hex.EncodeToString(fileKey), "media_type": 1, "to_user_id": chatID, "rawsize": len(data), "rawfilemd5": hex.EncodeToString(digest[:]), "filesize": len(ciphertext), "no_need_thumb": true, "aeskey": keyHex, "base_info": ILinkBaseInfo{ChannelVersion: ILinkChannelVersion}})
	if err != nil {
		return fmt.Errorf("get image upload URL: %w", err)
	}
	var upload struct {
		Ret         int    `json:"ret"`
		ErrCode     int    `json:"errcode"`
		UploadParam string `json:"upload_param"`
		FullURL     string `json:"upload_full_url"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&upload)
	_ = resp.Body.Close()
	if decodeErr != nil {
		return fmt.Errorf("decode media upload parameters: %w", decodeErr)
	}
	if resp.StatusCode != http.StatusOK || upload.Ret != 0 || upload.ErrCode != 0 {
		return fmt.Errorf("WeChat media upload rejected: HTTP %d ret=%d code=%d", resp.StatusCode, upload.Ret, upload.ErrCode)
	}
	uploadURL := upload.FullURL
	if uploadURL == "" && upload.UploadParam != "" {
		uploadURL = ILinkCDNBaseURL + "/upload?" + url.Values{"encrypted_query_param": {upload.UploadParam}, "filekey": {hex.EncodeToString(fileKey)}}.Encode()
	}
	parsed, err := url.Parse(uploadURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("invalid WeChat media upload URL")
	}
	if a.token.Load() != account {
		return fmt.Errorf("account changed")
	}
	ctx, cancel := context.WithTimeout(ctx, ILinkMediaUploadTimeout)
	defer cancel()
	downloadParam, err := a.uploadILinkMedia(ctx, uploadURL, ciphertext)
	if err != nil {
		return err
	}
	item := ILinkItem{Type: ILinkItemTypeImage, ImageItem: &ILinkImageItem{Media: ILinkCDNMedia{EncryptQueryParam: downloadParam, AESKey: base64.StdEncoding.EncodeToString(key), EncryptType: 1}, AESKey: keyHex, MidSize: len(ciphertext)}}
	a.mu.Lock()
	token := ""

	reply := a.replyContexts[compositeKey(account.AccountID, chatID)]
	if reply.Bound || time.Now().Before(reply.Expires) {
		token = reply.Token
	}

	a.mu.Unlock()
	body := ILinkSendMessageRequest{Msg: ILinkMessage{ToUserID: chatID, ClientID: "chord-gateway-image-" + hex.EncodeToString(fileKey), MessageType: ILinkMessageTypeBot, MessageState: ILinkMessageStateFinish, ContextToken: token, ItemList: []ILinkItem{item}}}

	if token == "" || a.token.Load() != account {
		return fmt.Errorf("reply context unavailable")
	}
	return a.sendMessage(ctx, account, body)
}

func (a *WechatAdapter) uploadILinkMedia(ctx context.Context, uploadURL string, ciphertext []byte) (string, error) {
	client := a.mediaClient()
	var lastErr error
	for attempt := 0; attempt < ILinkCDNUploadAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * ILinkCDNUploadBackoff):
			}
		}
		param, retryable, err := postILinkMedia(ctx, client, uploadURL, ciphertext)
		if err == nil {
			return param, nil
		}
		lastErr = err
		if !retryable {
			return "", err
		}
		log.Warnf("wechat ilink: media upload attempt %d/%d failed error=%v", attempt+1, ILinkCDNUploadAttempts, err)
	}
	return "", fmt.Errorf("WeChat media upload failed after %d attempts: %w", ILinkCDNUploadAttempts, lastErr)
}

// postILinkMedia reports whether the failure is worth retrying. A 4xx means the
// request itself is invalid, so repeating it cannot succeed.
func postILinkMedia(ctx context.Context, client *http.Client, uploadURL string, ciphertext []byte) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(ciphertext))
	if err != nil {
		return "", false, fmt.Errorf("create media upload request")
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	cdn, err := client.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("WeChat CDN upload failed: %w", err)
	}
	downloadParam := cdn.Header.Get("x-encrypted-param")
	diagnostic := cdn.Header.Get("x-error-message")
	_, _ = io.Copy(io.Discard, io.LimitReader(cdn.Body, 1<<20))
	_ = cdn.Body.Close()
	if cdn.StatusCode >= 400 && cdn.StatusCode < 500 {
		return "", false, fmt.Errorf("WeChat CDN upload rejected: HTTP %d %s", cdn.StatusCode, diagnostic)
	}
	if cdn.StatusCode != http.StatusOK || downloadParam == "" {
		retryable := cdn.StatusCode >= http.StatusInternalServerError || cdn.StatusCode == http.StatusOK
		return "", retryable, fmt.Errorf("WeChat CDN upload failed: HTTP %d %s", cdn.StatusCode, diagnostic)
	}
	return downloadParam, false, nil
}
