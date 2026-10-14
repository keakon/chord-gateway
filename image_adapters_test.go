package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func imageForUpload(t *testing.T) *ReceivedImage {
	t.Helper()
	data := testImageBytes(t)
	m := testImageMetadata(data)
	f, err := os.CreateTemp(t.TempDir(), "image-*")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(data)
	_ = f.Close()
	return &ReceivedImage{Path: f.Name(), Size: m.Size, SHA256: m.SHA256, MIME: m.MIME, Width: m.Width, Height: m.Height}
}

// The cipher grows the plaintext buffer in place, so pin both allocation paths:
// one where spare capacity avoids a reallocation and one where append must
// allocate. Both must produce valid PKCS#7 padding and a lossless round trip.
func TestEncryptILinkImagePadsInPlace(t *testing.T) {
	key := []byte("0123456789abcdef")
	plain := []byte("an image payload that is not a multiple of the block size")
	for _, spare := range []int{0, aes.BlockSize} {
		buf := make([]byte, len(plain), len(plain)+spare)
		copy(buf, plain)
		ciphertext, err := encryptILinkImage(buf, key)
		if err != nil {
			t.Fatalf("spare=%d: encrypt: %v", spare, err)
		}
		if len(ciphertext)%aes.BlockSize != 0 || len(ciphertext) <= len(plain) {
			t.Fatalf("spare=%d: bad ciphertext length %d for %d plaintext bytes", spare, len(ciphertext), len(plain))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			t.Fatalf("spare=%d: cipher: %v", spare, err)
		}
		decrypted := make([]byte, len(ciphertext))
		copy(decrypted, ciphertext)
		for i := 0; i < len(decrypted); i += aes.BlockSize {
			block.Decrypt(decrypted[i:i+aes.BlockSize], decrypted[i:i+aes.BlockSize])
		}
		padding := int(decrypted[len(decrypted)-1])
		if padding <= 0 || padding > aes.BlockSize {
			t.Fatalf("spare=%d: invalid PKCS#7 padding byte %d", spare, padding)
		}
		for _, value := range decrypted[len(decrypted)-padding:] {
			if int(value) != padding {
				t.Fatalf("spare=%d: inconsistent PKCS#7 padding", spare)
			}
		}
		if !bytes.Equal(decrypted[:len(decrypted)-padding], plain) {
			t.Fatalf("spare=%d: round trip changed the plaintext", spare)
		}
	}
}

