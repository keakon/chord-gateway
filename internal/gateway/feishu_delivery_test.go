package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	feishu "github.com/keakon/chord-gateway/internal/platform/feishu"
	"github.com/keakon/chord-gateway/internal/platform/wechat"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func TestMessageRedirectIsNotReplayed(t *testing.T) {
	for _, platform := range []string{"feishu", "wechat"} {
		t.Run(platform, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests.Add(1)
				if req.URL.Path != "/redirected" {
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				fmt.Fprint(w, `{"code":0,"ret":0,"data":{"message_id":"sample-message"}}`)
			}))
			defer server.Close()
			var err error
			if platform == "feishu" {
				oldURL := feishu.OpenBaseURL
				feishu.OpenBaseURL = server.URL
				defer func() { feishu.OpenBaseURL = oldURL }()
				a := testFeishuAdapter(t, &config.FeishuConfig{})

				a.SetTokenForTest("sample-token", time.Now().Add(time.Hour))
				a.SetHTTPClientForTest(server.Client())
				_, err = (&NotificationRouter{adapter: a}).sendCardToTarget(im.DeliveryTarget{Identity: im.ChatIdentity{Platform: "feishu", ChatID: "sample-chat"}, ProcessKey: (processKey{workspaceID: "ws", imType: "feishu", chatID: "sample-chat"}).String()}, map[string]any{}, "sample prompt")
			} else {
				a := newTestWechatAdapter(t)
				a.SetBaseURLForTest(server.URL)
				err = a.SendMessageForTest(t.Context(), a.TokenForTest(), wechat.ILinkSendMessageRequest{})
			}
			if requests.Load() != 1 || !errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
				t.Fatalf("redirect replayed or classified as rejection: requests=%d err=%v", requests.Load(), err)
			}
		})
	}
}

