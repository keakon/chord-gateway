// Access token lifecycle: cache invalidation, refresh and the
// account-identity checks that guard a live connection.
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/keakon/golog/log"
)

// --- Access token management ---

func (a *FeishuAdapter) invalidateAccessToken() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accessToken = ""
	a.tokenExpireAt = time.Time{}
}

func (a *FeishuAdapter) connectionContext() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *FeishuAdapter) matchesActiveAccount(account string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.stopped && (a.activeAccount == "" || a.activeAccount == account)
}

func (a *FeishuAdapter) getAccessToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fc := a.feishuConfig()
	if fc == nil || !a.matchesActiveAccount(fc.AppID) {
		return "", fmt.Errorf("feishu: account config missing or changed")
	}
	a.mu.Lock()
	if a.accessToken != "" && time.Now().Before(a.tokenExpireAt) {
		token := a.accessToken
		a.mu.Unlock()
		return token, nil
	}
	refresh := a.tokenRefresh
	if refresh == nil {
		refresh = &feishuTokenRefresh{done: make(chan struct{})}
		a.tokenRefresh = refresh
		parent := a.ctx
		if parent == nil {
			parent = context.Background()
		}
		// A caller can stop waiting without cancelling the refresh shared by
		// other callers. Shutdown and a short deadline bound the refresh itself.
		go func() {
			refreshCtx, cancel := context.WithTimeout(parent, 10*time.Second)
			defer cancel()
			refresh.token, refresh.expireAt, refresh.err = a.fetchAccessToken(refreshCtx)
			a.mu.Lock()
			if refresh.err == nil {
				a.accessToken = refresh.token
				a.tokenExpireAt = refresh.expireAt
			}
			close(refresh.done)
			a.tokenRefresh = nil
			a.mu.Unlock()
		}()
	}
	a.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-refresh.done:
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return refresh.token, refresh.err
	}
}

func (a *FeishuAdapter) fetchAccessToken(ctx context.Context) (string, time.Time, error) {
	fc := a.feishuConfig()
	if fc == nil || !a.matchesActiveAccount(fc.AppID) {
		return "", time.Time{}, fmt.Errorf("feishu: account config missing or changed")
	}
	body, err := json.Marshal(map[string]string{
		"app_id":     fc.AppID,
		"app_secret": fc.AppSecret,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal feishu token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		OpenBaseURL+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create feishu token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("feishu token HTTP request: %w", err)
	}
	defer resp.Body.Close()

	var result FeishuTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, fmt.Errorf("feishu decode token response: %w", err)
	}

	if result.Code != 0 {
		return "", time.Time{}, fmt.Errorf("feishu token error: code=%d", result.Code)
	}

	// Refresh 5 minutes before actual expiry.
	expiry := result.Expire - 300
	if expiry <= 0 {
		expiry = result.Expire
	}
	expireAt := time.Now().Add(time.Duration(expiry) * time.Second)

	log.Infof("feishu: access token refreshed expire=%v", result.Expire)
	return result.TenantAccessToken, expireAt, nil
}
