package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/disgoorg/disgo/rest"
	"github.com/gorilla/websocket"
	"github.com/keakon/golog/log"

	gateway "github.com/keakon/chord-gateway/internal/discordgateway"
)

// SDK attributes can include frames, tokens and complete HTTP bodies. Only
// fixed event names and error categories are forwarded; attributes are dropped.
type discordSafeLogHandler struct{}

func (discordSafeLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}
func (discordSafeLogHandler) Handle(_ context.Context, record slog.Record) error {
	switch record.Message {
	case "error unmarshalling response body":
		discordLogCategory("rest_response", "invalid_response")
	case "error closing websocket connection":
		discordLogCategory("gateway_close", "transport_error")
	}
	return nil
}
func (h discordSafeLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h discordSafeLogHandler) WithGroup(string) slog.Handler      { return h }
func discordLogCategory(event, category string) {
	log.Warnf("discord event=%v category=%v", event, category)
}

// A contentless mention is an ordinary interaction shape, not an incident, so
// its log line is rate limited instead of counted per occurrence. The process
// budget keeps a busy channel from filling the log at the allowed interval.
const (
	discordContentlessInterval = time.Minute
	discordContentlessBudget   = 64
)

var discordContentless = struct {
	mu       sync.Mutex
	last     time.Time
	reported int
}{}

func discordContentlessReported() bool {
	now := time.Now()
	discordContentless.mu.Lock()
	defer discordContentless.mu.Unlock()
	if discordContentless.reported >= discordContentlessBudget {
		return false
	}
	if !discordContentless.last.IsZero() && now.Sub(discordContentless.last) < discordContentlessInterval {
		return false
	}
	discordContentless.last, discordContentless.reported = now, discordContentless.reported+1
	return true
}
func discordRESTError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("discord unknown_send: %w", errDeliveryOutcomeUnknown)
	}
	var response *rest.Error
	if errors.As(err, &response) && response.Response != nil {
		status := response.Response.StatusCode
		if status == http.StatusUnauthorized {
			return errors.New("discord authentication_failed")
		}
		if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
			switch response.Code {
			case rest.JSONErrorCodeThreadLocked:
				return errors.New("discord thread_locked")
			case rest.JSONErrorCodeUnknownChannel:
				return errors.New("discord channel_unavailable")
			}
		}
		if status == http.StatusForbidden {
			return errors.New("discord permission_denied")
		}
		if status == http.StatusTooManyRequests {
			if delay := discordRetryDelay(response.Response.Header.Get("Retry-After")); delay > 0 {
				return &IMRateLimitError{RetryAfter: delay}
			}
			return errors.New("discord invalid_rate_limit")
		}
		if status == http.StatusRequestTimeout {
			return fmt.Errorf("discord unknown_send: %w", errDeliveryOutcomeUnknown)
		}
		if status >= 400 && status < 500 {
			return fmt.Errorf("discord request_rejected status=%d", status)
		}
	}
	return fmt.Errorf("discord unknown_send: %w", errDeliveryOutcomeUnknown)
}
func discordGatewayError(err error) error {
	if errors.Is(err, gateway.ErrSessionStartLimitFatal) {
		return errors.New("discord identify_quota_unavailable")
	}
	if errors.Is(err, gateway.ErrReplayCapacityExceeded) {
		return errors.New("discord replay_capacity_exceeded; check the original Chord session before restarting")
	}
	var closeError *websocket.CloseError
	if errors.As(err, &closeError) {
		return fmt.Errorf("discord gateway_closed code=%d", closeError.Code)
	}
	return errors.New("discord gateway_unavailable")
}
