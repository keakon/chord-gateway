package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestDiscordWrittenPOSTCancellationPreservesUnknownOutcome(t *testing.T) {
	for _, cancellation := range []string{"caller", "deadline", "http_timeout"} {
		t.Run(cancellation, func(t *testing.T) {
			var requests atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if requests.Add(1) == 1 {
					fmt.Fprint(w, `{"id":"999","channel_id":"500"}`)
					return
				}
				if _, err := io.ReadAll(request.Body); err != nil {
					t.Error(err)
				}
				close(entered) // The platform received the whole POST before cancellation.
				<-release
				fmt.Fprint(w, `{"id":"1000","channel_id":"500"}`)
			}))
			defer server.Close()
			defer close(release)
			adapter := discordRESTTestAdapter(t, server)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancellation == "deadline" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 250*time.Millisecond)
				defer deadlineCancel()
			}
			if cancellation == "http_timeout" {
				adapter.rest.HTTPClient().Timeout = 100 * time.Millisecond
			}
			done := make(chan error, 1)
			go func() { done <- adapter.SendTextContext(ctx, "500", strings.Repeat("a", 5700)) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("second POST was not written")
			}
			if cancellation == "caller" {
				cancel()
			}
			select {
			case err := <-done:
				var partial *im.IMTextSendError
				if !errors.As(err, &partial) || partial.AcceptedSegments != 1 || partial.Category != "unknown_send" || !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || requests.Load() != 2 {
					t.Fatalf("written POST uncertainty lost or later output sent: requests=%d err=%v", requests.Load(), err)
				}
				if cancellation == "caller" && !errors.Is(err, context.Canceled) {
					t.Fatal("caller cancellation cause lost")
				}
				if cancellation == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("deadline cause lost")
				}
			case <-time.After(time.Second):
				t.Fatal("HTTP cancellation did not finish")
			}
		})
	}
}