func TestWechatImageUploadEncryptionAndContext(t *testing.T) {
	image := imageForUpload(t)
	data, _ := os.ReadFile(image.Path)
	keyHex := ""
	sent := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/getuploadurl":
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			keyHex, _ = p["aeskey"].(string)
			if p["no_need_thumb"] != true || int(p["rawsize"].(float64)) != len(data) {
				t.Error("upload metadata lost")
			}
			digest := md5.Sum(data)
			if p["rawfilemd5"] != hex.EncodeToString(digest[:]) {
				t.Error("upload digest does not match the plaintext")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"upload_full_url": server.URL + "/cdn"})
		case "/cdn":
			if r.Header.Get("Authorization") != "" {
				t.Error("bot credentials leaked to CDN")
			}
			ciphertext, _ := io.ReadAll(r.Body)
			key, _ := hex.DecodeString(keyHex)
			block, _ := aes.NewCipher(key)
			for i := 0; i < len(ciphertext); i += aes.BlockSize {
				block.Decrypt(ciphertext[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
			}
			if !bytes.Equal(ciphertext[:len(data)], data) {
				t.Error("image encryption corrupted original")
			}
			w.Header().Set("x-encrypted-param", "download-param")
		case "/ilink/bot/sendmessage":
			var p ilinkSendMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&p)
			sent++
			// The adapter resolves the chat's reply token itself.
			if p.Msg.ContextToken != "new-context" || p.Msg.ToUserID != "sample-user" || len(p.Msg.ItemList) != 1 {
				t.Error("message source binding lost")
			}
			item := p.Msg.ItemList[0]
			decoded, _ := base64.StdEncoding.DecodeString(item.ImageItem.Media.AESKey)
			if item.Type != ilinkItemTypeImage || item.ImageItem.Media.EncryptQueryParam != "download-param" {
				t.Error("media reference lost")
			}
			// Both spellings are sent: the raw key base64-encoded under media, and
			// the 32-hex key on the item root where it takes priority.
			if hex.EncodeToString(decoded) != keyHex || item.ImageItem.AESKey != keyHex {
				t.Error("media key encoding lost")
			}
			_, _ = w.Write([]byte(`{"ret":0}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	a := newTestWechatAdapter(t)
	a.imCfg.Wechat.BaseURL = server.URL
	a.token.Store(&TokenData{AccountID: "bot"})
	a.cachePairedContext("bot", "sample-user", "new-context", true)
	if err := a.SendImage("sample-user", image); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatal("image not sent exactly once")
	}
}

func TestFeishuImageUploadTenantTokenAndRefresh(t *testing.T) {
	image := imageForUpload(t)
	tokens, uploads, sends := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			tokens++
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"tenant-key","expire":3600}`))
		case "/open-apis/im/v1/images":
			uploads++
			if r.Header.Get("Authorization") != "Bearer tenant-key" {
				t.Error("wrong token type")
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			defer r.MultipartForm.RemoveAll()
			if r.FormValue("image_type") != "message" {
				t.Error("wrong image type")
			}
			f, header, err := r.FormFile("image")
			if err != nil {
				t.Error(err)
			} else {
				if header.Filename != "image.png" || header.Header.Get("Content-Type") != "image/png" {
					t.Error("image part is missing its filename extension or MIME type")
				}
				received, _ := io.ReadAll(f)
				_ = f.Close()
				original, _ := os.ReadFile(image.Path)
				if !bytes.Equal(received, original) {
					t.Error("original changed")
				}
			}
			if uploads == 1 {
				_, _ = w.Write([]byte(`{"code":99991663}`))
			} else {
				_, _ = w.Write([]byte(`{"code":0,"data":{"image_key":"image-key"}}`))
			}
		case "/open-apis/im/v1/messages":
			sends++
			var p map[string]string
			_ = json.NewDecoder(r.Body).Decode(&p)
			if p["msg_type"] != "image" || p["receive_id"] != "sample-chat" || p["content"] != `{"image_key":"image-key"}` {
				t.Error("wrong image message")
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"message_id":"message-1"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	originalURL := feishuOpenBaseURL
	feishuOpenBaseURL = server.URL
	defer func() { feishuOpenBaseURL = originalURL }()
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "sample-app", AppSecret: "sample-secret"})
	a.httpClient = server.Client()
	if err := a.SendImage("sample-chat", image); err != nil {
		t.Fatal(err)
	}
	if tokens != 2 || uploads != 2 || sends != 1 {
		t.Fatalf("tokens=%d uploads=%d sends=%d", tokens, uploads, sends)
	}
}

func TestWechatSendResponseFailureIsNotRetried(t *testing.T) {
	for _, raw := range []string{`{"ret":-2}`, `{"errcode":-3}`, `unparseable`, `{}`, `null`, `{"ret":null}`} {
		t.Run(raw, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.WriteString(w, raw) }))
			defer srv.Close()
			a := newTestWechatAdapter(t)
			a.imCfg.Wechat.BaseURL = srv.URL
			if a.sendMessage(t.Context(), a.token.Load(), ilinkSendMessageRequest{}) == nil {
				t.Fatal("invalid or rejected receipt reported success")
			}
			if requests.Load() != 1 {
				t.Fatal("unknown send replayed")
			}
		})
	}
}

