package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func TestFeishuExpiredTokenRecoveryStatusAndLimit(t *testing.T) {
	for _, phase := range []string{"upload", "send"} {
		for _, tc := range []struct {
			status     int
			persistent bool
		}{
			{http.StatusOK, false},
			{http.StatusBadRequest, false},
			{http.StatusUnauthorized, false},
			{http.StatusBadRequest, true},
			{http.StatusRequestTimeout, false},
			{http.StatusTooManyRequests, false},
			{http.StatusBadGateway, false},
			{http.StatusCreated, false},
			{http.StatusTemporaryRedirect, false},
		} {
			t.Run(fmt.Sprintf("%s/%d/persistent=%t", phase, tc.status, tc.persistent), func(t *testing.T) {
				a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "sample-app", AppSecret: "sample-secret"})

				a.SetTokenForTest("old-token", time.Now().Add(time.Hour))
				var tokens, targetRequests atomic.Int32
				target := "/open-apis/im/v1/messages"
				if phase == "upload" {
					target = "/open-apis/im/v1/images"
				}
				a.SetHTTPClientForTest(&http.Client{Transport: imageRoundTripper(func(req *http.Request) (*http.Response, error) {
					defer req.Body.Close()
					_, _ = io.Copy(io.Discard, req.Body)
					status, body := http.StatusOK, `{"code":0,"data":{"message_id":"sample-message","image_key":"sample-image"}}`
					if req.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
						tokens.Add(1)
						body = `{"code":0,"tenant_access_token":"fresh-token","expire":3600}`
					} else if req.URL.Path == target {
						attempt := targetRequests.Add(1)
						if attempt == 1 || tc.persistent {
							status, body = tc.status, `{"code":99991663}`
						}
						if attempt > 1 && req.Header.Get("Authorization") != "Bearer fresh-token" {
							t.Error("retry did not use refreshed token")
						}
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})})
				err := a.SendImage("sample-chat", imageForUpload(t))
				recoverable := tc.status == http.StatusOK || tc.status == http.StatusBadRequest || tc.status == http.StatusUnauthorized
				wantTokens, wantRequests := int32(0), int32(1)
				if recoverable {
					wantTokens, wantRequests = 1, 2
				}
				if (err == nil) != (recoverable && !tc.persistent) {
					t.Fatalf("recovery error = %v", err)
				}
				if tokens.Load() != wantTokens || targetRequests.Load() != wantRequests {
					t.Fatalf("tokens=%d requests=%d, want %d/%d", tokens.Load(), targetRequests.Load(), wantTokens, wantRequests)
				}
				if err != nil && phase == "send" && errors.Is(err, im.ErrDeliveryOutcomeUnknown) != !recoverable {
					t.Fatalf("wrong delivery outcome: %v", err)
				}
			})
		}
	}
}

func TestFeishuTokenCallerCancellationKeepsSharedRefresh(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "sample-app", AppSecret: "sample-secret"})

	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	a.SetHTTPClientForTest(&http.Client{Transport: imageRoundTripper(func(req *http.Request) (*http.Response, error) {
		defer req.Body.Close()
		if requests.Add(1) != 1 {
			t.Error("shared refresh was replayed")
		} else {
			close(started)
		}
		select {
		case <-release:
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"tenant_access_token":"shared-token","expire":3600}`))}, nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	})})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	defer close(release)
	done := make(chan error, 1)
	go func() {
		_, err := a.GetAccessTokenForTest(ctx)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not start")
	}
	refresh := a.TokenRefreshForTest()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled token caller = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("token caller did not cancel")
	}
	select {
	case <-refresh.TokenRefreshDoneForTest():
		t.Fatal("caller cancelled the shared refresh")
	default:
	}
	// A caller joining the existing refresh can stop waiting independently too.
	waitCtx, cancelWait := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancelWait()
	if _, err := a.GetAccessTokenForTest(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled shared-refresh waiter = %v", err)
	}
	release <- struct{}{}
	token, err := a.GetAccessTokenForTest(t.Context())
	if err != nil || token != "shared-token" || requests.Load() != 1 {
		t.Fatalf("shared refresh: token=%q requests=%d err=%v", token, requests.Load(), err)
	}
}
