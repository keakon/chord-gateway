package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/slack-go/slack"
)

type slackRoundTripFunc func(*http.Request) (*http.Response, error)

func (f slackRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSlackAPIMethodCooldownSurvivesNewCallers(t *testing.T) {
	for _, method := range []string{"conversations.info", "auth.test", "bots.info", "apps.connections.open", "chat.postMessage"} {
		t.Run(method, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client := newSlackHTTPClient(&http.Client{Transport: slackRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				})})
				call := func() {
					// Recreate the SDK caller while retaining installation-wide state.
					sdk := slack.New("sample-token", slack.OptionAppLevelToken("sample-app-token"), slack.OptionHTTPClient(client))
					var err error
					switch method {
					case "conversations.info":
						_, err = newSlackInbound(sdk, slackIdentity{}, nil).conversation(t.Context(), "CCHAN1", false)
					case "auth.test":
						_, err = sdk.AuthTestContext(t.Context())
					case "bots.info":
						_, err = sdk.GetBotInfoContext(t.Context(), slack.GetBotInfoParameters{Bot: "BBOT1"})
					case "apps.connections.open":
						_, _, err = sdk.StartSocketModeContext(t.Context())
						err = sanitizeSlackConnectionError(err)
					case "chat.postMessage":
						_, _, err = sdk.PostMessageContext(t.Context(), "CCHAN1", slack.MsgOptionText("output", false))
					}
					var limited *slack.RateLimitedError
					if !errors.As(err, &limited) || limited.RetryAfter != time.Minute {
						t.Fatal(err)
					}
				}
				call()
				start := time.Now()
				call()
				if calls != 2 || time.Since(start) != time.Minute {
					t.Fatalf("calls=%d elapsed=%s", calls, time.Since(start))
				}
			})
		})
	}
}

func TestSlackMethodCooldownIsolationCancellationAndExtension(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limits := &slackMethodLimits{blocked: make(map[string]time.Time)}
		limits.block("conversations.info", time.Minute)
		if err := limits.wait(t.Context(), "auth.test"); err != nil {
			t.Fatal(err)
		}
		other := &slackMethodLimits{blocked: make(map[string]time.Time)}
		if err := other.wait(t.Context(), "conversations.info"); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := limits.wait(ctx, "conversations.info"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if time.Since(start) != time.Second {
			t.Fatal("unrelated methods or installations blocked")
		}
		go func() {
			time.Sleep(10 * time.Second)
			limits.block("conversations.info", time.Minute)
			limits.block("conversations.info", time.Second)
		}()
		if err := limits.wait(t.Context(), "conversations.info"); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 71*time.Second {
			t.Fatalf("deadline lost: %s", time.Since(start))
		}
	})
}

func TestSlackCooldownCancellationDoesNotSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := newSlackHTTPClient(&http.Client{Transport: slackRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			t.Error("request reached network during cooldown")
			return nil, errors.New("unexpected send")
		})})
		client.limits.block("chat.postMessage", time.Minute)
		sdk := slack.New("sample-token", slack.OptionHTTPClient(client))
		sender := testSlackSender(t, sdk)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := sender.Send(ctx, nil, "CCHAN1", "output")
		var partial *IMTextSendError
		if !errors.As(err, &partial) || partial.Category != "cancelled" || errors.Is(err, errDeliveryOutcomeUnknown) || calls != 0 {
			t.Fatalf("send=%v calls=%d", err, calls)
		}
	})
}

func TestSlackConnectionPreservesRateLimit(t *testing.T) {
	limited := &slack.RateLimitedError{RetryAfter: time.Minute}
	err := sanitizeSlackConnectionError(limited)
	var got *slack.RateLimitedError
	if !errors.As(err, &got) || got.RetryAfter != time.Minute || slackConnectionFatal(err) || slackConnectionCategory(err) != "rate_limited" {
		t.Fatal(err)
	}
}