func TestImageUploadPreviewPreservesAlphaAndLimit(t *testing.T) {
	original := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	original.SetNRGBA(0, 0, color.NRGBA{R: 200, A: 80})
	var data bytes.Buffer
	if err := png.Encode(&data, original); err != nil {
		t.Fatal(err)
	}
	preview, err := imageUploadPreview(t.Context(), bytes.NewReader(data.Bytes()), 1024)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(preview))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, alpha := decoded.At(0, 0).RGBA()
	if alpha >= 65535 || len(preview) >= 1024 {
		t.Fatal("preview lost transparency or limit")
	}
	if _, err := imageUploadPreview(t.Context(), strings.NewReader("invalid"), 1024); err == nil {
		t.Fatal("invalid image accepted")
	}
}

func TestFeishuImageSendRequiresConfirmedReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		unknown    bool
	}{
		{"missing_code", `{}`, http.StatusOK, true},
		{"null", `null`, http.StatusOK, true},
		{"null_code", `{"code":null}`, http.StatusOK, true},
		{"invalid", `invalid`, http.StatusOK, true},
		{"missing_message_id", `{"code":0}`, http.StatusOK, true},
		{"blank_message_id", `{"code":0,"data":{"message_id":" "}}`, http.StatusOK, true},
		{"gateway_error", `{}`, http.StatusBadGateway, true},
		{"http_error_with_success", `{"code":0,"data":{"message_id":"sample-message"}}`, http.StatusBadGateway, true},
		{"http_error_with_expiry", `{"code":99991663}`, http.StatusBadGateway, true},
		{"rejected", `{"code":123,"msg":"rejected"}`, http.StatusOK, false},
		{"rejected_http_400", `{"code":230001,"msg":"Invalid request param."}`, http.StatusBadRequest, false},
		{"invalid_body_http_400", `invalid`, http.StatusBadRequest, false},
		{"success_body_http_400", `{"code":0,"data":{"message_id":"sample-message"}}`, http.StatusBadRequest, false},
		{"unexpected_http_201", `{"code":0,"data":{"message_id":"sample-message"}}`, http.StatusCreated, true},
		{"unexpected_http_202", `{}`, http.StatusAccepted, true},
		{"rate_limited_http_429", `{"code":99991400}`, http.StatusTooManyRequests, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var uploads, sends atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/open-apis/im/v1/images":
					uploads.Add(1)
					io.WriteString(w, `{"code":0,"data":{"image_key":"sample-image"}}`)
				case "/open-apis/im/v1/messages":
					sends.Add(1)
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			oldURL := feishuOpenBaseURL
			feishuOpenBaseURL = srv.URL
			defer func() { feishuOpenBaseURL = oldURL }()
			a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "sample-app", AppSecret: "sample-secret"})

			a.accessToken, a.tokenExpireAt = "sample-token", time.Now().Add(time.Hour)
			a.httpClient = srv.Client()
			err := a.SendImage("sample-chat", imageForUpload(t))
			if err == nil || errors.Is(err, errDeliveryOutcomeUnknown) != tc.unknown {
				t.Fatalf("receipt error = %v", err)
			}
			if uploads.Load() != 1 || sends.Load() != 1 {
				t.Fatal("image uploaded or sent again")
			}
		})
	}
}

func TestWechatSendRequiresExplicitReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		unknown    bool
	}{
		{"missing", `{}`, http.StatusOK, true},
		{"null", `null`, http.StatusOK, true},
		{"null_ret", `{"ret":null}`, http.StatusOK, true},
		{"null_status", `{"ret":null,"errcode":null}`, http.StatusOK, true},
		{"invalid", `invalid`, http.StatusOK, true},
		{"gateway_error", `{"ret":0}`, http.StatusBadGateway, true},
		{"rejected", `{"ret":-2}`, http.StatusOK, false},
		{"errcode", `{"errcode":-3}`, http.StatusOK, false},
		{"ret_error_with_zero_code", `{"ret":-2,"errcode":0}`, http.StatusOK, false},
		{"code_error_with_zero_ret", `{"ret":0,"errcode":-3}`, http.StatusOK, false},
		{"rejected_http_401", `{"ret":0}`, http.StatusUnauthorized, false},
		{"rate_limited_http_429", `{"ret":0}`, http.StatusTooManyRequests, true},
		{"unexpected_http_201", `{"ret":0}`, http.StatusCreated, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			a := newTestWechatAdapter(t)
			a.imCfg.Wechat.BaseURL = srv.URL
			err := a.sendMessage(t.Context(), a.token.Load(), ilinkSendMessageRequest{})
			if err == nil || errors.Is(err, errDeliveryOutcomeUnknown) != tc.unknown {
				t.Fatalf("receipt error = %v", err)
			}
			if requests.Load() != 1 {
				t.Fatal("unconfirmed send retried")
			}
		})
	}
}

func TestFeishuImageSendRefreshRequiresFreshReceipt(t *testing.T) {
	for _, body := range []string{`{"code":0,"data":{"message_id":"sample-message"}}`, `{}`, `{"code":0}`} {
		t.Run(body, func(t *testing.T) {
			var tokens, uploads, sends atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/open-apis/auth/v3/tenant_access_token/internal":
					tokens.Add(1)
					io.WriteString(w, `{"code":0,"tenant_access_token":"fresh-token","expire":3600}`)
				case "/open-apis/im/v1/images":
					uploads.Add(1)
					io.WriteString(w, `{"code":0,"data":{"image_key":"sample-image"}}`)
				case "/open-apis/im/v1/messages":
					if sends.Add(1) == 1 {
						io.WriteString(w, `{"code":99991663,"data":{"message_id":"unconfirmed-message"}}`)
					} else {
						io.WriteString(w, body)
					}
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			oldURL := feishuOpenBaseURL
			feishuOpenBaseURL = srv.URL
			defer func() { feishuOpenBaseURL = oldURL }()
			a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "sample-app", AppSecret: "sample-secret"})

			a.accessToken, a.tokenExpireAt = "old-token", time.Now().Add(time.Hour)
			a.httpClient = srv.Client()
			err := a.SendImage("sample-chat", imageForUpload(t))
			success := strings.Contains(body, "sample-message")
			if success && err != nil || !success && !errors.Is(err, errDeliveryOutcomeUnknown) {
				t.Fatalf("fresh send receipt: %v", err)
			}
			if tokens.Load() != 1 || uploads.Load() != 1 || sends.Load() != 2 {
				t.Fatal("unexpected refresh or replay")
			}
		})
	}
}

func TestFeishuUploadRequiresExplicitSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing_code", `{"data":{"image_key":"sample-image"}}`, http.StatusOK},
		{"null_code", `{"code":null,"data":{"image_key":"sample-image"}}`, http.StatusOK},
		{"missing_key", `{"code":0}`, http.StatusOK},
		{"http_error_with_expiry", `{"code":99991663}`, http.StatusBadGateway},
		{"success_body_http_400", `{"code":0,"data":{"image_key":"sample-image"}}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			oldURL := feishuOpenBaseURL
			feishuOpenBaseURL = srv.URL
			defer func() { feishuOpenBaseURL = oldURL }()
			a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "sample-app", AppSecret: "sample-secret"})

			a.accessToken, a.tokenExpireAt = "sample-token", time.Now().Add(time.Hour)
			a.httpClient = srv.Client()
			data := testImageBytes(t)
			if _, err := a.uploadImage(context.Background(), bytes.NewReader(data), int64(len(data)), "sample.png", "image/png", false); err == nil {
				t.Fatal("invalid upload receipt accepted")
			}
			if requests.Load() != 1 {
				t.Fatal("unconfirmed upload retried")
			}
		})
	}
}

func TestFeishuPreviewNoteFailureKeepsConfirmedDelivery(t *testing.T) {
	img := imageForUpload(t)
	// A PNG with a large ancillary chunk reaches the platform byte limit without
	// an expensive random-pixel fixture. The preview decodes the original pixels.
	original, err := os.ReadFile(img.Path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, feishuImageUploadLimit)
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(chunk)-12))
	copy(chunk[4:8], "teSt")
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	data := append(append(append([]byte(nil), original[:len(original)-12]...), chunk...), original[len(original)-12:]...)
	if err := os.WriteFile(img.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	meta := testImageMetadata(data)
	img.Size, img.SHA256 = meta.Size, meta.SHA256
	var uploads, images, notes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/im/v1/images":
			uploads.Add(1)
			io.WriteString(w, `{"code":0,"data":{"image_key":"preview-image"}}`)
		case "/open-apis/im/v1/messages":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["msg_type"] == "image" {
				images.Add(1)
				io.WriteString(w, `{"code":0,"data":{"message_id":"sample-message"}}`)
			} else {
				notes.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	oldURL := feishuOpenBaseURL
	feishuOpenBaseURL = srv.URL
	defer func() { feishuOpenBaseURL = oldURL }()
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "sample-app", AppSecret: "sample-secret"})

	a.accessToken, a.tokenExpireAt = "sample-token", time.Now().Add(time.Hour)
	a.httpClient = srv.Client()
	if err := a.SendImage("sample-chat", img); err != nil {
		t.Fatalf("confirmed preview reported as failed: %v", err)
	}
	if uploads.Load() != 1 || images.Load() != 1 || notes.Load() != 1 {
		t.Fatal("preview or note replayed")
	}
}

func TestWechatSendAcceptsExplicitSuccessStatus(t *testing.T) {
	for _, body := range []string{`{"ret":0}`, `{"errcode":0}`, `{"ret":0,"errcode":0}`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer srv.Close()
			a := newTestWechatAdapter(t)
			a.imCfg.Wechat.BaseURL = srv.URL
			if err := a.sendMessage(t.Context(), a.token.Load(), ilinkSendMessageRequest{}); err != nil {
				t.Fatalf("explicit success rejected: %v", err)
			}
		})
	}
}

// The CDN upload is keyed by filekey, so a transient failure may be retried; a
// 4xx rejection may not, because repeating an invalid request cannot succeed.
func TestWechatMediaUploadRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failStatus   int
		failAttempts int
		wantAttempts int
		wantErr      bool
	}{
		{"transient_then_success", http.StatusBadGateway, 2, 3, false},
		{"server_error_exhausted", http.StatusInternalServerError, 3, 3, true},
		{"rejection_not_retried", http.StatusBadRequest, 1, 1, true},
		{"redirect_not_retried", http.StatusTemporaryRedirect, 1, 1, true},
		{"missing_header_then_success", http.StatusOK, 1, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/ilink/bot/getuploadurl":
					_ = json.NewEncoder(w).Encode(map[string]string{"upload_full_url": server.URL + "/cdn"})
				case "/cdn":
					attempts++
					if attempts <= tc.failAttempts {
						w.Header().Set("x-error-message", "busy")
						w.WriteHeader(tc.failStatus)
						return
					}
					w.Header().Set("x-encrypted-param", "download-param")
				case "/ilink/bot/sendmessage":
					_, _ = w.Write([]byte(`{"ret":0}`))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			a := newTestWechatAdapter(t)
			a.imCfg.Wechat.BaseURL = server.URL
			a.cachePairedContext("bot", "sample-user", "context", true)
			err := a.SendImage("sample-user", imageForUpload(t))
			if (err != nil) != tc.wantErr {
				t.Fatalf("upload error = %v", err)
			}
			if errors.Is(err, errDeliveryOutcomeUnknown) {
				t.Fatal("upload failure reported as an unknown send outcome")
			}
			if attempts != tc.wantAttempts {
				t.Fatalf("upload attempts = %d, want %d", attempts, tc.wantAttempts)
			}
		})
	}
}
