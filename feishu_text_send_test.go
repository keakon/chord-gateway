package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func textSendFeishuAdapter(t *testing.T, transport func(*http.Request) (*http.Response, error)) *FeishuAdapter {
	t.Helper()
	lifecycle, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return &FeishuAdapter{
		imCfg:       config.IMAdapterConfig{Feishu: &config.FeishuConfig{AppID: "text-budget-account"}},
		httpClient:  &http.Client{Transport: imageRoundTripper(transport)},
		accessToken: "cached-text-token", tokenExpireAt: time.Now().Add(time.Hour),
		ctx: lifecycle, cancel: cancel,
	}
}

func textSendResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func textSendRequest(t *testing.T, req *http.Request) string {
	t.Helper()
	defer req.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Error(err)
	}
	if body["msg_type"] != "text" {
		t.Errorf("unexpected message type %q", body["msg_type"])
	}
	var content map[string]string
	if err := json.Unmarshal([]byte(body["content"]), &content); err != nil {
		t.Error(err)
	}
	return content["text"]
}

func TestFeishuTextSharedBudgetTruncationAndDeadline(t *testing.T) {
	for _, text := range []string{strings.Repeat("🙂", 20000), strings.Repeat(strings.Repeat("x", 2000)+"\n", 40)} {
		var parts []string
		a := textSendFeishuAdapter(t, func(req *http.Request) (*http.Response, error) {
			deadline, ok := req.Context().Deadline()
			if !ok || time.Until(deadline) > im.IMTextSendTimeout {
				t.Errorf("text request has no shared five-minute deadline")
			}
			parts = append(parts, textSendRequest(t, req))
			return textSendResponse(http.StatusOK, `{"code":0,"data":{"message_id":"accepted"}}`), nil
		})
		if err := a.SendTextContext(t.Context(), "chat", text); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(parts, "")
		if len(parts) > im.MaxTextSegments || len(joined) > im.MaxTextBytes || !strings.Contains(parts[len(parts)-1], "Output truncated") {
			t.Fatalf("unbounded or silent truncation: %d segments, %d bytes", len(parts), len(joined))
		}
		for _, part := range parts {
			if !utf8.ValidString(part) || utf8.RuneCountInString(part) > feishuMaxTextLen {
				t.Fatal("segment split damaged Unicode or exceeded existing Feishu budget")
			}
		}
	}
}

func TestFeishuTextPartialUnknownStopsWithoutReplay(t *testing.T) {
	for _, response := range []struct {
		status int
		body   string
	}{
		{http.StatusBadGateway, "upstream failed"},
		{http.StatusOK, `{"code":0,"data":{}}`},
	} {
		var calls atomic.Int32
		a := textSendFeishuAdapter(t, func(req *http.Request) (*http.Response, error) {
			textSendRequest(t, req)
			if calls.Add(1) == 1 {
				return textSendResponse(http.StatusOK, `{"code":0,"data":{"message_id":"accepted"}}`), nil
			}
			return textSendResponse(response.status, response.body), nil
		})
		err := a.SendTextContext(t.Context(), "chat", strings.Repeat("x", feishuMaxTextLen*3))
		var partial *im.IMTextSendError
		if !errors.As(err, &partial) || partial.AcceptedSegments != 1 || partial.Category != "unknown_send" || !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || calls.Load() != 2 {
			t.Fatalf("unknown output replayed or lost confirmed count: %v calls=%d", err, calls.Load())
		}
		last := a.TextSendTargetSnapshot("chat")
		if last.LastAcceptedSegments != 1 || last.LastFailureCategory != "unknown_send" || a.TextSendMetrics().PartialFailures != 1 {
			t.Fatal("partial delivery is missing from shared status diagnostics", last)
		}
	}
}

func TestFeishuTextExplicit429RetriesOnlyRejectedSegment(t *testing.T) {
	var sent []string
	a := textSendFeishuAdapter(t, func(req *http.Request) (*http.Response, error) {
		sent = append(sent, textSendRequest(t, req))
		if len(sent) == 2 {
			response := textSendResponse(http.StatusTooManyRequests, `{"code":230020}`)
			response.Header.Set("Retry-After", "1")
			return response, nil
		}
		return textSendResponse(http.StatusOK, `{"code":0,"data":{"message_id":"accepted"}}`), nil
	})
	first, second := strings.Repeat("a", feishuMaxTextLen), strings.Repeat("b", feishuMaxTextLen)
	if err := a.SendTextContext(t.Context(), "chat", first+second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sent, []string{first, second, second}) || a.TextSendMetrics().RateLimitWaits != 1 {
		t.Fatal("rate limit replayed an already accepted segment", len(sent))
	}
}

func TestFeishuTextRateLimitCancellationHonorsCallerAndDisconnect(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprint(disconnect), func(t *testing.T) {
			var calls atomic.Int32
			entered := make(chan struct{})
			a := textSendFeishuAdapter(t, func(req *http.Request) (*http.Response, error) {
				textSendRequest(t, req)
				calls.Add(1)
				close(entered)
				response := textSendResponse(http.StatusTooManyRequests, "rate limit")
				response.Header.Set("Retry-After", "300")
				return response, nil
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- a.SendTextContext(ctx, "chat", "body") }()
			waitFeishuLifecycle(t, entered)
			if disconnect {
				a.Disconnect()
			} else {
				cancel()
			}
			if err := waitFeishuLifecycle(t, done); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
				t.Fatalf("rate-limit wait ignored cancellation: %v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestFeishuTextUsesGlobalSlotsAcrossAdapterInstances(t *testing.T) {
	var active, peak atomic.Int32
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	transport := func(req *http.Request) (*http.Response, error) {
		defer req.Body.Close()
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		entered <- struct{}{}
		<-release
		return textSendResponse(http.StatusOK, `{"code":0,"data":{"message_id":"accepted"}}`), nil
	}
	a, b := textSendFeishuAdapter(t, transport), textSendFeishuAdapter(t, transport)
	done := make(chan error, 16)
	for i := range 16 {
		go func() {
			adapter := a
			if i%2 != 0 {
				adapter = b
			}
			done <- adapter.SendTextContext(t.Context(), fmt.Sprint(i), "body")
		}()
	}
	for range 8 {
		waitFeishuLifecycle(t, entered)
	}
	once.Do(func() { close(release) })
	for range 16 {
		if err := waitFeishuLifecycle(t, done); err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 8 {
		t.Fatalf("adapter multiplied global send budget: %d", peak.Load())
	}
}

func TestFeishuTextRetryAfterParsing(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", time.Second}, {"invalid", time.Second}, {"-1", time.Second},
		{"2", 2 * time.Second}, {"9223372036854775807", im.IMTextSendTimeout},
		{now.Add(3 * time.Second).UTC().Format(http.TimeFormat), 3 * time.Second},
	} {
		if got := feishuTextRetryAfter(tc.value, now); got != tc.want {
			t.Fatalf("%q: %v, want %v", tc.value, got, tc.want)
		}
	}
}
