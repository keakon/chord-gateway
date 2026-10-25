package discord

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"github.com/keakon/chord-gateway/internal/im"
)

// Preserve Discord's fractional retry delay for the owned REST rate limiter,
// including body-only and global responses. This transport never retries a
// request or reads successful response bodies.
type discordRateLimitTransport struct{ base http.RoundTripper }

// discordRESTTimeout stays below the shared control-send budget so a stalled
// REST request surfaces as a timeout instead of being cut off by it, which would
// leave every control send reporting unknown_send.
const discordRESTTimeout = 10 * time.Second

func newDiscordHTTPClient(proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	return im.MessageHTTPClient(&http.Client{Timeout: discordRESTTimeout, Transport: discordRateLimitTransport{base: im.ProxiedTransport(proxyFunc)}})
}

func discordProxyDialer(proxyFunc func(*http.Request) (*url.URL, error)) *websocket.Dialer {
	dialer := *websocket.DefaultDialer
	// gorilla/websocket rejects socks5h; the shared wrapper rewrites it to
	// socks5 (identical remote-DNS behavior) and keeps nil as direct.
	dialer.Proxy = im.WebsocketProxyFunc(proxyFunc)
	return &dialer
}

func (t discordRateLimitTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func discordRetryDelay(value string) time.Duration {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return 0
	}
	return time.Duration(math.Ceil(seconds * float64(time.Second)))
}

func (t discordRateLimitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	outcome, _ := request.Context().Value(SendOutcomeKey{}).(*SendOutcome)
	var attempt *discordSendAttempt
	if outcome != nil {
		attempt = outcome.beginAttempt()
		trace := &httptrace.ClientTrace{
			WroteHeaders: func() { outcome.wroteAttempt(attempt) },
			WroteRequest: func(httptrace.WroteRequestInfo) { outcome.wroteAttempt(attempt) },
		}
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return response, err
	}
	if outcome != nil {
		outcome.responseAttempt(attempt, response.StatusCode)
	}
	if response.StatusCode != http.StatusTooManyRequests {
		return response, nil
	}
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	delay := discordRetryDelay(response.Header.Get("Retry-After"))
	if response.Body != nil {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if readErr != nil {
			_ = response.Body.Close()
			return nil, readErr
		}
		response.Body = &discordReplayBody{Reader: io.MultiReader(bytes.NewReader(body), response.Body), closer: response.Body}
		var payload struct {
			RetryAfter float64 `json:"retry_after"`
			Global     bool    `json:"global"`
		}
		if json.Unmarshal(body, &payload) == nil {
			if bodyDelay := discordRetryDelay(strconv.FormatFloat(payload.RetryAfter, 'f', -1, 64)); bodyDelay > delay {
				delay = bodyDelay
			}
			if payload.Global {
				response.Header.Set("X-RateLimit-Global", "true")
			}
		}
	}
	if delay <= 0 {
		return response, nil
	}
	response.Header.Set("Retry-After", strconv.FormatFloat(delay.Seconds(), 'f', -1, 64))
	return response, nil
}

type discordReplayBody struct {
	io.Reader
	closer io.Closer
}

func (b *discordReplayBody) Close() error { return b.closer.Close() }
