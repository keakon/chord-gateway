package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/buildinfo"
	"github.com/keakon/chord-gateway/internal/im"
)

// discordRESTAdapterForTest builds an adapter whose REST client targets a stub
// server through the owned rate-limit transport. Package-local twin of the
// root package helper, which cannot reach unexported fields across packages.
func discordRESTAdapterForTest(t *testing.T, server *httptest.Server) *DiscordAdapter {
	t.Helper()
	adapter, err := NewDiscordAdapter(&config.Config{}, config.IMAdapterConfig{Discord: &config.DiscordConfig{BotToken: "placeholder", ApplicationID: "800"}}, &config.Paths{StateDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter.rest.Close(t.Context())
	client := *server.Client()
	client.Transport = discordRateLimitTransport{base: client.Transport}
	adapter.rest = rest.New(rest.NewClient("placeholder", rest.WithURL(server.URL), rest.WithHTTPClient(&client), rest.WithRateLimiter(NewDiscordRateLimiter(adapter.ctx)), rest.WithLogger(slog.New(discordSafeLogHandler{}))))
	t.Cleanup(adapter.Disconnect)
	return adapter
}

// discordMetricsAdapterForTest builds an adapter with an injected send segment.
func discordMetricsAdapterForTest(t *testing.T, send func(context.Context, string, string) error) *DiscordAdapter {
	t.Helper()
	adapter, err := NewDiscordAdapter(nil, config.IMAdapterConfig{Discord: &config.DiscordConfig{BotToken: "placeholder", ApplicationID: "800"}}, &config.Paths{StateDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewDiscordTextSender(send)
	if err != nil {
		t.Fatal(err)
	}
	adapter.sender = sender
	t.Cleanup(adapter.Disconnect)
	return adapter
}

type discordRoundTripFunc func(*http.Request) (*http.Response, error)

func (f discordRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type discordTrackedBody struct {
	reader        io.Reader
	reads, closes atomic.Int32
}

func (b *discordTrackedBody) Read(p []byte) (int, error) { b.reads.Add(1); return b.reader.Read(p) }
func (b *discordTrackedBody) Close() error               { b.closes.Add(1); return nil }

func TestDiscordRetryDelayPreservesFractionalSeconds(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"0.125", 125 * time.Millisecond}, {"0.0000000001", time.Nanosecond}, {"2.5", 2500 * time.Millisecond},
		{"", 0}, {"0", 0}, {"-1", 0}, {"NaN", 0}, {"+Inf", 0}, {"1e100", 0}, {"malformed", 0},
	} {
		if got := discordRetryDelay(tc.value); got != tc.want {
			t.Fatalf("delay(%q)=%s want %s", tc.value, got, tc.want)
		}
	}
}

func TestDiscordHTTPRateLimitNormalizationPreservesBodyAndFraction(t *testing.T) {
	for _, tc := range []struct {
		name, header, body string
		want               time.Duration
		global             bool
	}{
		{"fractional_header", "0.125", `{}`, 125 * time.Millisecond, false},
		{"body_only", "", `{"retry_after":0.25}`, 250 * time.Millisecond, false},
		{"larger_body", "0.125", `{"retry_after":0.75}`, 750 * time.Millisecond, false},
		{"larger_header", "0.75", `{"retry_after":0.125}`, 750 * time.Millisecond, false},
		{"global_no_bucket", "", `{"retry_after":0.05,"global":true}`, 50 * time.Millisecond, true},
		{"malformed_body_valid_header", "0.125", `invalid json`, 125 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &discordTrackedBody{reader: strings.NewReader(tc.body)}
			headers := make(http.Header)
			if tc.header != "" {
				headers.Set("Retry-After", tc.header)
			}
			calls := 0
			transport := discordRateLimitTransport{base: discordRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers, Body: body}, nil
			})}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/channels/500/messages", strings.NewReader("sample body"))
			if err != nil {
				t.Fatal(err)
			}
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			if got := discordRetryDelay(response.Header.Get("Retry-After")); got != tc.want {
				t.Fatalf("delay=%s want %s", got, tc.want)
			}
			if (response.Header.Get("X-RateLimit-Global") != "") != tc.global {
				t.Fatal("global limit lost")
			}
			if response.Header.Get("X-RateLimit-Bucket") != "" {
				t.Fatal("invented server bucket identity")
			}
			if body.closes.Load() != 0 || calls != 1 {
				t.Fatal("body closed early or POST repeated")
			}
			got, err := io.ReadAll(response.Body)
			if err != nil || string(got) != tc.body {
				t.Fatal("response body changed", string(got), err)
			}
			if err := response.Body.Close(); err != nil || body.closes.Load() != 1 {
				t.Fatal("underlying response body not closed", err)
			}
		})
	}
}

