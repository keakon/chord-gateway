package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/buildinfo"
)

const dingtalkAPIMaxResponse = 1 << 20

// Never include upstream response bodies, request URLs, IDs, or credentials in
// errors: HTTP errors can otherwise expose the registration ticket or token.
type dingtalkAPIError struct {
	Category string
	cause    error
}

func (e *dingtalkAPIError) Error() string  { return "dingtalk: " + e.Category }
func (e *dingtalkAPIError) Unwrap() error  { return e.cause }
func (e *dingtalkAPIError) Terminal() bool { return e.Category == "auth_failed" }

func (e *dingtalkAPIError) IMTextSendFailureCategory() string {
	switch e.Category {
	case "unknown_send":
		return "unknown_send"
	case "token_unavailable":
		// Token acquisition cannot have delivered the message, so it must
		// never report unknown_send (which implies possible delivery).
		return "delivery_failed"
	case "auth_failed":
		return "credentials"
	case "proactive_send_denied":
		return "forbidden"
	case "target_unreachable":
		return "invalid_target"
	case "rate_limited":
		return "platform_rejected"
	case "invalid_request":
		return "invalid_request"
	case "connection_unavailable":
		return "connection_unavailable"
	default:
		return "delivery_failed"
	}
}

func dingtalkTerminalAuthError(err error) bool {
	var e *dingtalkAPIError
	return errors.As(err, &e) && e.Terminal()
}

type dingtalkAPI struct {
	lifecycle context.Context
	cfg       config.DingtalkConfig
	client    *http.Client
	baseURL   string // Internal test seam; production always uses api.dingtalk.com.
	tokens    dingtalkTokenCache

	rateMu          sync.Mutex
	accountNext     time.Time
	targetNext      map[string]time.Time
	accountInterval time.Duration
	targetInterval  time.Duration
	retryBase       time.Duration
	observeRateWait func(time.Duration)
}

func newDingtalkAPI(lifecycle context.Context, cfg *config.DingtalkConfig, proxyFunc func(*http.Request) (*url.URL, error)) *dingtalkAPI {
	transport := proxiedTransport(proxyFunc)
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 5 * time.Second
	return &dingtalkAPI{
		lifecycle: lifecycle,
		cfg:       *cfg,
		client: &http.Client{
			Timeout:   15 * time.Second,
			Transport: transport,
			// A redirected POST must never forward clientSecret or accessToken.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		baseURL:         "https://api.dingtalk.com",
		targetNext:      make(map[string]time.Time),
		accountInterval: 500 * time.Millisecond,
		targetInterval:  3 * time.Second,
		retryBase:       time.Second,
	}
}

func (a *dingtalkAPI) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	joined, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.lifecycle, cancel)
	if a.lifecycle.Err() != nil {
		cancel()
	}
	return joined, func() { stop(); cancel() }
}

type dingtalkHTTPResponse struct {
	status     int
	retryAfter string
	body       []byte
}

