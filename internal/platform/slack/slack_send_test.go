package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/keakon/chord-gateway/internal/im"
)

type slackPostFunc func(context.Context, string, ...slackapi.MsgOption) (string, string, error)

func (f slackPostFunc) PostMessageContext(ctx context.Context, channel string, options ...slackapi.MsgOption) (string, string, error) {
	return f(ctx, channel, options...)
}

func testSlackSender(t *testing.T, client slackPostClient) *Sender {
	t.Helper()
	sender, err := NewSender(client)
	if err != nil {
		t.Fatal(err)
	}
	sender.limits.channelInterval = time.Millisecond
	sender.limits.accountInterval = time.Millisecond
	return sender
}

func testSlackAPI(t *testing.T, handler http.HandlerFunc) *slackapi.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return slackapi.New("sample-bot-token", slackapi.OptionAPIURL(server.URL+"/"), slackapi.OptionHTTPClient(NewHTTPClient(server.Client(), nil)), slackapi.OptionDebug(false))
}

func TestSlackSendSafeTextAndThread(t *testing.T) {
	var forms []url.Values
	client := testSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		forms = append(forms, r.PostForm)
		_, _ = fmt.Fprint(w, `{"ok":true,"channel":"CCHAN1","ts":"1234567890.000001"}`)
	})
	sender := testSlackSender(t, client)
	target, err := Target("CCHAN1", "1234567890.000009")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Repeat("<@UUSER1> <!channel> <!here> & 😀 https://example.invalid\n", 130)
	if err := sender.Send(context.Background(), nil, target, source); err != nil {
		t.Fatal(err)
	}
	var combined strings.Builder
	for _, form := range forms {
		if im.UTF16Units(form.Get("text")) > slackTextBudget {
			t.Fatal("rendered segment exceeds UTF-16 budget")
		}
		for key, expected := range map[string]string{"channel": "CCHAN1", "thread_ts": "1234567890.000009", "mrkdwn": "false", "parse": "none", "link_names": "false", "unfurl_links": "false", "unfurl_media": "false"} {
			if form.Get(key) != expected {
				t.Errorf("%s = %q, want %q", key, form.Get(key), expected)
			}
		}
		if form.Get("reply_broadcast") != "" || form.Get("blocks") != "" || strings.ContainsAny(form.Get("text"), "<>") {
			t.Fatal("unsafe message control fields")
		}
		decoded := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(form.Get("text"))
		combined.WriteString(decoded)
	}
	if len(forms) < 2 || combined.String() != source {
		t.Fatal("segmentation lost text or split escaped entities")
	}
	if im.UTF16Units("a😀") != 3 {
		t.Fatal("supplementary rune is not counted as two UTF-16 units")
	}
}

func TestSlackSendRequiresExplicitReceipt(t *testing.T) {
	for _, body := range []string{
		`{"ok":false,"channel":"CCHAN1","ts":"1234567890.000001"}`,
		`{"channel":"CCHAN1","ts":"1234567890.000001"}`,
		`{"ok":true,"channel":"CCHAN1"}`,
		`{"ok":true,"channel":"COTHER1","ts":"1234567890.000001"}`,
		`{"ok":true,"channel":"CCHAN1","ts":"not-a-timestamp"}`,
		`{"ok":"true","channel":"CCHAN1","ts":"1234567890.000001"}`,
		`not-json`,
	} {
		t.Run(body, func(t *testing.T) {
			var calls atomic.Int32
			client := testSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = fmt.Fprint(w, body) })
			err := testSlackSender(t, client).Send(context.Background(), nil, "CCHAN1", "sample")
			if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || calls.Load() != 1 {
				t.Fatalf("error=%v, calls=%d", err, calls.Load())
			}
		})
	}
}