func TestDiscordHTTPNon429ResponseIsUntouched(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			body := &discordTrackedBody{reader: strings.NewReader("sample response")}
			headers := make(http.Header)
			headers.Set("X-RateLimit-Reset-After", "0.025")
			original := &http.Response{StatusCode: status, Header: headers, Body: body}
			transport := discordRateLimitTransport{base: discordRoundTripFunc(func(*http.Request) (*http.Response, error) { return original, nil })}
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/channels/500/messages", nil)
			response, err := transport.RoundTrip(request)
			if err != nil || response != original || response.Body != body || body.reads.Load() != 0 || response.Header.Get("X-RateLimit-Reset-After") != "0.025" {
				t.Fatal("successful scheduling rounded up or body read", err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDiscordHTTPBoundedInspectionStillReplaysWholeResponse(t *testing.T) {
	content := strings.Repeat("x", 80<<10)
	body := &discordTrackedBody{reader: strings.NewReader(content)}
	headers := make(http.Header)
	headers.Set("Retry-After", "0.125")
	transport := discordRateLimitTransport{base: discordRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers, Body: body}, nil
	})}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/channels/500/messages", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	if err != nil || string(got) != content {
		t.Fatal("large response body truncated", len(got), err)
	}
	if err := response.Body.Close(); err != nil || body.closes.Load() != 1 {
		t.Fatal("large response body leaked", err)
	}
}

type discordFailingReader struct{ err error }

func (r discordFailingReader) Read([]byte) (int, error) { return 0, r.err }

func TestDiscordHTTPReadFailureClosesUnderlyingBody(t *testing.T) {
	failure := errors.New("sample read failure")
	body := &discordTrackedBody{reader: discordFailingReader{failure}}
	transport := discordRateLimitTransport{base: discordRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: body}, nil
	})}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/channels/500/messages", nil)
	response, err := transport.RoundTrip(request)
	if !errors.Is(err, failure) || response != nil || body.closes.Load() != 1 {
		t.Fatal("failed response body leaked", response, err)
	}
}