func (a *dingtalkAPI) post(ctx context.Context, path, token string, payload any) (dingtalkHTTPResponse, error) {
	var result dingtalkHTTPResponse
	if err := ctx.Err(); err != nil {
		return result, err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return result, &dingtalkAPIError{Category: "invalid_request"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return result, &dingtalkAPIError{Category: "invalid_request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("x-acs-dingtalk-access-token", token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return result, &dingtalkAPIError{Category: "unknown_send", cause: ctx.Err()}
	}
	defer resp.Body.Close()
	result.status = resp.StatusCode
	result.retryAfter = resp.Header.Get("Retry-After")
	result.body, err = io.ReadAll(io.LimitReader(resp.Body, dingtalkAPIMaxResponse+1))
	if err != nil || len(result.body) > dingtalkAPIMaxResponse {
		return result, &dingtalkAPIError{Category: "unknown_send", cause: ctx.Err()}
	}
	return result, nil
}

type dingtalkResponseError struct {
	Code      json.RawMessage `json:"code"`
	ErrCode   json.RawMessage `json:"errcode"`
	ErrorCode json.RawMessage `json:"errorCode"`
	Error     json.RawMessage `json:"error"`
	Success   *bool           `json:"success"`
}

func dingtalkCode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func dingtalkSuccessCode(s string) bool {
	switch s {
	case "", "0", "200", "OK", "ok", "Success", "success":
		return true
	default:
		return false
	}
}

func (r dingtalkResponseError) failed() bool {
	errorValue := dingtalkCode(r.Error)
	return r.Success != nil && !*r.Success || !dingtalkSuccessCode(dingtalkCode(r.Code)) || !dingtalkSuccessCode(dingtalkCode(r.ErrCode)) || !dingtalkSuccessCode(dingtalkCode(r.ErrorCode)) || (errorValue != "" && errorValue != "false" && errorValue != "0")
}

func dingtalkHTTPError(status int, registration bool, body []byte) error {
	category := "unknown_send"
	var response dingtalkResponseError
	_ = json.Unmarshal(body, &response)
	code := strings.ToLower(dingtalkCode(response.Code))
	if code == "" {
		code = strings.ToLower(dingtalkCode(response.ErrCode))
	}
	if code == "" {
		code = strings.ToLower(dingtalkCode(response.ErrorCode))
	}
	switch {
	case status == http.StatusUnauthorized:
		category = "auth_failed"
	case status == http.StatusForbidden:
		category = "proactive_send_denied"
		if registration {
			category = "auth_failed"
		}
	case status == http.StatusTooManyRequests:
		category = "rate_limited"
	case strings.Contains(code, "invalidclient"), strings.Contains(code, "invalidappkey"), strings.Contains(code, "invalidappsecret"), strings.Contains(code, "invalidauthentication"), strings.Contains(code, "invalidaccesstoken"), strings.Contains(code, "accesstokenexpired"):
		category = "auth_failed"
	case strings.Contains(code, "forbidden"), strings.Contains(code, "permission"), strings.Contains(code, "unauthorized"):
		category = "proactive_send_denied"
		if registration {
			category = "auth_failed"
		}
	case strings.Contains(code, "flowcontrol"), strings.Contains(code, "throttl"), strings.Contains(code, "ratelimit"):
		category = "rate_limited"
	case status == http.StatusNotFound, strings.Contains(code, "conversation"), strings.Contains(code, "invalidstaff"), strings.Contains(code, "invaliduser"), strings.Contains(code, "robotnotingroup"):
		category = "target_unreachable"
	}
	return &dingtalkAPIError{Category: category}
}

// openConnection returns a fresh, single-use registration ticket. The Stream
// supervisor owns URL validation, dialing, and ticket lifetime.
func (a *dingtalkAPI) openConnection(ctx context.Context) (endpoint, ticket string, err error) {
	ctx, cancel := a.requestContext(ctx)
	defer cancel()
	payload := struct {
		ClientID      string `json:"clientId"`
		ClientSecret  string `json:"clientSecret"`
		Subscriptions []struct {
			Type  string `json:"type"`
			Topic string `json:"topic"`
		} `json:"subscriptions"`
		UA string `json:"ua"`
	}{ClientID: a.cfg.ClientID, ClientSecret: a.cfg.ClientSecret, UA: "chord-gateway-go/" + buildinfo.Current().Short()}
	payload.Subscriptions = append(payload.Subscriptions, struct {
		Type  string `json:"type"`
		Topic string `json:"topic"`
	}{Type: "CALLBACK", Topic: "/v1.0/im/bot/messages/get"})
	response, err := a.post(ctx, "/v1.0/gateway/connections/open", "", payload)
	if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
		return "", "", dingtalkHTTPError(response.status, true, response.body)
	}
	if err != nil {
		return "", "", err
	}
	var result struct {
		dingtalkResponseError
		Endpoint string `json:"endpoint"`
		Ticket   string `json:"ticket"`
	}
	if response.status < 200 || response.status >= 300 {
		return "", "", dingtalkHTTPError(response.status, true, response.body)
	}
	if json.Unmarshal(response.body, &result) != nil {
		return "", "", &dingtalkAPIError{Category: "unknown_send"}
	}
	if result.failed() {
		return "", "", dingtalkHTTPError(response.status, true, response.body)
	}
	u, parseErr := url.Parse(result.Endpoint)
	if parseErr != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || result.Ticket == "" {
		return "", "", &dingtalkAPIError{Category: "auth_failed"}
	}
	return result.Endpoint, result.Ticket, nil
}

func dingtalkWait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Both local pacing and explicit 429 retries contribute their actual waiting
// time, including a wait interrupted by cancellation. Never count HTTP time.
func (a *dingtalkAPI) waitRateLimit(ctx context.Context, delay time.Duration) error {
	if delay <= 0 || ctx.Err() != nil {
		return ctx.Err()
	}
	started := time.Now()
	err := dingtalkWait(ctx, delay)
	if a.observeRateWait != nil {
		a.observeRateWait(time.Since(started))
	}
	return err
}

// Reserve only when the slot becomes available. A canceled waiter does not
// consume future slots, and every retry counts against both limiters.
func (a *dingtalkAPI) waitSendSlot(ctx context.Context, target string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		a.rateMu.Lock()
		next := a.accountNext
		if t := a.targetNext[target]; t.After(next) {
			next = t
		}
		if !next.After(now) {
			a.accountNext = now.Add(a.accountInterval)
			for key, until := range a.targetNext {
				if !until.After(now) {
					delete(a.targetNext, key)
				}
			}
			// Target keys come from callbacks; bound the table so a flood of
			// distinct chats cannot grow it without limit. Expired entries were
			// just swept; evict one arbitrary entry only when still over budget.
			if len(a.targetNext) >= 128 {
				for key := range a.targetNext {
					delete(a.targetNext, key)
					break
				}
			}
			a.targetNext[target] = now.Add(a.targetInterval)
			a.rateMu.Unlock()
			return nil
		}
		a.rateMu.Unlock()
		if err := a.waitRateLimit(ctx, next.Sub(now)); err != nil {
			return err
		}
	}
}

