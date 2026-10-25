package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"

	discord "github.com/keakon/chord-gateway/internal/platform/discord"
)

func discordMetricsAdapter(t *testing.T, send func(context.Context, string, string) error) *discord.DiscordAdapter {
	adapter, err := discord.NewDiscordAdapter(nil, config.IMAdapterConfig{Discord: &config.DiscordConfig{BotToken: "example-token", ApplicationID: "123456789012345678"}}, testPaths(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := discord.NewDiscordTextSender(send)
	if err != nil {
		t.Fatal(err)
	}
	adapter.SetSenderForTest(sender)
	t.Cleanup(adapter.Disconnect)
	return adapter
}

func assertDiscordPartialMetrics(t *testing.T, adapter *discord.DiscordAdapter, target string, err error, category string, failures uint64) {
	t.Helper()
	var partial *im.IMTextSendError
	if !errors.As(err, &partial) || partial.AcceptedSegments != 1 || partial.Category != category {
		t.Fatalf("send result and expected classification differ: %v", err)
	}
	snapshot := adapter.TextSendMetrics()
	if snapshot.Platform != "discord" || snapshot.Failures != failures || snapshot.PartialFailures != failures {
		t.Fatalf("failure counted more than once: %+v", snapshot)
	}
	diagnostic := adapter.TextSendTargetSnapshot(target)
	if diagnostic.LastFailureCategory != partial.Category || diagnostic.LastAcceptedSegments != partial.AcceptedSegments || diagnostic.LastFailureAt.IsZero() {
		t.Fatalf("metric and send error disagree: err=%+v diagnostic=%+v", partial, diagnostic)
	}
}

func TestDiscordMetricsWrittenPostCancellationCountsUnknownOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			t.Error(err)
			return
		}
		if requests.Add(1) == 1 {
			fmt.Fprint(w, `{"id":"999","channel_id":"500"}`)
			return
		}
		// Receiving the second POST proves it was written, but cancellation
		// leaves acceptance of that segment unconfirmed.
		cancel()
		<-request.Context().Done()
	}))
	defer server.Close()
	adapter := discordRESTTestAdapter(t, server)
	err := adapter.SendTextContext(ctx, "500", strings.Repeat("a", 5700))
	if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || !errors.Is(err, context.Canceled) || requests.Load() != 2 {
		t.Fatalf("written POST lost unknown evidence or continued: requests=%d error=%v", requests.Load(), err)
	}
	assertDiscordPartialMetrics(t, adapter, "500", err, "unknown_send", 1)
	var lines []string
	logIMMetrics(nil, &MultiAdapter{adapters: []im.IMAdapter{adapter}}, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	if len(lines) != 1 || !strings.Contains(lines[0], "platform=discord") || !strings.Contains(lines[0], "failures=1 partial_failures=1") {
		t.Fatal("shared reporter did not receive Discord diagnostics", lines)
	}
}

func TestDiscordMetricsRateWaitAndPartialUnknownCountOnce(t *testing.T) {
	calls := 0
	adapter := discordMetricsAdapter(t, func(ctx context.Context, _, _ string) error {
		calls++
		switch calls {
		case 1:
			return nil
		case 2:
			return &im.IMRateLimitError{RetryAfter: time.Millisecond}
		default:
			ctx.Value(discord.SendOutcomeKey{}).(*discord.SendOutcome).Unknown.Store(true)
			return im.ErrDeliveryOutcomeUnknown
		}
	})
	err := adapter.SendTextContext(t.Context(), "500", strings.Repeat("a", 5700))
	assertDiscordPartialMetrics(t, adapter, "500", err, "unknown_send", 1)
	snapshot := adapter.TextSendMetrics()
	if calls != 3 || snapshot.RateLimitWaits != 1 || snapshot.RateLimitWaitTotal <= 0 || snapshot.RateLimitWaitMax <= 0 {
		t.Fatal("rate retry or unknown failure was counted repeatedly", calls, snapshot)
	}
}

func TestDiscordMetricsConcurrentTargetsAndLaterFailureKeepTheirCategories(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	adapter := discordMetricsAdapter(t, func(ctx context.Context, target, _ string) error {
		mu.Lock()
		calls[target]++
		count := calls[target]
		mu.Unlock()
		if count%2 == 1 {
			return nil
		}
		if target == "501" && count == 2 {
			ctx.Value(discord.SendOutcomeKey{}).(*discord.SendOutcome).Unknown.Store(true)
			return im.ErrDeliveryOutcomeUnknown
		}
		return errors.New("discord permission_denied")
	})
	type result struct {
		target string
		err    error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, target := range []string{"501", "502"} {
		workers.Go(func() {
			results <- result{target, adapter.SendTextContext(t.Context(), target, strings.Repeat("a", 3800))}
		})
	}
	workers.Wait()
	close(results)
	for result := range results {
		category := "delivery_failed"
		if result.target == "501" {
			category = "unknown_send"
		}
		assertDiscordPartialMetrics(t, adapter, result.target, result.err, category, 2)
	}
	// A later failure in the same target must retain its own definite category.
	err := adapter.SendTextContext(t.Context(), "501", strings.Repeat("a", 3800))
	assertDiscordPartialMetrics(t, adapter, "501", err, "delivery_failed", 3)
	if errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
		t.Fatal("earlier unknown evidence leaked into a later output")
	}
}