func TestIsAmbiguousDeliveryStatus(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{
		{http.StatusOK, false},
		{http.StatusCreated, true},
		{http.StatusAccepted, true},
		{http.StatusNoContent, true},
		{http.StatusMovedPermanently, true},
		{http.StatusTemporaryRedirect, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusRequestTimeout, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
	} {
		if got := im.IsAmbiguousDeliveryStatus(tc.code); got != tc.want {
			t.Fatalf("status %d ambiguous = %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestFeishuRejectedCardUsesTextFallback(t *testing.T) {
	for _, event := range []string{"confirm_request", "question_request", "role"} {
		t.Run(event, func(t *testing.T) {
			var types, texts []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body map[string]string
				_ = json.NewDecoder(req.Body).Decode(&body)
				types = append(types, body["msg_type"])
				if body["msg_type"] == "interactive" {
					// A 4xx is a definite platform rejection, so the card was not
					// delivered and the text entry point is still required.
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"code":230099,"msg":"Failed to create card content."}`)
					return
				}
				var content map[string]string
				_ = json.Unmarshal([]byte(body["content"]), &content)
				texts = append(texts, content["text"])
				fmt.Fprint(w, `{"code":0,"data":{"message_id":"sample-message"}}`)
			}))
			defer server.Close()
			oldURL := feishu.OpenBaseURL
			feishu.OpenBaseURL = server.URL
			defer func() { feishu.OpenBaseURL = oldURL }()
			a := testFeishuAdapter(t, &config.FeishuConfig{})

			a.SetTokenForTest("sample-token", time.Now().Add(time.Hour))
			a.SetHTTPClientForTest(server.Client())
			key := processKey{workspaceID: "test", imType: "feishu", chatID: "sample-chat"}.String()
			before := time.Now().Add(-time.Minute)
			mgr := newTestChordManager(testConfig())
			proc := &ChordProcess{state: ControlState{LastPushAt: before}}
			mgr.procs[key] = proc
			r := NewNotificationRouter(mgr)
			defer r.Close()
			r.SetAdapter(a)
			question := &QuestionPayload{RequestID: "question-1", Question: "original prompt", Options: []string{"first", "second"}}
			state := ControlState{SessionID: "session", PendingConfirm: &ConfirmPayload{RequestID: "confirm-1", ToolName: "sample-tool"}, LastQuestion: question}
			if event == "role" {
				r.showRoleMenu("sample-chat", key, "first", []RoleInfo{{Name: "first"}, {Name: "second"}}, im.IncomingMessage{IMType: "feishu"})
			} else {
				target, _ := r.targetForKey(key)
				r.deliverChordEvent(target, event, state, "original prompt")
			}
			if len(types) != 2 || types[0] != "interactive" || types[1] != "text" {
				t.Fatalf("rejected card did not use the text fallback: %v", types)
			}
			if len(texts) != 1 || strings.Contains(texts[0], "unconfirmed") {
				t.Fatalf("rejected card sent the unconfirmed notice: %v", texts)
			}
			if event != "role" && !proc.State().LastPushAt.After(before) {
				t.Fatal("fallback text did not count as visible output")
			}
		})
	}
}

func TestFeishuCardOutcomeUnknownDoesNotFallback(t *testing.T) {
	var types []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(req.Body).Decode(&body)
		types = append(types, body["msg_type"])
		fmt.Fprint(w, `{"code":0}`)
	}))
	defer server.Close()
	oldURL := feishu.OpenBaseURL
	feishu.OpenBaseURL = server.URL
	defer func() { feishu.OpenBaseURL = oldURL }()
	a := testFeishuAdapter(t, &config.FeishuConfig{})

	a.SetTokenForTest("sample-token", time.Now().Add(time.Hour))
	a.SetHTTPClientForTest(server.Client())
	if _, err := (&NotificationRouter{adapter: a}).sendCardToTarget(im.DeliveryTarget{Identity: im.ChatIdentity{Platform: "feishu", ChatID: "sample-chat"}, ProcessKey: (processKey{workspaceID: "ws", imType: "feishu", chatID: "sample-chat"}).String()}, map[string]any{}, "original prompt"); !errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
		t.Fatal("card uncertainty was swallowed", err)
	}
	if len(types) != 1 || types[0] != "interactive" {
		t.Fatalf("unexpected fallback: %v", types)
	}
}

func TestFeishuRouterCardFailureNeverReplaysPrompt(t *testing.T) {
	for _, event := range []string{"confirm_request", "question_request", "role"} {
		for _, outcome := range []string{"unknown", "rejected", "fallback_failed", "fallback_unknown", "notice_failed"} {
			t.Run(event+"/"+outcome, func(t *testing.T) {
				var types, texts []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					var body map[string]string
					_ = json.NewDecoder(req.Body).Decode(&body)
					types = append(types, body["msg_type"])
					if body["msg_type"] == "interactive" {
						if outcome == "unknown" || outcome == "notice_failed" {
							fmt.Fprint(w, `{"code":0}`)
						} else {
							fmt.Fprint(w, `{"code":123,"msg":"rejected"}`)
						}
					} else {
						var content map[string]string
						_ = json.Unmarshal([]byte(body["content"]), &content)
						texts = append(texts, content["text"])
						if outcome == "fallback_unknown" && len(texts) == 1 {
							fmt.Fprint(w, `{"code":0}`)
						} else if outcome == "fallback_failed" || outcome == "notice_failed" {
							fmt.Fprint(w, `{"code":123,"msg":"rejected"}`)
						} else {
							fmt.Fprint(w, `{"code":0,"data":{"message_id":"sample-message"}}`)
						}
					}
				}))
				defer server.Close()
				oldURL := feishu.OpenBaseURL
				feishu.OpenBaseURL = server.URL
				defer func() { feishu.OpenBaseURL = oldURL }()
				a := testFeishuAdapter(t, &config.FeishuConfig{})

				a.SetTokenForTest("sample-token", time.Now().Add(time.Hour))
				a.SetHTTPClientForTest(server.Client())
				key := processKey{workspaceID: "test", imType: "feishu", chatID: "sample-chat"}.String()
				before := time.Now().Add(-time.Minute)
				mgr := newTestChordManager(testConfig())
				proc := &ChordProcess{state: ControlState{LastPushAt: before}}
				mgr.procs[key] = proc
				r := NewNotificationRouter(mgr)
				defer r.Close()
				r.SetAdapter(a)
				question := &QuestionPayload{RequestID: "question-1", Question: "original prompt", Options: []string{"first", "second"}}
				state := ControlState{SessionID: "session", PendingConfirm: &ConfirmPayload{RequestID: "confirm-1", ToolName: "sample-tool"}, LastQuestion: question}
				if event == "role" {
					r.showRoleMenu("sample-chat", key, "first", []RoleInfo{{Name: "first"}, {Name: "second"}}, im.IncomingMessage{IMType: "feishu"})
				} else {
					target, _ := r.targetForKey(key)
					r.deliverChordEvent(target, event, state, "original prompt")
				}
				wantRequests := 2
				if outcome == "fallback_unknown" {
					wantRequests = 3
				}
				if len(types) != wantRequests || types[0] != "interactive" || types[1] != "text" || (wantRequests == 3 && types[2] != "text") {
					t.Fatalf("card or text replayed: %v", types)
				}
				if outcome == "unknown" || outcome == "notice_failed" || outcome == "fallback_unknown" {
					notice := texts[len(texts)-1]
					if !strings.Contains(notice, "unconfirmed") || strings.Contains(notice, "original prompt") {
						t.Fatalf("uncertain send repeated prompt: %v", texts)
					}
				}
				if event != "role" {
					visible := outcome == "unknown" || outcome == "rejected" || outcome == "fallback_unknown"
					if proc.State().LastPushAt.After(before) != visible {
						t.Fatal("reminder was not based on confirmed visible output")
					}
					if outcome != "rejected" && r.shownQuestions[key][question.RequestID] {
						t.Fatal("failed or uncertain question cannot be explicitly retried")
					}
				}
			})
		}
	}
}

func TestFeishuStaleEventCannotSendOrUpdateCard(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"sample-message"}}`)
	}))
	defer server.Close()
	oldURL := feishu.OpenBaseURL
	feishu.OpenBaseURL = server.URL
	defer func() { feishu.OpenBaseURL = oldURL }()
	for _, event := range []string{"confirm_request", "question_request", "question_resolved", "uncertain_notice"} {
		t.Run(event, func(t *testing.T) {
			a := testFeishuAdapter(t, &config.FeishuConfig{})

			a.SetTokenForTest("sample-token", time.Now().Add(time.Hour))
			a.SetHTTPClientForTest(server.Client())
			mgr := newTestChordManager(&config.Config{})
			r := NewNotificationRouter(mgr)
			defer r.Close()
			r.SetAdapter(a)
			key := (processKey{workspaceID: "ws", imType: "feishu", chatID: "sample-chat"}).String()
			old := &ChordProcess{bindingInstanceID: "old"}
			mgr.procs[key] = old
			target, _ := r.targetForKey(key)
			r.recordCardHandle(key, "question", "question-old", &im.InteractiveCardHandle{MessageID: "old-message"})
			mgr.removeProcessIfCurrent(key, old)
			mgr.mu.Lock()
			mgr.procs[key] = &ChordProcess{bindingInstanceID: "new"}
			mgr.mu.Unlock()
			state := ControlState{BindingInstanceID: "old", PendingConfirm: &ConfirmPayload{RequestID: "confirm-old"}, Questions: map[string]*QuestionPayload{"question-old": {RequestID: "question-old", Question: "Choose", Options: []string{"A"}}}, LastQuestionResolved: &QuestionPayload{RequestID: "question-old", Outcome: QuestionResolvedAnswered}}
			if event == "uncertain_notice" {
				r.reportFeishuCardFailure(target, im.ErrDeliveryOutcomeUnknown)
			} else {
				r.deliverChordEvent(target, event, state, "old prompt")
			}
			if requests.Load() != 0 {
				t.Fatalf("stale event made %d HTTP requests", requests.Load())
			}
		})
	}
}
