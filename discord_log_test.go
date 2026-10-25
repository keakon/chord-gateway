package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/rest"
	"github.com/gorilla/websocket"
	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestDiscordRESTErrorNeverRetainsSensitiveSDKObjects(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/webhooks/sample-secret-token", nil)
			request.Header.Set("Authorization", "Bot sample-secret-token")
			response := &http.Response{StatusCode: status, Header: make(http.Header)}
			if status == http.StatusTooManyRequests {
				response.Header.Set("Retry-After", "0.125")
			}
			raw := &rest.Error{Request: request, RqBody: []byte("sample-request-body"), RsBody: []byte("sample-response-body"), Response: response, Message: "sample-private-error"}
			safe := discordRESTError(raw)
			var retained *rest.Error
			if safe == nil || errors.As(safe, &retained) {
				t.Fatal("SDK error object retained")
			}
			for _, marker := range []string{"sample-secret-token", "sample-request-body", "sample-response-body", "sample-private-error", "example.com", "webhooks"} {
				if strings.Contains(fmt.Sprintf("%v %+v %#v", safe, safe, safe), marker) {
					t.Fatal("sensitive SDK detail exposed", marker)
				}
			}
			if status == http.StatusTooManyRequests {
				var limited *im.IMRateLimitError
				if !errors.As(safe, &limited) || limited.RetryAfter != 125*time.Millisecond {
					t.Fatal("fractional safe retry delay lost", safe)
				}
			}
			if status == http.StatusRequestTimeout || status >= 500 {
				if !errors.Is(safe, im.ErrDeliveryOutcomeUnknown) {
					t.Fatal("unknown send outcome lost", safe)
				}
			}
		})
	}
	if got := discordRESTError(fmt.Errorf("sample-secret-token: %w", context.Canceled)); got != context.Canceled {
		t.Fatal("cancel cause not sanitized", got)
	}
	if got := discordRESTError(errors.New("sample-secret-token sample-body")); !errors.Is(got, im.ErrDeliveryOutcomeUnknown) || strings.Contains(got.Error(), "sample-secret-token") {
		t.Fatal("transport error leaked", got)
	}
	if got := discordGatewayError(&websocket.CloseError{Code: 4004, Text: "sample-secret-token sample-body"}); got.Error() != "discord gateway_closed code=4004" {
		t.Fatal("gateway close text leaked", got)
	}
}

func TestDiscordSDKLogsStripAllUntrustedFields(t *testing.T) {
	// A subprocess isolates the process-wide legacy logger hook, avoiding races
	// with unrelated adapter workers while checking the actual emitted text.
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDiscordLogCaptureHelper$", "-test.v=false")
	command.Env = append(os.Environ(), "CHORD_GATEWAY_DISCORD_LOG_HELPER=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("log helper failed: %v %s", err, output)
	}
	text := string(output)
	for _, marker := range []string{"sample-secret-token", "sample-message-body", "example.com", "sample-frame", "sample-unknown-event"} {
		if strings.Contains(text, marker) {
			t.Fatal("SDK log leaked", marker)
		}
	}
	if !strings.Contains(text, "discord event=rest_response category=invalid_response") || !strings.Contains(text, "discord event=gateway_close category=transport_error") {
		t.Fatal("allowed categories missing", text)
	}
}

type discordSecretLogValue struct{ evaluated *atomic.Int32 }

func (value discordSecretLogValue) LogValue() slog.Value {
	value.evaluated.Add(1)
	return slog.StringValue("sample-secret-token")
}

func TestDiscordLogCaptureHelper(t *testing.T) {
	if os.Getenv("CHORD_GATEWAY_DISCORD_LOG_HELPER") != "1" {
		return
	}
	log.Warnf = func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
	var evaluated atomic.Int32
	handler := discordSafeLogHandler{}
	if handler.Enabled(t.Context(), slog.LevelDebug) || handler.Enabled(t.Context(), slog.LevelInfo) || !handler.Enabled(t.Context(), slog.LevelWarn) {
		t.Fatal("SDK logging level changed")
	}
	logger := slog.New(handler).With("token", "sample-secret-token", "lazy", discordSecretLogValue{&evaluated}).WithGroup("sample-frame")
	for _, event := range []string{"error unmarshalling response body", "error closing websocket connection", "sample-unknown-event sample-secret-token", "new response"} {
		logger.ErrorContext(t.Context(), event,
			slog.String("body", "sample-message-body"), slog.String("endpoint", "https://example.com/webhooks/sample-secret-token"),
			slog.Any("err", errors.New("sample-frame sample-secret-token")), slog.Any("lazy", discordSecretLogValue{&evaluated}))
	}
	if evaluated.Load() != 0 {
		t.Fatal("SDK logging evaluated secret attributes")
	}
}
