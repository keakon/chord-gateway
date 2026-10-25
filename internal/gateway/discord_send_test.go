package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	disgo "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"

	discord "github.com/keakon/chord-gateway/internal/platform/discord"
)

func discordTestSender(t *testing.T, send func(context.Context, string, string) error) *im.IMTextSender {
	t.Helper()
	sender, err := discord.NewDiscordTextSender(send)
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

func TestDiscordTextSenderRenderedUTF16Boundary(t *testing.T) {
	for _, tc := range []struct{ name, text, first, joined string }{
		{"exact_ascii", strings.Repeat("a", 1900), strings.Repeat("a", 1900), strings.Repeat("a", 1900)},
		{"emoji_boundary", strings.Repeat("a", 1899) + "😀tail", strings.Repeat("a", 1899), strings.Repeat("a", 1899) + "😀tail"},
		{"escaped_boundary", strings.Repeat("a", 1899) + "*tail", strings.Repeat("a", 1899), strings.Repeat("a", 1899) + `\*tail`},
		{"escape_pairs", strings.Repeat("*", 1901), strings.Repeat(`\*`, 950), strings.Repeat(`\*`, 1901)},
		{"emoji_units", strings.Repeat("😀", 951), strings.Repeat("😀", 950), strings.Repeat("😀", 951)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var parts []string
			sender := discordTestSender(t, func(_ context.Context, _, text string) error { parts = append(parts, text); return nil })
			if err := sender.Send(t.Context(), nil, nil, "500", tc.text, false); err != nil {
				t.Fatal(err)
			}
			if len(parts) == 0 || parts[0] != tc.first || strings.Join(parts, "") != tc.joined {
				t.Fatalf("segment boundary or body changed: lengths=%v", discordPartLengths(parts))
			}
			for _, part := range parts {
				if !utf8.ValidString(part) || im.UTF16Units(part) > 1900 {
					t.Fatal("invalid segment", im.UTF16Units(part))
				}
				trailing := len(part) - len(strings.TrimRight(part, `\`))
				if trailing%2 != 0 {
					t.Fatal("split an escape pair")
				}
			}
		})
	}
	if got := discord.EscapeMarkdown("*bold* _italic_ ~strike~ `code` \\ >quote"); got != `\*bold\* \_italic\_ \~strike\~ \`+"`code\\`"+` \\ \>quote` {
		t.Fatalf("Markdown controls retained: %q", got)
	}
}

func discordPartLengths(parts []string) []int {
	lengths := make([]int, len(parts))
	for index, part := range parts {
		lengths[index] = im.UTF16Units(part)
	}
	return lengths
}

func TestDiscordTextSenderTruncationReservesFinalNotice(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"segment_limit", strings.Repeat("a", 70_000)},
		{"source_byte_limit", strings.Repeat("😀", 20_000)},
		{"escaped_segment_limit", strings.Repeat("*", 40_000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var parts []string
			sender := discordTestSender(t, func(_ context.Context, _, text string) error { parts = append(parts, text); return nil })
			if err := sender.Send(t.Context(), nil, nil, "500", tc.text, false); err != nil {
				t.Fatal(err)
			}
			if len(parts) == 0 || len(parts) > 32 || !strings.Contains(parts[len(parts)-1], "Output truncated") || !strings.Contains(parts[len(parts)-1], "Chord session") {
				t.Fatalf("missing reserved notice or too many segments: count=%d", len(parts))
			}
			for _, part := range parts {
				if !utf8.ValidString(part) || im.UTF16Units(part) > 1900 {
					t.Fatal("truncation broke segment", im.UTF16Units(part))
				}
			}
			if tc.name == "source_byte_limit" && len(strings.ReplaceAll(strings.Join(parts, ""), `\`, "")) > 64<<10 {
				t.Fatal("source byte limit exceeded")
			}
		})
	}
}

func TestDiscordTextSenderPartialFailureAndCancellation(t *testing.T) {
	for _, tc := range []string{"unknown", "caller", "permit", "lifecycle"} {
		t.Run(tc, func(t *testing.T) {
			caller, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()
			permit, cancelPermit := context.WithCancel(t.Context())
			defer cancelPermit()
			lifecycle, cancelLifecycle := context.WithCancel(t.Context())
			defer cancelLifecycle()
			calls := 0
			sender := discordTestSender(t, func(ctx context.Context, _, text string) error {
				calls++
				if tc == "unknown" {
					if calls == 2 {
						return im.ErrDeliveryOutcomeUnknown
					}
					return nil
				}
				switch tc {
				case "caller":
					cancelCaller()
				case "permit":
					cancelPermit()
				case "lifecycle":
					cancelLifecycle()
				}
				<-ctx.Done()
				return nil // This segment had an explicit acceptance before cancellation.
			})
			err := sender.Send(caller, lifecycle, permit, "500", strings.Repeat("a", 5700), false)
			var partial *im.IMTextSendError
			if !errors.As(err, &partial) || partial.AcceptedSegments != 1 {
				t.Fatalf("accepted segment evidence lost: %v", err)
			}
			if tc == "unknown" {
				if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || calls != 2 {
					t.Fatal("unknown output retried or continued", calls, err)
				}
			} else if !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatal("cancelled output continued", calls, err)
			}
		})
	}
}

func TestDiscordTextSenderTargetOrderingAndQueuedCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var calls []string
	var once sync.Once
	sender := discordTestSender(t, func(_ context.Context, _, text string) error {
		once.Do(func() { close(entered); <-release })
		mu.Lock()
		calls = append(calls, text)
		mu.Unlock()
		return nil
	})
	done := make(chan error, 2)
	go func() { done <- sender.Send(t.Context(), nil, nil, "500", strings.Repeat("a", 3800), false) }()
	<-entered
	go func() { done <- sender.Send(t.Context(), nil, nil, "500", strings.Repeat("b", 3800), false) }()
	waitIngressCondition(t, func() bool { return sender.TargetRefsForTest("500") == 2 })
	queued, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sender.Send(queued, nil, nil, "500", "cancelled", false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("target output did not finish")
		}
	}
	if !reflect.DeepEqual(calls, []string{strings.Repeat("a", 1900), strings.Repeat("a", 1900), strings.Repeat("b", 1900), strings.Repeat("b", 1900)}) {
		t.Fatal("same-target output interleaved")
	}
	if sender.TargetCountForTest() != 0 {
		t.Fatal("target slot leaked")
	}
}

func discordRESTTestAdapter(t *testing.T, server *httptest.Server) *discord.DiscordAdapter {
	t.Helper()
	adapter, err := discord.NewDiscordAdapter(&config.Config{}, config.IMAdapterConfig{Discord: &config.DiscordConfig{BotToken: "placeholder", ApplicationID: "800"}}, &config.Paths{StateDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter.RESTForTest().Close(t.Context())
	client := *server.Client()
	client.Transport = discord.NewRateLimitTransportForTest(client.Transport)
	adapter.SetRESTForTest(rest.New(rest.NewClient("placeholder", rest.WithURL(server.URL), rest.WithHTTPClient(&client), rest.WithRateLimiter(discord.NewDiscordRateLimiter(adapter.ContextForTest())), rest.WithLogger(slog.New(discord.SafeLogHandlerForTest())))))
	t.Cleanup(adapter.Disconnect)
	return adapter
}

func TestDiscordRESTTextPayloadPreventsMentionsAndEmbeds(t *testing.T) {
	var got map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/channels/500/messages" {
			t.Errorf("unexpected endpoint %s %s", request.Method, request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"999","channel_id":"500","type":0,"author":{"id":"800","bot":true},"timestamp":"2026-10-14T00:00:00Z"}`)
	}))
	defer server.Close()
	adapter := discordRESTTestAdapter(t, server)
	if err := adapter.SendTextContext(t.Context(), "500", "@everyone <@123> <@&456> *sample* https://example.com"); err != nil {
		t.Fatal(err)
	}
	var mentions map[string]json.RawMessage
	if err := json.Unmarshal(got["allowed_mentions"], &mentions); err != nil {
		t.Fatal("allowed_mentions missing", err)
	}
	for _, key := range []string{"parse", "users", "roles"} {
		var values []string
		if err := json.Unmarshal(mentions[key], &values); err != nil || values == nil || len(values) != 0 {
			t.Fatalf("mention allowance %s=%s", key, mentions[key])
		}
	}
	if string(mentions["replied_user"]) != "false" {
		t.Fatal("reply notification not disabled")
	}
	var flags disgo.MessageFlags
	if err := json.Unmarshal(got["flags"], &flags); err != nil || !flags.Has(disgo.MessageFlagSuppressEmbeds) {
		t.Fatal("link previews not suppressed")
	}
	for _, key := range []string{"message_reference", "embeds", "components"} {
		if _, exists := got[key]; exists {
			t.Fatal("unexpected output field", key)
		}
	}
	var content string
	if err := json.Unmarshal(got["content"], &content); err != nil || !strings.Contains(content, `\*sample\*`) {
		t.Fatal("content not rendered as plain text", content, err)
	}
}

func TestDiscordRESTUnknownSendIsNeverRetried(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"sample failure","code":0}`)
			}))
			defer server.Close()
			adapter := discordRESTTestAdapter(t, server)
			err := adapter.SendTextContext(t.Context(), "500", strings.Repeat("a", 3800))
			var partial *im.IMTextSendError
			if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || !errors.As(err, &partial) || partial.AcceptedSegments != 0 || requests.Load() != 1 {
				t.Fatalf("unknown POST retried or misclassified: count=%d err=%v", requests.Load(), err)
			}
		})
	}
}

func TestDiscordRESTPartialFailureKeepsThreadTarget(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/channels/501/messages" {
			t.Errorf("thread output changed destination: %s", request.URL.Path)
		}
		if requests.Add(1) == 1 {
			fmt.Fprint(w, `{"id":"999","channel_id":"501"}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Missing Permissions","code":50013}`)
	}))
	defer server.Close()
	adapter := discordRESTTestAdapter(t, server)
	err := adapter.SendTextContext(t.Context(), "501", strings.Repeat("a", 5700))
	var partial *im.IMTextSendError
	if !errors.As(err, &partial) || partial.AcceptedSegments != 1 || requests.Load() != 2 || errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
		t.Fatalf("partial thread rejection retried or continued: requests=%d err=%v", requests.Load(), err)
	}
}

