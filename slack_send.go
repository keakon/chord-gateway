package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const slackTextBudget = 3500

type slackPostClient interface {
	PostMessageContext(context.Context, string, ...slack.MsgOption) (string, string, error)
}

type slackSender struct {
	client slackPostClient
	text   *IMTextSender
	limits *slackSendLimits
}

func newSlackSender(client slackPostClient) (*slackSender, error) {
	if client == nil {
		return nil, errors.New("slack sender requires an API client")
	}
	s := &slackSender{client: client, limits: newSlackSendLimits()}
	text, err := NewIMTextSender(IMTextSendOptions{
		Platform: "slack", Budget: slackTextBudget,
		Render: escapeSlackText, Measure: imUTF16Units, SendSegment: s.sendSegment,
	})
	if err != nil {
		return nil, fmt.Errorf("create slack text sender: %w", err)
	}
	s.text = text
	return s, nil
}

func (s *slackSender) Send(ctx, lifecycle context.Context, target, text string) error {
	if _, _, err := decodeSlackTarget(target); err != nil {
		return &IMTextSendError{Category: "invalid_target", Cause: errors.New("invalid slack output target")}
	}
	return s.text.Send(ctx, lifecycle, nil, target, text, false)
}

func escapeSlackText(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

func (s *slackSender) sendSegment(ctx context.Context, target, text string) error {
	channel, thread, err := decodeSlackTarget(target)
	if err != nil {
		return errors.New("invalid slack output target")
	}
	if err = s.limits.wait(ctx, channel); err != nil {
		return err
	}
	options := []slack.MsgOption{
		slack.MsgOptionText(text, false), slack.MsgOptionDisableMarkdown(),
		slack.MsgOptionParse(false), slack.MsgOptionLinkNames(false),
		slack.MsgOptionDisableLinkUnfurl(), slack.MsgOptionDisableMediaUnfurl(),
	}
	if thread != "" {
		options = append(options, slack.MsgOptionTS(thread))
	}
	responseChannel, ts, err := s.client.PostMessageContext(ctx, channel, options...)
	if err != nil {
		var limited *slack.RateLimitedError
		if errors.As(err, &limited) && limited.RetryAfter > 0 {
			// The receipt transport already published the method cooldown.
			return &IMRateLimitError{RetryAfter: limited.RetryAfter}
		}
		return sanitizeSlackSendError(err)
	}
	if !validSlackTimestamp(ts) || responseChannel != channel {
		return fmt.Errorf("%w: slack message receipt is missing or inconsistent", errDeliveryOutcomeUnknown)
	}
	return nil
}

func sanitizeSlackSendError(err error) error {
	var unsent *slackUnsentRequestError
	if errors.As(err, &unsent) {
		return unsent.cause
	}
	var apiError slack.SlackErrorResponse
	if errors.As(err, &apiError) {
		switch apiError.Err {
		case "internal_error", "fatal_error":
			return fmt.Errorf("%w: slack message operation %s", errDeliveryOutcomeUnknown, apiError.Err)
		case "channel_not_found", "not_in_channel", "is_archived", "missing_scope",
			"invalid_auth", "account_inactive", "token_revoked", "not_authed":
			return fmt.Errorf("slack message rejected: %s", apiError.Err)
		default:
			return errors.New("slack message rejected: slack_api_rejected")
		}
	}
	var status slack.StatusCodeError
	if errors.As(err, &status) && !isAmbiguousDeliveryStatus(status.Code) {
		return fmt.Errorf("slack message rejected: HTTP %d", status.Code)
	}
	if errors.Is(err, context.Canceled) {
		return errors.Join(errDeliveryOutcomeUnknown, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(errDeliveryOutcomeUnknown, context.DeadlineExceeded)
	}
	// SDK errors may contain request URLs, response bodies or credentials.
	return fmt.Errorf("%w: slack message request was not confirmed", errDeliveryOutcomeUnknown)
}

// Slack's SDK accepts an HTTP 200 with ok:false and no error as successful.
// Verify the receipt before its parser sees it, without modifying its source.
type slackReceiptTransport struct {
	base   http.RoundTripper
	limits *slackMethodLimits
}

// Cancellation while waiting for a method cooldown has no delivery ambiguity.
type slackUnsentRequestError struct{ cause error }

func (e *slackUnsentRequestError) Error() string { return "slack request cancelled before transport" }
func (e *slackUnsentRequestError) Unwrap() error { return e.cause }

func (t slackReceiptTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t slackReceiptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	method := path.Base(req.URL.Path)
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		seconds, parseErr := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64)
		if parseErr == nil && seconds > 0 && seconds <= int64(time.Duration(1<<63-1)/time.Second) {
			t.limits.block(method, time.Duration(seconds)*time.Second)
		}
	}
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	_ = resp.Body.Close()
	if readErr != nil || len(body) > 1<<20 {
		return nil, errors.New("slack receipt could not be read")
	}
	var receipt struct {
		OK    *bool  `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	if json.Unmarshal(body, &receipt) != nil || receipt.OK == nil ||
		!*receipt.OK && strings.TrimSpace(receipt.Error) == "" ||
		*receipt.OK && strings.HasSuffix(req.URL.Path, "/chat.postMessage") && !validSlackTimestamp(receipt.TS) {
		return nil, errors.New("slack receipt was not confirmed")
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// Wait outside http.Client.Do: its Timeout must bound the network operation,
// not erase the unsent evidence of a cancelled cooldown wait.
type slackHTTPClient struct {
	client *http.Client
	limits *slackMethodLimits
}

func (c *slackHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if err := c.limits.wait(req.Context(), path.Base(req.URL.Path)); err != nil {
		return nil, &slackUnsentRequestError{cause: err}
	}
	return c.client.Do(req)
}

// closeIdleConnections releases pooled sockets on adapter shutdown. It is
// nil-safe so a partially constructed adapter can still be stopped.
func (c *slackHTTPClient) closeIdleConnections() {
	if c == nil || c.client == nil {
		return
	}
	if closer, ok := c.client.Transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func newSlackHTTPClient(base *http.Client, proxyFunc func(*http.Request) (*url.URL, error)) *slackHTTPClient {
	client := messageHTTPClient(base)
	client.Timeout = 15 * time.Second
	// A configured proxy always wins over an injected transport. Production
	// passes a nil base, so proxyFunc also supplies the direct-connection default.
	if proxyFunc != nil || client.Transport == nil {
		client.Transport = proxiedTransport(proxyFunc)
	}
	limits := &slackMethodLimits{blocked: make(map[string]time.Time)}
	client.Transport = slackReceiptTransport{base: client.Transport, limits: limits}
	return &slackHTTPClient{client: client, limits: limits}
}