func TestSlackTextSegmentationPreservesEntityBoundaries(t *testing.T) {
	sender := testSlackSender(t, slackPostFunc(func(context.Context, string, ...slackapi.MsgOption) (string, string, error) {
		t.Fatal("segmentation must not invoke transport")
		return "", "", nil
	}))
	for _, source := range []string{strings.Repeat("<&>😀", 900), strings.Repeat("&amp;", 900)} {
		parts, err := sender.text.Segments(source)
		if err != nil {
			t.Fatal(err)
		}
		var decoded strings.Builder
		for _, part := range parts {
			if im.UTF16Units(part) > slackTextBudget {
				t.Fatal("escaped segment exceeded budget")
			}
			decoded.WriteString(strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(part))
		}
		if decoded.String() != source {
			t.Fatal("an entity was split or double-decoded")
		}
	}
}

func TestSlackHTTPClientChecksIdentityAPIReceipt(t *testing.T) {
	client := testSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":false,"team_id":"TTEAM1","user_id":"UUSER1","bot_id":"BBOT1"}`)
	})
	if _, err := client.AuthTestContext(context.Background()); err == nil {
		t.Fatal("identity fields were accepted without explicit ok:true")
	}
}

func TestSlackSendPartialFailureNeverReplays(t *testing.T) {
	var calls atomic.Int32
	client := testSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `{"ok":true,"channel":"CCHAN1","ts":"1234567890.000001"}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, "sensitive-response-body")
	})
	sender := testSlackSender(t, client)
	err := sender.Send(context.Background(), nil, "CCHAN1", strings.Repeat("x", 8000))
	if last := sender.text.TextSendTargetSnapshot("CCHAN1"); last.LastFailureCategory != "unknown_send" || last.LastAcceptedSegments != 1 {
		t.Fatalf("status=%+v", last)
	}
	var partial *im.IMTextSendError
	if !errors.As(err, &partial) || partial.AcceptedSegments != 1 || partial.Category != "unknown_send" || !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || calls.Load() != 2 {
		t.Fatalf("error=%v, partial=%+v, calls=%d", err, partial, calls.Load())
	}
	if strings.Contains(partial.Cause.Error(), "sensitive") {
		t.Fatal("response body exposed in sanitized error")
	}
}

func TestSlackSendClassifiesRejections(t *testing.T) {
	// A definite rejection must carry its bounded category; collapsing every
	// failure into delivery_failed hides which platform decision happened.
	for code, category := range map[string]string{
		"channel_not_found": "invalid_target",
		"not_in_channel":    "forbidden",
		"is_archived":       "forbidden",
		"missing_scope":     "forbidden",
		"invalid_auth":      "credentials",
		"token_revoked":     "credentials",
		"not_authed":        "credentials",
		"account_inactive":  "credentials",
	} {
		t.Run(code, func(t *testing.T) {
			client := testSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, code) })
			err := testSlackSender(t, client).Send(context.Background(), nil, "CCHAN1", "sample")
			var sendErr *im.IMTextSendError
			if !errors.As(err, &sendErr) || errors.Is(err, im.ErrDeliveryOutcomeUnknown) || !strings.Contains(sendErr.Cause.Error(), code) {
				t.Fatalf("error=%v", err)
			}
			if sendErr.Category != category {
				t.Fatalf("category=%q, want %q", sendErr.Category, category)
			}
		})
	}
	var calls atomic.Int32
	client := testSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	})
	err := testSlackSender(t, client).Send(context.Background(), nil, "CCHAN1", "sample")
	var sendErr *im.IMTextSendError
	if !errors.As(err, &sendErr) || sendErr.Category != "platform_rejected" || calls.Load() != 1 {
		t.Fatalf("HTTP rejection category: error=%v calls=%d", err, calls.Load())
	}
}

func TestSlackSendInternalAPIFailureRemainsUnknown(t *testing.T) {
	for _, code := range []string{"internal_error", "fatal_error"} {
		t.Run(code, func(t *testing.T) {
			var calls atomic.Int32
			client := testSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, code)
			})
			err := testSlackSender(t, client).Send(context.Background(), nil, "CCHAN1", "sample")
			if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || calls.Load() != 1 {
				t.Fatalf("potentially accepted operation was retried or called a definite rejection: error=%v, calls=%d", err, calls.Load())
			}
		})
	}
}

func TestSlackSendDisallowsRedirectAndUserTargets(t *testing.T) {
	var calls atomic.Int32
	client := testSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/replayed", http.StatusTemporaryRedirect)
	})
	sender := testSlackSender(t, client)
	if err := sender.Send(context.Background(), nil, "UUSER1", "sample"); err == nil || calls.Load() != 0 {
		t.Fatal("user target must not initiate a DM")
	}
	if err := sender.Send(context.Background(), nil, "CCHAN1", "sample"); !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || calls.Load() != 1 {
		t.Fatalf("redirect followed: error=%v, calls=%d", err, calls.Load())
	}
}

func TestSlackSDKRateLimitHasNoHiddenRetry(t *testing.T) {
	var calls atomic.Int32
	client := testSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	sender := testSlackSender(t, client)
	err := sender.sendSegment(context.Background(), "CCHAN1", "sample")
	var limited *im.IMRateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != time.Second || calls.Load() != 1 {
		t.Fatalf("error=%v, calls=%d", err, calls.Load())
	}
}

func TestSlackSendHonorsPublishedMethodCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		httpClient := NewHTTPClient(&http.Client{Transport: slackRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"60"}}, Body: http.NoBody, Request: r}, nil
		})}, nil)
		sender := testSlackSender(t, slackapi.New("sample-bot-token", slackapi.OptionHTTPClient(httpClient)))
		first, _ := Target("CCHAN1", "1234567890.000001")
		second, _ := Target("CCHAN1", "1234567890.000002")
		var limited *im.IMRateLimitError
		if err := sender.sendSegment(t.Context(), first, "first"); !errors.As(err, &limited) {
			t.Fatal(err)
		}
		start := time.Now()
		if err := sender.sendSegment(t.Context(), second, "second"); !errors.As(err, &limited) {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != time.Minute || calls.Load() != 2 {
			t.Fatalf("published method cooldown not honored: calls=%d elapsed=%s", calls.Load(), elapsed)
		}
	})
}

func TestSlackSendCancellationInterruptsLimitsAndHTTP(t *testing.T) {
	t.Run("pacing wait", func(t *testing.T) {
		var calls atomic.Int32
		sender := testSlackSender(t, slackPostFunc(func(context.Context, string, ...slackapi.MsgOption) (string, string, error) {
			calls.Add(1)
			return "", "", nil
		}))
		sender.limits.accountNext = time.Now().Add(time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		started := time.Now()
		if err := sender.Send(ctx, nil, "CCHAN1", "sample"); !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 || time.Since(started) > time.Second {
			t.Fatalf("cancelled wait: error=%v, calls=%d", err, calls.Load())
		}
	})
	t.Run("lifecycle HTTP", func(t *testing.T) {
		entered := make(chan struct{})
		sender := testSlackSender(t, slackPostFunc(func(ctx context.Context, _ string, _ ...slackapi.MsgOption) (string, string, error) {
			close(entered)
			<-ctx.Done()
			return "", "", ctx.Err()
		}))
		lifecycle, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- sender.Send(context.Background(), lifecycle, "CCHAN1", "sample") }()
		<-entered
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("lifecycle cancellation did not interrupt HTTP")
		}
	})
}

func TestSlackSendSameTargetOutputDoesNotInterleave(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	client := slackPostFunc(func(_ context.Context, channel string, _ ...slackapi.MsgOption) (string, string, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return channel, "1234567890.000001", nil
	})
	sender := testSlackSender(t, client)
	first := make(chan error, 1)
	go func() { first <- sender.Send(context.Background(), nil, "CCHAN1", strings.Repeat("x", 7000)) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := sender.Send(ctx, nil, "CCHAN1", "second"); !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("target ordering was bypassed: %v", err)
	}
	close(release)
	if err := <-first; err != nil || calls.Load() != 2 {
		t.Fatalf("first output: error=%v, calls=%d", err, calls.Load())
	}
}
