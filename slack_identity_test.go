package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func TestSlackIdentityEvidenceChain(t *testing.T) {
	for _, test := range []struct {
		name string
		auth map[string]any
		bot  map[string]any
		ok   bool
	}{
		{"matching", map[string]any{"team_id": "T1", "user_id": "UBOT", "bot_id": "B1"}, map[string]any{"id": "B1", "app_id": "A1", "user_id": "UBOT", "name": "ExampleBot"}, true},
		{"wrong team", map[string]any{"team_id": "T2", "user_id": "UBOT", "bot_id": "B1"}, nil, false},
		{"user token", map[string]any{"team_id": "T1", "user_id": "U1"}, nil, false},
		{"wrong app", map[string]any{"team_id": "T1", "user_id": "UBOT", "bot_id": "B1"}, map[string]any{"id": "B1", "app_id": "A2", "user_id": "UBOT"}, false},
		{"wrong user", map[string]any{"team_id": "T1", "user_id": "UBOT", "bot_id": "B1"}, map[string]any{"id": "B1", "app_id": "A1", "user_id": "U2"}, false},
		{"deleted", map[string]any{"team_id": "T1", "user_id": "UBOT", "bot_id": "B1"}, map[string]any{"id": "B1", "app_id": "A1", "user_id": "UBOT", "deleted": true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/auth.test" {
					result := map[string]any{"ok": true}
					for key, value := range test.auth {
						result[key] = value
					}
					_ = json.NewEncoder(w).Encode(result)
					return
				}
				if r.URL.Path != "/bots.info" {
					t.Errorf("unexpected method %s", r.URL.Path)
				}
				if err := r.ParseForm(); err != nil || r.Form.Get("bot") != "B1" || r.Form.Get("team_id") != "T1" {
					t.Error("bot evidence request not scoped to installation")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "bot": test.bot})
			}))
			defer server.Close()
			client := slack.New("test-token", slack.OptionAPIURL(server.URL+"/"), slack.OptionHTTPClient(newSlackHTTPClient(server.Client(), nil)))
			identity, err := verifySlackIdentity(t.Context(), client, "T1", "A1")
			if test.ok {
				if err != nil {
					t.Fatal(err)
				}
				if identity.BotName != "ExampleBot" {
					t.Fatal("bot display name missing", identity.BotName)
				}
				parts, err := DecodeScopedID(identity.AccountID, "account-v1:", 2)
				if err != nil || parts[0] != "T1" || parts[1] != "UBOT" {
					t.Fatalf("invalid account identity: %v %v", parts, err)
				}
			} else if !errors.Is(err, errSlackFatal) {
				t.Fatalf("identity mismatch was not fatal: %v", err)
			}
		})
	}
}

func TestSlackIdentityRejectsUnconfirmedHTTP200Evidence(t *testing.T) {
	for _, method := range []string{"auth.test", "bots.info"} {
		for _, invalidOK := range []any{nil, false, "true"} {
			t.Run(fmt.Sprintf("%s_ok_%v", method, invalidOK), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body := map[string]any{"ok": true, "team_id": "T1", "user_id": "UBOT", "bot_id": "B1"}
					if r.URL.Path == "/bots.info" {
						body = map[string]any{"ok": true, "bot": map[string]any{"id": "B1", "app_id": "A1", "user_id": "UBOT"}}
					}
					if r.URL.Path == "/"+method {
						if invalidOK == nil {
							delete(body, "ok")
						} else {
							body["ok"] = invalidOK
						}
					}
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer server.Close()
				client := slack.New("test-token", slack.OptionAPIURL(server.URL+"/"), slack.OptionHTTPClient(newSlackHTTPClient(server.Client(), nil)))
				if _, err := verifySlackIdentity(t.Context(), client, "T1", "A1"); err == nil {
					t.Fatal("accepted identity fields without confirmed API receipt")
				}
			})
		}
	}
}

func TestSlackAPIErrorsAreRedactedAndClassified(t *testing.T) {
	for _, code := range []string{"invalid_auth", "account_inactive", "token_revoked", "not_authed"} {
		if err := slackAPIError("auth.test", slack.SlackErrorResponse{Err: code}); !errors.Is(err, errSlackFatal) {
			t.Fatal(err)
		}
	}
	err := slackAPIError("auth.test", errors.New("dial https://private.invalid/?token=test-secret"))
	if strings.Contains(err.Error(), "test-secret") || strings.Contains(err.Error(), "private.invalid") {
		t.Fatal("SDK transport details leaked")
	}
	if !errors.Is(slackAPIError("auth.test", context.Canceled), context.Canceled) {
		t.Fatal("cancellation identity lost")
	}
	wrapped := &url.Error{Op: "Post", URL: "https://private.invalid/?token=test-secret", Err: context.DeadlineExceeded}
	if err := slackAPIError("auth.test", wrapped); strings.Contains(err.Error(), "test-secret") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cancelled API request leaked transport URL", err)
	}
}

func TestSlackTargetAndTimestampPreserveExactStrings(t *testing.T) {
	for _, channel := range []string{"C123", "G123", "D123"} {
		target, err := slackTarget(channel, "1760000000.000001")
		if err != nil {
			t.Fatal(err)
		}
		gotChannel, ts, err := decodeSlackTarget(target)
		if err != nil || gotChannel != channel || ts != "1760000000.000001" {
			t.Fatalf("target round trip: %s %s %v", gotChannel, ts, err)
		}
	}
	for _, target := range []string{"U123", "C123:1760000000.1", "", "Dlower", "C1\n"} {
		if _, _, err := decodeSlackTarget(target); err == nil {
			t.Fatalf("accepted unsupported target %q", target)
		}
	}
	for _, ts := range []string{"1e9", "1760000000", "1760000000.0000000001", "1760000000.-1", "0.1"} {
		if validSlackTimestamp(ts) {
			t.Fatalf("accepted malformed ts %q", ts)
		}
	}
	got, err := slackSourceTime("1760000000.000001")
	if err != nil || !got.Equal(time.Unix(1760000000, 1000)) {
		t.Fatal(got, err)
	}
}
