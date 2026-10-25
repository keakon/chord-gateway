package dingtalk

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type dingtalkTokenRefresh struct {
	done  chan struct{}
	token string
	err   error
}

type dingtalkTokenCache struct {
	mu      sync.Mutex
	token   string
	expires time.Time
	refresh *dingtalkTokenRefresh
	closed  bool
}

// The refresh belongs to the adapter lifecycle, not the first waiting send.
// Closing done publishes the result to all independently cancelable waiters.
func (a *dingtalkAPI) accessToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", &DingtalkAPIError{Category: "unknown_send", cause: err}
	}
	if err := a.lifecycle.Err(); err != nil {
		return "", &DingtalkAPIError{Category: "unknown_send", cause: err}
	}
	a.tokens.mu.Lock()
	if a.tokens.closed {
		a.tokens.mu.Unlock()
		return "", &DingtalkAPIError{Category: "unknown_send", cause: context.Canceled}
	}
	if a.tokens.token != "" && time.Now().Before(a.tokens.expires) {
		token := a.tokens.token
		a.tokens.mu.Unlock()
		return token, nil
	}
	refresh := a.tokens.refresh
	if refresh == nil {
		refresh = &dingtalkTokenRefresh{done: make(chan struct{})}
		a.tokens.refresh = refresh
		go a.refreshToken(refresh)
	}
	a.tokens.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", &DingtalkAPIError{Category: "unknown_send", cause: ctx.Err()}
	case <-a.lifecycle.Done():
		return "", &DingtalkAPIError{Category: "unknown_send", cause: a.lifecycle.Err()}
	case <-refresh.done:
		if err := ctx.Err(); err != nil {
			return "", &DingtalkAPIError{Category: "unknown_send", cause: err}
		}
		return refresh.token, refresh.err
	}
}

func (a *dingtalkAPI) refreshToken(refresh *dingtalkTokenRefresh) {
	ctx, cancel := context.WithTimeout(a.lifecycle, 15*time.Second)
	defer cancel()
	started := time.Now()
	response, err := a.post(ctx, "/v1.0/oauth2/accessToken", "", struct {
		AppKey    string `json:"appKey"`
		AppSecret string `json:"appSecret"`
	}{AppKey: a.cfg.ClientID, AppSecret: a.cfg.ClientSecret})
	if response.status == 401 || response.status == 403 {
		err = dingtalkHTTPError(response.status, true, response.body)
	}
	var result struct {
		dingtalkResponseError
		AccessToken string `json:"accessToken"`
		ExpireIn    int64  `json:"expireIn"`
	}
	var expires time.Time
	if err == nil {
		switch {
		case response.status < 200 || response.status >= 300:
			err = dingtalkHTTPError(response.status, true, response.body)
		case json.Unmarshal(response.body, &result) != nil:
			err = &DingtalkAPIError{Category: "auth_failed"}
		case result.failed():
			err = dingtalkHTTPError(response.status, true, response.body)
		case result.AccessToken == "" || result.ExpireIn <= 0 || result.ExpireIn > int64((time.Duration(1<<63-1))/time.Second):
			err = &DingtalkAPIError{Category: "auth_failed"}
		default:
			valid := time.Duration(result.ExpireIn) * time.Second
			expires = started.Add(valid - min(time.Minute, valid/10))
			if !expires.After(time.Now()) {
				err = &DingtalkAPIError{Category: "auth_failed"}
			}
		}
	}
	a.tokens.mu.Lock()
	if a.tokens.closed || a.lifecycle.Err() != nil {
		err = &DingtalkAPIError{Category: "unknown_send", cause: context.Canceled}
	}
	if err == nil {
		a.tokens.token, a.tokens.expires = result.AccessToken, expires
		refresh.token = result.AccessToken
	}
	refresh.err = err
	a.tokens.refresh = nil
	close(refresh.done)
	a.tokens.mu.Unlock()
}

func (a *dingtalkAPI) invalidateToken(rejected string) {
	a.tokens.mu.Lock()
	// An older in-flight response must not discard a newly refreshed token.
	if a.tokens.token == rejected {
		a.tokens.token = ""
		a.tokens.expires = time.Time{}
	}
	a.tokens.mu.Unlock()
}

// Close joins the shared refresh after the adapter has canceled its lifecycle.
func (a *dingtalkAPI) Close() {
	a.tokens.mu.Lock()
	a.tokens.closed = true
	refresh := a.tokens.refresh
	a.tokens.token = ""
	a.tokens.expires = time.Time{}
	a.tokens.mu.Unlock()
	if refresh != nil {
		<-refresh.done
	}
	if a.client != nil {
		a.client.CloseIdleConnections()
	}
}
