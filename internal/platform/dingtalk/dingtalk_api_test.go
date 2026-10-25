package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/buildinfo"
)

func dingtalkTestAPI(t *testing.T, handler http.HandlerFunc) (*dingtalkAPI, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(handler)
	a := newDingtalkAPI(ctx, &config.DingtalkConfig{ClientID: "test-client", ClientSecret: "test-secret", RobotCode: "test-robot", CorpID: "test-corp"}, nil)
	a.baseURL = server.URL
	a.accountInterval = 0
	a.targetInterval = 0
	a.retryBase = time.Millisecond
	t.Cleanup(func() { cancel(); a.Close(); server.Close() })
	return a, cancel
}

func dingtalkTestCategory(t *testing.T, err error, want string) {
	t.Helper()
	var e *DingtalkAPIError
	if !errors.As(err, &e) || e.Category != want {
		t.Fatalf("error = %v, want category %q", err, want)
	}
}

func TestDingtalkAPIActiveTargetsAndPayload(t *testing.T) {
	var mu sync.Mutex
	var received []struct {
		Path string
		Body map[string]any
	}
	a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path == "/v1.0/oauth2/accessToken" {
			if body["appKey"] != "test-client" || body["appSecret"] != "test-secret" {
				t.Error("token request credentials do not match config")
			}
			WriteTokenForTest(w)
			return
		}
		if r.Header.Get("x-acs-dingtalk-access-token") != "test-token" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing expected request headers")
		}
		mu.Lock()
		received = append(received, struct {
			Path string
			Body map[string]any
		}{r.URL.Path, body})
		mu.Unlock()
		_, _ = w.Write([]byte(`{"processQueryKey":"test-query"}`))
	})
	for _, target := range []struct{ kind, id string }{{"private", "staff-original"}, {"group", "conversation-original"}} {
		if err := a.SendText(context.Background(), target.kind, target.id, "Response \"with quotes\"\n你好😀"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 || received[0].Path != "/v1.0/robot/oToMessages/batchSend" || received[1].Path != "/v1.0/robot/groupMessages/send" {
		t.Fatalf("unexpected paths: %+v", received)
	}
	if !reflect.DeepEqual(received[0].Body["userIds"], []any{"staff-original"}) || received[1].Body["openConversationId"] != "conversation-original" {
		t.Fatal("raw staff/conversation ID was not preserved")
	}
	for _, request := range received {
		if request.Body["robotCode"] != "test-robot" || request.Body["msgKey"] != "sampleText" {
			t.Fatalf("unexpected message payload: %+v", request.Body)
		}
		param, ok := request.Body["msgParam"].(string)
		if !ok {
			t.Fatal("msgParam must be a JSON string")
		}
		var content struct {
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(param), &content) != nil || content.Content != "Response \"with quotes\"\n你好😀" {
			t.Fatalf("unexpected msgParam: %q", param)
		}
	}
}

func TestDingtalkAPIResponseClassificationAndRetryBound(t *testing.T) {
	cases := []struct {
		name, body, category string
		status, calls        int
	}{
		{"unauthorized", `{"message":"test-secret test-token"}`, "auth_failed", 401, 1},
		{"forbidden", `{"code":"Forbidden.AccessDenied"}`, "proactive_send_denied", 403, 1},
		{"rate-limit", `{"message":"limited"}`, "rate_limited", 429, 4},
		{"server-error", `{"message":"test-token"}`, "unknown_send", 503, 1},
		{"empty-success", `{}`, "unknown_send", 200, 1},
		{"blank-key", `{"processQueryKey":" "}`, "unknown_send", 200, 1},
		{"invalid-staff", `{"processQueryKey":"test-query","invalidStaffIdList":["test-staff"]}`, "target_unreachable", 200, 1},
		{"flow-controlled-staff", `{"processQueryKey":"test-query","flowControlledStaffIdList":["test-staff"]}`, "rate_limited", 200, 1},
		{"explicit-error", `{"processQueryKey":"test-query","code":"Forbidden.AccessDenied"}`, "proactive_send_denied", 200, 1},
		{"explicit-unsuccessful", `{"processQueryKey":"test-query","success":false}`, "unknown_send", 200, 1},
		{"invalid-json", `{"processQueryKey":`, "unknown_send", 200, 1},
		{"target-not-found", `{}`, "target_unreachable", 404, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1.0/oauth2/accessToken" {
					WriteTokenForTest(w)
					return
				}
				sends.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			err := a.SendText(context.Background(), "private", "test-staff", "Response")
			dingtalkTestCategory(t, err, tc.category)
			if got := int(sends.Load()); got != tc.calls {
				t.Fatalf("send calls = %d, want %d", got, tc.calls)
			}
			for _, sensitive := range []string{"test-secret", "test-token", "test-staff", a.baseURL} {
				if strings.Contains(err.Error(), sensitive) {
					t.Fatalf("error includes sensitive data: %v", err)
				}
			}
		})
	}
}

