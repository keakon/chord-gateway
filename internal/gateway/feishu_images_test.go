package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	feishu "github.com/keakon/chord-gateway/internal/platform/feishu"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

type imageRoundTripper func(*http.Request) (*http.Response, error)

func (f imageRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFeishuImageUploadOutlivesJSONTimeout(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{})

	a.SetTokenForTest("sample-token", time.Now().Add(time.Hour))
	uploads, sends := 0, 0
	a.SetHTTPClientForTest(&http.Client{
		Timeout: 10 * time.Millisecond,
		Transport: imageRoundTripper(func(req *http.Request) (*http.Response, error) {
			defer req.Body.Close()
			_, _ = io.Copy(io.Discard, req.Body)
			body := `{"code":0,"data":{"message_id":"sample-message"}}`
			if req.URL.Path == "/open-apis/im/v1/images" {
				uploads++
				deadline, ok := req.Context().Deadline()
				if !ok || time.Until(deadline) <= time.Minute || time.Until(deadline) > feishu.MediaUploadTimeoutForTest {
					t.Error("upload does not have its independent media deadline")
				}
				timer := time.NewTimer(50 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-req.Context().Done():
					return nil, req.Context().Err()
				case <-timer.C:
				}
				body = `{"code":0,"data":{"image_key":"sample-image"}}`
			} else {
				sends++
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}),
	})
	if err := a.SendImage("sample-chat", imageForUpload(t)); err != nil {
		t.Fatalf("media upload inherited the short JSON timeout: %v", err)
	}
	if uploads != 1 || sends != 1 {
		t.Fatalf("uploads=%d sends=%d, want one of each", uploads, sends)
	}
}

func TestFeishuDisconnectCancelsImageUpload(t *testing.T) {
	for _, phase := range []string{"waiting", "token", "uploading", "sending", "uploaded"} {
		t.Run(phase, func(t *testing.T) {
			a := testFeishuAdapter(t, &config.FeishuConfig{})
			a.SetTokenForTest("sample-token", time.Now().Add(time.Hour))
			connected := make(chan struct{})
			a.SetLongConnForTest(func(ctx context.Context) error {
				close(connected)
				<-ctx.Done()
				return ctx.Err()
			})
			connectDone := make(chan error, 1)
			go func() { connectDone <- a.Connect() }()
			defer a.Disconnect()
			select {
			case <-connected:
			case <-time.After(2 * time.Second):
				t.Fatal("adapter did not connect")
			}
			started := make(chan struct{})
			if phase == "token" {
				a.ClearTokenForTest()
			}
			a.SetHTTPClientForTest(&http.Client{Transport: imageRoundTripper(func(req *http.Request) (*http.Response, error) {
				defer req.Body.Close()
				if (phase == "sending" || phase == "uploaded") && req.URL.Path == "/open-apis/im/v1/images" {
					if phase == "uploaded" {
						a.Disconnect()
						close(started)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"image_key":"sample-image"}}`))}, nil
				}
				allowed := phase == "uploading" && req.URL.Path == "/open-apis/im/v1/images" ||
					phase == "token" && req.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" ||
					phase == "sending" && req.URL.Path == "/open-apis/im/v1/messages"
				if !allowed {
					t.Error("cancelled image unexpectedly reached the platform")
					return nil, errors.New("unexpected request")
				}
				close(started)
				<-req.Context().Done()
				return nil, req.Context().Err()
			})})
			if phase == "waiting" {
				release, err := im.AcquireImageUpload(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			img := imageForUpload(t)
			sendDone := make(chan error, 1)
			go func() {
				if phase == "waiting" {
					close(started)
				}
				sendDone <- a.SendImage("sample-chat", img)
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("image upload did not start")
			}
			a.Disconnect()
			select {
			case err := <-sendDone:
				if !errors.Is(err, context.Canceled) || errors.Is(err, im.ErrDeliveryOutcomeUnknown) != (phase == "sending") {
					t.Fatalf("cancelled upload = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("disconnect did not cancel image upload")
			}
			select {
			case err := <-connectDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("adapter did not disconnect")
			}
		})
	}
}
