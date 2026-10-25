package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

type slackHintCaptureAdapter struct{ pairingTestAdapter }

func (*slackHintCaptureAdapter) Type() string { return "slack" }

func slackHintTestRouter(t *testing.T, chat string, bound bool) (*NotificationRouter, *slackHintCaptureAdapter, im.IncomingMessage) {
	t.Helper()
	bindings := "{}"
	if bound {
		bindings = fmt.Sprintf("{%q: project}", chat)
	}
	store := testConfigStore(t, fmt.Sprintf(`ims:
  slack:
    app_token: app-token-placeholder
    bot_token: bot-token-placeholder
    app_id: app-example
    team_id: team-example
    owner: {account_id: account-example, user_id: owner-example}
    chat_bindings: %s
workspaces: {project: {path: %q}}
`, bindings, t.TempDir()))
	mgr := NewChordManager(store.Snapshot(), &config.Paths{StateDir: t.TempDir()})
	mgr.store = store
	r := NewNotificationRouter(mgr)
	pairing := NewPairingService(store, false, nil)
	r.admission = NewAdmission(pairing)
	mgr.admission = r.admission
	adapter := &slackHintCaptureAdapter{}
	r.SetAdapter(adapter)
	t.Cleanup(func() { r.admission.Close(); r.Close() })
	if err := pairing.PlatformReadyWithPresentation("slack", "account-example", im.PlatformPresentation{Platform: "slack", BotMention: "<@bot-example>", BotUsername: "ExampleBot"}); err != nil {
		t.Fatal(err)
	}
	msg := im.IncomingMessage{IMType: "slack", AccountID: "account-example", ChatID: chat, ChatKind: "group", SenderID: "owner-example", MessageID: "event-example"}
	return r, adapter, msg
}

func requireSlackHintReply(t *testing.T, adapter *slackHintCaptureAdapter, want ...string) {
	t.Helper()
	waitIngressCondition(t, func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		for _, text := range adapter.messages {
			matches := true
			for _, part := range want {
				if !strings.Contains(text, part) {
					matches = false
				}
			}
			if matches {
				return true
			}
		}
		return false
	})
}

func TestSlackGeneratedRuntimeHints(t *testing.T) {
	thread, err := im.EncodeScopedID("target-v1:", "slack-thread", "channel-example", "123.000001")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, chat, prefix string }{{"root", "channel-example", "/chord "}, {"thread", thread, "@ExampleBot /"}} {
		t.Run(tc.name+"/unbound help", func(t *testing.T) {
			r, adapter, msg := slackHintTestRouter(t, tc.chat, false)
			msg.Text = "/help"
			ticket, ok := r.admission.Authorize(msg)
			if !ok {
				t.Fatal("help not authorized")
			}
			msg.SetAdmissionTicket(&ticket)
			if result := r.DispatchAccepted(t.Context(), msg, nil); result.State != "completed" {
				t.Fatal(result)
			}
			requireSlackHintReply(t, adapter, tc.prefix+"bind <directory>", tc.prefix+"status")
		})
		t.Run(tc.name+"/role without session", func(t *testing.T) {
			r, adapter, msg := slackHintTestRouter(t, tc.chat, true)
			msg.Text = "/role"
			ticket, ok := r.admission.Authorize(msg)
			if !ok {
				t.Fatal("role not authorized")
			}
			msg.SetAdmissionTicket(&ticket)
			if result := r.DispatchAccepted(t.Context(), msg, nil); result.State != "completed" {
				t.Fatal(result)
			}
			requireSlackHintReply(t, adapter, tc.prefix+"new", tc.prefix+"role")
		})
		t.Run(tc.name+"/MCP missing status", func(t *testing.T) {
			r, adapter, msg := slackHintTestRouter(t, tc.chat, true)
			msg.Text = "/mcp status"
			ticket, ok := r.admission.Authorize(msg)
			if !ok {
				t.Fatal("MCP not authorized")
			}
			msg.SetAdmissionTicket(&ticket)
			if result := r.dispatchIngressSlash(t.Context(), msg, &ChordProcess{}, im.IMCommand{Content: "/mcp status"}, nil, ""); result.State != "completed" {
				t.Fatal(result)
			}
			requireSlackHintReply(t, adapter, tc.prefix+"status")
		})
		t.Run(tc.name+"/question capacity", func(t *testing.T) {
			r, _, _ := slackHintTestRouter(t, tc.chat, true)
			r.questionOperations = make(map[string]pendingQuestionOperation)
			for i := 0; i < maxQuestionOperations; i++ {
				r.questionOperations[fmt.Sprint(i)] = pendingQuestionOperation{expiresAt: time.Now().Add(time.Hour)}
			}
			key := (processKey{workspaceID: "project", imType: "slack", chatID: tc.chat}).String()
			_, err := r.waitQuestionOperationTarget(context.Background(), key, "owner-example", nil, map[string]any{"action": "answer", "request_id": "question-example"}, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.prefix+"status") {
				t.Fatal(err)
			}
		})
	}
}

func TestSlackGeneratedQuestionAndRoleHintsPreserveContent(t *testing.T) {
	r := &NotificationRouter{}
	roles := []RoleInfo{{Name: "builder"}, {Name: "Keep /role in content"}}
	for _, thread := range []bool{false, true} {
		presentation := im.PlatformPresentation{Platform: "slack", BotMention: "<@bot-example>", BotUsername: "ExampleBot"}
		hint := func(command string) string {
			return im.FormatCommandHint(presentation, im.CommandTarget{Thread: thread}, command)
		}
		state := ControlState{LastQuestionResolved: &QuestionPayload{RequestID: "question-example", Outcome: QuestionResolvedNoResponse}}
		if got := r.formatNotification("question_resolved", state, hint); !strings.Contains(got, hint("/answer @question-example <answer>")) {
			t.Fatal(got)
		}
		menu := buildRoleMenuText("builder", roles, hint)
		if !strings.Contains(menu, "Keep /role in content") || !strings.Contains(menu, hint("/role <number>")) || !strings.Contains(menu, hint("/role <name>")) {
			t.Fatal(menu)
		}
		if _, err := selectRoleName("99", roles, false, hint); err == nil || !strings.Contains(err.Error(), hint("/role")) {
			t.Fatal(err)
		}
		if _, err := selectRoleName("missing", roles, false, hint); err == nil || !strings.Contains(err.Error(), hint("/role")) {
			t.Fatal(err)
		}
		r.SetAdapter(&stubIMAdapter{typ: "wechat", supportsLoginRenewal: true})
		if got, _ := r.loginReply("", hint); !strings.Contains(got, hint("/login <platform>")) {
			t.Fatal(got)
		}
	}
}