func TestDiscordCancelledBeforePOSTDoesNotClaimUnknownOutcome(t *testing.T) {
	for _, scenario := range []string{"pre_cancelled", "rate_wait"} {
		t.Run(scenario, func(t *testing.T) {
			var requests atomic.Int32
			entered := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"retry_after":60}`)
				entered <- struct{}{}
			}))
			defer server.Close()
			adapter := discordRESTTestAdapter(t, server)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario == "pre_cancelled" {
				cancel()
			}
			done := make(chan error, 1)
			go func() { done <- adapter.SendTextContext(ctx, "500", "sample task") }()
			if scenario == "rate_wait" {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("429 was not received")
				}
				timer := time.NewTimer(30 * time.Millisecond)
				select {
				case <-timer.C:
				case err := <-done:
					timer.Stop()
					t.Fatal("rate wait returned early", err)
				}
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
					t.Fatal("unsent or explicitly rejected message classified as unknown", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled wait did not finish")
			}
			want := int32(0)
			if scenario == "rate_wait" {
				want = 1
			}
			if requests.Load() != want {
				t.Fatal("cancelled output made another POST", requests.Load())
			}
		})
	}
}

type discordCancelAfterReceiptREST struct {
	rest.Rest
	cancel context.CancelFunc
}

func (r discordCancelAfterReceiptREST) CreateMessage(channelID snowflake.ID, message discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error) {
	result, err := r.Rest.CreateMessage(channelID, message, opts...)
	r.cancel()
	return result, err
}

func TestDiscordExplicitReceiptClearsCancellationUncertainty(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(status)
				if status == http.StatusOK {
					fmt.Fprint(w, `{"id":"999","channel_id":"500"}`)
				} else {
					fmt.Fprint(w, `{"message":"Missing Permissions","code":50013}`)
				}
			}))
			defer server.Close()
			adapter := discordRESTTestAdapter(t, server)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			adapter.rest = discordCancelAfterReceiptREST{Rest: adapter.rest, cancel: cancel}
			err := adapter.SendTextContext(ctx, "500", strings.Repeat("a", 3800))
			var partial *im.IMTextSendError
			wantAccepted := 0
			if status == http.StatusOK {
				wantAccepted = 1
			}
			if !errors.As(err, &partial) || partial.AcceptedSegments != wantAccepted || !errors.Is(err, context.Canceled) || errors.Is(err, im.ErrDeliveryOutcomeUnknown) || requests.Load() != 1 {
				t.Fatal("explicit receipt became unknown or cancellation continued output", requests.Load(), err)
			}
		})
	}
}

func TestDiscordRetryAttemptClearsPrior429Evidence(t *testing.T) {
	var requests atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"retry_after":0.001}`)
			return
		}
		if _, err := io.ReadAll(request.Body); err != nil {
			t.Error(err)
		}
		close(entered)
		<-release
		fmt.Fprint(w, `{"id":"999","channel_id":"500"}`)
	}))
	defer server.Close()
	defer close(release)
	adapter := discordRESTTestAdapter(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- adapter.SendTextContext(ctx, "500", "sample task") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("safe 429 retry was not written")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || requests.Load() != 2 {
			t.Fatal("prior rejected attempt hid new unconfirmed POST", requests.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry cancellation did not finish")
	}
}

func TestDiscordDeletedAndLockedChannelsFailWithExplicitSafeCategory(t *testing.T) {
	for _, tc := range []struct {
		code   int
		status int
		want   string
	}{
		{160005, http.StatusBadRequest, "discord thread_locked"},
		{160005, http.StatusForbidden, "discord thread_locked"},
		{10003, http.StatusNotFound, "discord channel_unavailable"},
		{50013, http.StatusForbidden, "discord permission_denied"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if request.URL.Path != "/channels/501/messages" {
					t.Errorf("thread failure redirected to %s", request.URL.Path)
				}
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"message":"sample-private-error-body","code":%d}`, tc.code)
			}))
			defer server.Close()
			adapter := discordRESTTestAdapter(t, server)
			err := adapter.SendTextContext(t.Context(), "501", "sample task")
			var partial *im.IMTextSendError
			if !errors.As(err, &partial) || partial.Cause.Error() != tc.want || errors.Is(err, im.ErrDeliveryOutcomeUnknown) || requests.Load() != 1 {
				t.Fatal("channel failure ambiguous or destination changed", requests.Load(), err)
			}
		})
	}
}

func TestDiscordHTTPResponseWinsOverLateWriterCallbacks(t *testing.T) {
	outcome := &discordSendOutcome{}
	ctx := context.WithValue(t.Context(), discordSendOutcomeKey{}, outcome)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/channels/500/messages", nil)
	var traces []*httptrace.ClientTrace
	status := http.StatusTooManyRequests
	transport := discordRateLimitTransport{base: discordRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		traces = append(traces, httptrace.ContextClientTrace(request.Context()))
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"retry_after":0.001}`))}, nil
	})}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	traces[0].WroteRequest(httptrace.WroteRequestInfo{})
	traces[0].WroteHeaders()
	if outcome.unknown.Load() || outcome.status.Load() != http.StatusTooManyRequests {
		t.Fatal("late writer changed explicit 429 outcome")
	}
	status = http.StatusForbidden
	response, err = transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	traces[0].WroteRequest(httptrace.WroteRequestInfo{}) // The old attempt finishes during a newer connection.
	traces[1].WroteRequest(httptrace.WroteRequestInfo{}) // The new request writer also finishes after rejection.
	if outcome.unknown.Load() || outcome.status.Load() != http.StatusForbidden {
		t.Fatal("old writer overwrote newer explicit rejection")
	}
	outcome.accepted()
	traces[1].WroteRequest(httptrace.WroteRequestInfo{})
	if outcome.unknown.Load() {
		t.Fatal("late writer changed a validated receipt")
	}
}

func TestDiscordHTTPOldWriterCannotMarkUnwrittenRetryUnknown(t *testing.T) {
	outcome := &discordSendOutcome{}
	ctx := context.WithValue(t.Context(), discordSendOutcomeKey{}, outcome)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/channels/500/messages", nil)
	var first *httptrace.ClientTrace
	calls := 0
	transport := discordRateLimitTransport{base: discordRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		trace := httptrace.ContextClientTrace(request.Context())
		if calls == 1 {
			first = trace
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"retry_after":0.001}`))}, nil
		}
		first.WroteRequest(httptrace.WroteRequestInfo{})
		if outcome.unknown.Load() {
			t.Error("previous writer marked an unwritten retry unknown")
		}
		return nil, context.Canceled
	})}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = transport.RoundTrip(request)
	if !errors.Is(err, context.Canceled) || outcome.unknown.Load() {
		t.Fatal("old attempt contaminated cancelled unwritten retry", err)
	}
}
