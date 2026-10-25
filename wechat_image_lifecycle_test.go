package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWechatDisconnectCancelsEachImageStage(t *testing.T) {
	for _, stage := range []string{"/ilink/bot/getuploadurl", "/cdn", "/ilink/bot/sendmessage"} {
		t.Run(stage, func(t *testing.T) {
			a := newTestWechatAdapter(t)
			a.SetBaseURLForTest("https://example.invalid")
			ctx, cancel := context.WithCancel(t.Context())
			a.SetConnectionForTest(ctx, cancel)
			defer cancel()
			a.CachePairedContextForTest("bot", "owner", "reply-context", true)
			entered := make(chan struct{})
			a.SetTransportForTest(imageRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == stage {
					close(entered)
					<-request.Context().Done()
					return nil, request.Context().Err()
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"X-Encrypted-Param": {"download-param"}},
					Body:       io.NopCloser(strings.NewReader(`{"upload_full_url":"https://example.invalid/cdn","ret":0}`)),
				}, nil
			}))
			image := imageForUpload(t)
			done := make(chan error, 1)
			go func() { done <- a.SendImageContext(t.Context(), "owner", image) }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("image did not reach stage: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("image did not reach stage")
			}
			a.Disconnect()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("image cancellation = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("image survived disconnect")
			}
		})
	}
}