func TestDingtalkAPIUnauthorizedClearsTokenWithoutResend(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var tokens, sends atomic.Int32
			a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1.0/oauth2/accessToken" {
					tokens.Add(1)
					WriteTokenForTest(w)
					return
				}
				if sends.Add(1) == 1 {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{}`))
				} else {
					_, _ = w.Write([]byte(`{"processQueryKey":"test-query"}`))
				}
			})
			if err := a.SendText(context.Background(), "private", "test-staff", "First response"); err == nil {
				t.Fatal("first send must be rejected")
			}
			if sends.Load() != 1 || tokens.Load() != 1 {
				t.Fatal("unauthorized message was retried")
			}
			if err := a.SendText(context.Background(), "private", "test-staff", "New response"); err != nil {
				t.Fatal(err)
			}
			if tokens.Load() != 2 || sends.Load() != 2 {
				t.Fatal("next independent send must refresh token")
			}
		})
	}
}

func TestDingtalkAPIRetryAfterHonorsDeadline(t *testing.T) {
	var sends atomic.Int32
	a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/oauth2/accessToken" {
			WriteTokenForTest(w)
			return
		}
		sends.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := a.SendText(ctx, "private", "test-staff", "Response")
	dingtalkTestCategory(t, err, "rate_limited")
	if !errors.Is(err, context.DeadlineExceeded) || sends.Load() != 1 {
		t.Fatalf("retry ignored current send deadline: %v, calls=%d", err, sends.Load())
	}
}

func TestDingtalkAPIRateLimitsAccountAndTarget(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/oauth2/accessToken" {
			WriteTokenForTest(w)
			return
		}
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		_, _ = w.Write([]byte(`{"processQueryKey":"test-query"}`))
	})
	a.accountInterval = 20 * time.Millisecond
	a.targetInterval = 60 * time.Millisecond
	for _, staff := range []string{"staff-one", "staff-two", "staff-one"} {
		if err := a.SendText(context.Background(), "private", staff, "Response"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 3 || at[1].Sub(at[0]) < 18*time.Millisecond || at[2].Sub(at[0]) < 58*time.Millisecond {
		t.Fatalf("account/target intervals were not enforced: %+v", at)
	}
}

func TestDingtalkAPIRateLimitWaitCancellation(t *testing.T) {
	a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, _ *http.Request) { WriteTokenForTest(w) })
	a.accountNext = time.Now().Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.waitSendSlot(ctx, "private:test-staff"); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v", err)
	}
	if len(a.targetNext) != 0 {
		t.Fatal("canceled waiter consumed a target slot")
	}
}

func TestDingtalkAPIRegistrationPayloadAndEndpointValidation(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, ticket string
		valid                  bool
	}{
		{"valid", "wss://stream.example.test/connect", "test-ticket", true},
		{"plaintext", "ws://stream.example.test/connect", "test-ticket", false},
		{"userinfo", "wss://user@stream.example.test/connect", "test-ticket", false},
		{"fragment", "wss://stream.example.test/connect#test", "test-ticket", false},
		{"missing-host", "wss:///connect", "test-ticket", false},
		{"missing-ticket", "wss://stream.example.test/connect", "", false},
		{"malformed", "://bad", "test-ticket", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if json.NewDecoder(r.Body).Decode(&payload) != nil || r.URL.Path != "/v1.0/gateway/connections/open" {
					t.Error("invalid registration request")
				}
				if payload["clientId"] != "test-client" || payload["clientSecret"] != "test-secret" || payload["ua"] != "chord-gateway-go/"+buildinfo.Current().Short() {
					t.Error("registration payload mismatch")
				}
				if _, exists := payload["localIp"]; exists {
					t.Error("localIp must not be sent")
				}
				want := []any{map[string]any{"type": "CALLBACK", "topic": "/v1.0/im/bot/messages/get"}}
				if !reflect.DeepEqual(payload["subscriptions"], want) {
					t.Error("unexpected subscriptions")
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"endpoint": tc.endpoint, "ticket": tc.ticket})
			})
			endpoint, ticket, err := a.openConnection(context.Background())
			if tc.valid {
				if err != nil || endpoint != tc.endpoint || ticket != tc.ticket {
					t.Fatalf("registration = %q, %q, %v", endpoint, ticket, err)
				}
			} else {
				dingtalkTestCategory(t, err, "auth_failed")
				if !dingtalkTerminalAuthError(err) || endpoint != "" || ticket != "" {
					t.Fatal("invalid registration must terminate without exposing endpoint/ticket")
				}
			}
		})
	}
}

func TestDingtalkAPISecretsNeverFollowRedirect(t *testing.T) {
	var followed atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { followed.Add(1); WriteTokenForTest(w) }))
	defer other.Close()
	a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/secret", http.StatusTemporaryRedirect)
	})
	_, err := a.accessToken(context.Background())
	dingtalkTestCategory(t, err, "unknown_send")
	if followed.Load() != 0 || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), other.URL) {
		t.Fatalf("redirect leaked credential request: %v", err)
	}
}

func TestDingtalkAPIResponseSizeBound(t *testing.T) {
	a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/oauth2/accessToken" {
			WriteTokenForTest(w)
			return
		}
		_, _ = w.Write([]byte(strings.Repeat("a", dingtalkAPIMaxResponse+1)))
	})
	dingtalkTestCategory(t, a.SendText(context.Background(), "private", "test-staff", "Response"), "unknown_send")
}

func TestDingtalkPreSendFailureIsNotUnknownDelivery(t *testing.T) {
	t.Run("token", func(t *testing.T) {
		a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
		err := a.SendText(t.Context(), "private", "sample-staff", "output")
		var failure *DingtalkAPIError
		if !errors.As(err, &failure) || failure.IMTextSendFailureCategory() == "unknown_send" {
			t.Fatalf("token acquisition=%v", err)
		}
	})
	t.Run("pacing", func(t *testing.T) {
		a, _ := dingtalkTestAPI(t, func(_ http.ResponseWriter, _ *http.Request) { t.Error("send reached network") })
		a.tokens.token, a.tokens.expires = "sample-token", time.Now().Add(time.Hour)
		a.accountNext = time.Now().Add(time.Minute)
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		defer cancel()
		err := a.SendText(ctx, "private", "sample-staff", "output")
		var failure *DingtalkAPIError
		if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &failure) && failure.IMTextSendFailureCategory() == "unknown_send" {
			t.Fatalf("pacing=%v", err)
		}
	})
}