func TestDiscordRESTClientIdentifiesItselfToDiscord(t *testing.T) {
	var mu sync.Mutex
	var agents []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		agents = append(agents, request.Header.Get("User-Agent"))
		mu.Unlock()
		if request.URL.Path != "/users/@me" {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"500","bot":true}`)
	}))
	defer server.Close()
	client := NewDiscordRESTClient(t.Context(), &http.Client{Timeout: 3 * time.Second}, "placeholder", rest.WithURL(server.URL))
	var user discord.User
	if err := client.Do(rest.GetCurrentUser.Compile(nil), nil, &user, rest.WithCtx(t.Context())); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(agents) != 1 {
		t.Fatalf("requests=%d", len(agents))
	}
	if !strings.HasPrefix(agents[0], "DiscordBot ("+discordRepositoryURL+", ") || !strings.Contains(agents[0], buildinfo.Current().Short()) {
		t.Fatalf("User-Agent %q does not identify this gateway", agents[0])
	}
}

func TestDiscordHTTPClientStopsPOSTRedirects(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/redirected" {
			w.Header().Set("Location", "/redirected")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		fmt.Fprint(w, `{"id":"999"}`)
	}))
	defer server.Close()
	client := newDiscordHTTPClient(nil)
	if client.Timeout != discordRESTTimeout {
		t.Fatalf("HTTP timeout changed: %s", client.Timeout)
	}
	if discordRESTTimeout >= im.IMControlSendTimeout {
		t.Fatalf("REST timeout %s hides the control send budget %s", discordRESTTimeout, im.IMControlSendTimeout)
	}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/channels/500/messages", strings.NewReader("sample task"))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || requests.Load() != 1 {
		t.Fatal("POST body replayed by redirect")
	}
}

func TestDiscordHTTPInvalidRateLimitNeverCreatesRetryStorm(t *testing.T) {
	for _, payload := range []string{`invalid json`, `{"retry_after":0}`, `{"retry_after":-1}`, `{"retry_after":"0.125"}`, `{}`} {
		t.Run(payload, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, payload)
			}))
			defer server.Close()
			adapter := discordRESTAdapterForTest(t, server)
			adapter.rest.HTTPClient().Transport = discordRateLimitTransport{base: server.Client().Transport}
			err := adapter.SendTextContext(t.Context(), "500", "sample task")
			if err == nil || requests.Load() != 1 {
				t.Fatal("invalid retry delay created retry storm", requests.Load(), err)
			}
		})
	}
}

func TestDiscordHTTPBodyOnlyRateLimitRetriesAfterFractionalDelay(t *testing.T) {
	var requests atomic.Int32
	var mu sync.Mutex
	var first time.Time
	var elapsed time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if requests.Add(1) == 1 {
			first = time.Now()
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"retry_after":0.03,"global":true}`)
			return
		}
		elapsed = time.Since(first)
		fmt.Fprint(w, `{"id":"999","channel_id":"500"}`)
	}))
	defer server.Close()
	adapter := discordRESTAdapterForTest(t, server)
	adapter.rest.HTTPClient().Transport = discordRateLimitTransport{base: server.Client().Transport}
	ctx, cancel := context.WithTimeout(t.Context(), 750*time.Millisecond)
	defer cancel()
	if err := adapter.SendTextContext(ctx, "500", "sample task"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests.Load() != 2 || elapsed < 25*time.Millisecond || elapsed >= 700*time.Millisecond {
		t.Fatal("fractional global retry was early or rounded to seconds", requests.Load(), elapsed)
	}
}

type discordIdleCloseTransport struct{ closed atomic.Int32 }

func (*discordIdleCloseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unused test transport")
}
func (t *discordIdleCloseTransport) CloseIdleConnections() { t.closed.Add(1) }

func TestDiscordDisconnectReleasesOwnedRESTPool(t *testing.T) {
	adapter := discordMetricsAdapterForTest(t, func(context.Context, string, string) error { return nil })
	base := &discordIdleCloseTransport{}
	adapter.httpClient = &http.Client{Transport: base}
	adapter.Disconnect()
	if base.closed.Load() != 1 {
		t.Fatal("Disconnect left the owned REST pool open", base.closed.Load())
	}
	adapter.Disconnect()
	if base.closed.Load() != 1 {
		t.Fatal("repeated Disconnect closed the REST pool again", base.closed.Load())
	}
}

func TestDiscordHTTPClientOwnsPoolAndDelegatesIdleClose(t *testing.T) {
	client := newDiscordHTTPClient(nil)
	wrapped, ok := client.Transport.(discordRateLimitTransport)
	if !ok {
		t.Fatal("rate-limit transport missing")
	}
	pool, ok := wrapped.base.(*http.Transport)
	if !ok || pool == http.DefaultTransport {
		t.Fatal("Discord reused the process-wide HTTP pool")
	}
	client.CloseIdleConnections()
	base := &discordIdleCloseTransport{}
	client.Transport = discordRateLimitTransport{base: base}
	client.CloseIdleConnections()
	if base.closed.Load() != 1 {
		t.Fatal("HTTP idle-close call did not reach owned transport")
	}
}