func TestDiscordRESTInvalidReceiptIsUnknownAndDoesNotRetry(t *testing.T) {
	for _, receipt := range []string{`{}`, `{"id":"0"}`, `invalid json`} {
		t.Run(receipt, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				fmt.Fprint(w, receipt)
			}))
			defer server.Close()
			adapter := discordRESTTestAdapter(t, server)
			err := adapter.SendTextContext(t.Context(), "500", "sample task")
			if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) || requests.Load() != 1 {
				t.Fatalf("invalid acceptance receipt was retried or trusted: count=%d err=%v", requests.Load(), err)
			}
		})
	}
}

func TestDiscordRESTRateLimitWaitingIsCancelled(t *testing.T) {
	var requests atomic.Int32
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("X-RateLimit-Bucket", "sample-bucket")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Via", "sample-proxy")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"retry_after":60,"global":false}`)
		entered <- struct{}{}
	}))
	defer server.Close()
	adapter := discordRESTTestAdapter(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- adapter.SendTextContext(ctx, "500", "sample task") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("rate-limited request not sent")
	}
	select {
	case err := <-done:
		t.Fatal("rate limit did not wait", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || requests.Load() != 1 {
			t.Fatal("cancelled rate wait retried or lost context", requests.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("rate-limit wait ignored cancellation")
	}
}