func dingtalkRetryDelay(value string, fallback time.Duration) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		// Saturate instead of overflowing an attacker-controlled Retry-After.
		if seconds > int64((time.Duration(1<<63-1))/time.Second) {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return fallback
}

func (a *dingtalkAPI) SendText(ctx context.Context, kind, targetID, text string) error {
	ctx, cancel := a.requestContext(ctx)
	defer cancel()
	if targetID == "" || (kind != "private" && kind != "group") {
		return &dingtalkAPIError{Category: "target_unreachable"}
	}
	msgParam, _ := json.Marshal(struct {
		Content string `json:"content"`
	}{Content: text})
	payload := struct {
		RobotCode          string   `json:"robotCode"`
		UserIDs            []string `json:"userIds,omitempty"`
		OpenConversationID string   `json:"openConversationId,omitempty"`
		MsgKey             string   `json:"msgKey"`
		MsgParam           string   `json:"msgParam"`
	}{RobotCode: a.cfg.RobotCode, MsgKey: "sampleText", MsgParam: string(msgParam)}
	path := "/v1.0/robot/groupMessages/send"
	if kind == "private" {
		payload.UserIDs = []string{targetID}
		path = "/v1.0/robot/oToMessages/batchSend"
	} else {
		payload.OpenConversationID = targetID
	}
	for attempt := 0; ; attempt++ {
		token, err := a.accessToken(ctx)
		if err != nil {
			// Token acquisition cannot have delivered this message.
			var failure *dingtalkAPIError
			if errors.As(err, &failure) && failure.Category == "unknown_send" {
				return &dingtalkAPIError{Category: "token_unavailable", cause: failure.cause}
			}
			return err
		}
		if err := a.waitSendSlot(ctx, kind+":"+targetID); err != nil {
			return err
		}
		response, err := a.post(ctx, path, token, payload)
		if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
			a.invalidateToken(token)
			return dingtalkHTTPError(response.status, false, response.body)
		}
		if err != nil {
			return err
		}
		if response.status == http.StatusTooManyRequests && attempt < 3 {
			delay := dingtalkRetryDelay(response.retryAfter, a.retryBase<<attempt)
			if err := a.waitRateLimit(ctx, delay); err != nil {
				return &dingtalkAPIError{Category: "rate_limited", cause: err}
			}
			continue
		}
		if response.status < 200 || response.status >= 300 {
			return dingtalkHTTPError(response.status, false, response.body)
		}
		var result struct {
			dingtalkResponseError
			ProcessQueryKey           string   `json:"processQueryKey"`
			InvalidStaffIDList        []string `json:"invalidStaffIdList"`
			FlowControlledStaffIDList []string `json:"flowControlledStaffIdList"`
		}
		if json.Unmarshal(response.body, &result) != nil {
			return &dingtalkAPIError{Category: "unknown_send"}
		}
		if result.failed() {
			err := dingtalkHTTPError(response.status, false, response.body)
			if dingtalkTerminalAuthError(err) {
				a.invalidateToken(token)
			}
			return err
		}
		if len(result.InvalidStaffIDList) > 0 {
			return &dingtalkAPIError{Category: "target_unreachable"}
		}
		if len(result.FlowControlledStaffIDList) > 0 {
			return &dingtalkAPIError{Category: "rate_limited"}
		}
		if strings.TrimSpace(result.ProcessQueryKey) == "" {
			return &dingtalkAPIError{Category: "unknown_send"}
		}
		return nil
	}
}
