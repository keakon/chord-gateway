package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"time"

	"github.com/keakon/golog/log"
)

const feishuImageUploadLimit = 10_000_000

// A multi-megabyte upload cannot share the short budget of a JSON API call, so
// media uploads get their own deadline and a client without the adapter-wide
// timeout.
const feishuMediaUploadTimeout = 2 * time.Minute

// mediaClient returns a client without the adapter-wide request timeout. The
// caller bounds the request with a context deadline instead, because a shorter
// http.Client.Timeout cannot be extended by a longer context.
func (a *FeishuAdapter) mediaClient() *http.Client {
	if a.httpClient == nil {
		return &http.Client{Transport: proxiedTransport(a.proxyFunc)}
	}
	client := *a.httpClient
	client.Timeout = 0
	return &client
}

func imageExtension(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func (a *FeishuAdapter) SendImage(chatID string, image *ReceivedImage) error {
	return a.SendImageContext(a.connectionContext(), chatID, image)
}
func (a *FeishuAdapter) SendImageContext(parent context.Context, chatID string, image *ReceivedImage) error {
	// Root the deadline at the adapter's connection context so a shutdown
	// cancels an upload already in flight; fall back to Background when the
	// adapter is used before Connect has run.
	ctx, cancel := context.WithTimeout(parent, feishuMediaUploadTimeout)
	defer cancel()
	release, err := acquireImageUpload(ctx)
	if err != nil {
		return err
	}
	defer release()
	f, err := openReceivedImage(image)
	if err != nil {
		return err
	}
	defer f.Close()
	var source io.ReadSeeker = f
	size := image.Size
	mimeType := image.MIME
	name := "image" + imageExtension(mimeType)
	preview := size >= feishuImageUploadLimit
	if preview {
		data, err := imageUploadPreview(ctx, f, feishuImageUploadLimit)
		if err != nil {
			return err
		}
		source, size, mimeType, name = bytes.NewReader(data), int64(len(data)), "image/png", "preview.png"
	}
	key, err := a.uploadImage(ctx, source, size, name, mimeType, false)
	if err != nil {
		return err
	}
	if _, err := a.sendMessageContext(ctx, chatID, "image", map[string]string{"image_key": key}, false); err != nil {
		return fmt.Errorf("send feishu image: %w", err)
	}
	if preview {
		if _, err := a.sendMessageContext(ctx, chatID, "text", map[string]string{"text": "Image reaches or exceeds the Feishu upload limit; a reduced preview was sent. The original remains in the Chord session."}, false); err != nil {
			// The preview has a confirmed receipt; a failed note does not undo delivery.
			log.Warnf("feishu image preview note failed call=%v error=%v", image.CallID, err)
		}
	}
	return nil
}

func (a *FeishuAdapter) uploadImage(ctx context.Context, source io.ReadSeeker, size int64, name, mimeType string, retried bool) (string, error) {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("image_type", "message"); err != nil {
		return "", fmt.Errorf("encode image type: %w", err)
	}
	// The platform's reference request sets the part content type and a filename
	// with an extension; matching both is cheap and removes a detection risk.
	if _, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="image"; filename="` + name + `"`},
		"Content-Type":        {mimeType},
	}); err != nil {
		return "", fmt.Errorf("encode image upload: %w", err)
	}
	prefixSize := body.Len()
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("finish image upload: %w", err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind image upload: %w", err)
	}
	// Only form headers and the final boundary are buffered; image bytes stream
	// directly from the validated file (or the small encoded preview).
	form := body.Bytes()
	stream := io.MultiReader(bytes.NewReader(form[:prefixSize]), io.LimitReader(source, size), bytes.NewReader(form[prefixSize:]))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, feishuOpenBaseURL+"/open-apis/im/v1/images", stream)
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(form)) + size
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.mediaClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("upload feishu image: %w", err)
	}
	defer resp.Body.Close()
	if isAmbiguousDeliveryStatus(resp.StatusCode) {
		return "", fmt.Errorf("feishu image upload failed: HTTP %d", resp.StatusCode)
	}
	var result struct {
		Code *int `json:"code"`
		Data struct {
			ImageKey string `json:"image_key"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode image upload response: %w", err)
	}
	if result.Code != nil && *result.Code == 99991663 && !retried {
		a.invalidateAccessToken()
		resp.Body.Close()
		return a.uploadImage(ctx, source, size, name, mimeType, true)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("feishu image upload failed: HTTP %d", resp.StatusCode)
	}
	if result.Code == nil {
		return "", fmt.Errorf("feishu image upload response is missing code")
	}
	if *result.Code != 0 || result.Data.ImageKey == "" {
		return "", fmt.Errorf("feishu image upload rejected: HTTP %d code=%d", resp.StatusCode, *result.Code)
	}
	return result.Data.ImageKey, nil
}
