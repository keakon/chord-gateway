package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

func testConfig() *config.Config {
	return &config.Config{
		IM: config.IMConfig{Type: "wechat"},
		Workspaces: []config.Workspace{
			{ID: "test", Path: "/tmp/test", IMChatID: "chat-1"},
		},
	}
}

func newTestRouter() *NotificationRouter {
	return &NotificationRouter{
		cfg:           testConfig(),
		lastKeyChatID: make(map[string]string),
		lastTodos:     make(map[string][]TodoItem),
	}
}

// ---------------------------------------------------------------------------
// parseIMCommand
// ---------------------------------------------------------------------------

func TestParseIMCommand(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantType      string
		wantContent   string
		wantRequestID string
		wantAction    string
		wantAnswers   []string
		wantSessionID string
	}{
		{name: "status", input: "/status", wantType: "status"},
		{name: "summary", input: "/summary", wantType: "send", wantContent: "/summary"},
		{name: "cancel", input: "/cancel", wantType: "cancel"},
		{name: "allow with request_id", input: "/allow req-1", wantType: "confirm", wantAction: "allow", wantRequestID: "req-1"},
		{name: "deny with request_id", input: "/deny req-2", wantType: "confirm", wantAction: "deny", wantRequestID: "req-2"},
		{name: "answer", input: "/answer yes", wantType: "question", wantAnswers: []string{"yes"}},
		{name: "new", input: "/new", wantType: "new"},
		{name: "resume with session_id", input: "/resume 123", wantType: "resume", wantSessionID: "123"},
		{name: "sessions", input: "/sessions", wantType: "sessions"},
		{name: "current", input: "/current", wantType: "current"},
		{name: "todos", input: "/todos", wantType: "todos"},
		{name: "plain text becomes send", input: "hello world", wantType: "send", wantContent: "hello world"},
		{name: "unknown slash command becomes send", input: "/unknown", wantType: "send", wantContent: "/unknown"},
		{name: "allow without request_id", input: "/allow", wantType: "confirm", wantAction: "allow", wantRequestID: ""},
		{name: "login without target", input: "/login", wantType: "login", wantContent: ""},
		{name: "login weixin", input: "/login weixin", wantType: "login", wantContent: "weixin"},
		{name: "login feishu", input: "/login feishu", wantType: "login", wantContent: "feishu"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIMCommand(tt.input)
			if got.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tt.wantType)
			}
			if got.Content != tt.wantContent {
				t.Errorf("Content = %q, want %q", got.Content, tt.wantContent)
			}
			if got.RequestID != tt.wantRequestID {
				t.Errorf("RequestID = %q, want %q", got.RequestID, tt.wantRequestID)
			}
			if got.Action != tt.wantAction {
				t.Errorf("Action = %q, want %q", got.Action, tt.wantAction)
			}
			if tt.wantAnswers != nil {
				if len(got.Answers) != len(tt.wantAnswers) {
					t.Errorf("Answers = %v, want %v", got.Answers, tt.wantAnswers)
				} else {
					for i := range tt.wantAnswers {
						if got.Answers[i] != tt.wantAnswers[i] {
							t.Errorf("Answers[%d] = %q, want %q", i, got.Answers[i], tt.wantAnswers[i])
						}
					}
				}
			}
			if got.SessionID != tt.wantSessionID {
				t.Errorf("SessionID = %q, want %q", got.SessionID, tt.wantSessionID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// formatStatus
// ---------------------------------------------------------------------------

func TestFormatStatus(t *testing.T) {
	t.Run("busy shows spinner", func(t *testing.T) {
		s := formatStatus(ControlState{Busy: true})
		if !containsEmoji(s, "🔄") {
			t.Errorf("expected 🔄 in busy status, got: %s", s)
		}
	})

	t.Run("idle shows pause", func(t *testing.T) {
		s := formatStatus(ControlState{Busy: false})
		if !containsEmoji(s, "⏸️") {
			t.Errorf("expected ⏸️ in idle status, got: %s", s)
		}
	})

	t.Run("pending confirm shows wrench", func(t *testing.T) {
		s := formatStatus(ControlState{
			Busy:           true,
			PendingConfirm: &ConfirmPayload{ToolName: "Bash", RequestID: "r1"},
		})
		if !containsEmoji(s, "🔧") {
			t.Errorf("expected 🔧 with pending confirm, got: %s", s)
		}
	})

	t.Run("pending question shows question mark", func(t *testing.T) {
		s := formatStatus(ControlState{
			Busy:            false,
			PendingQuestion: &QuestionPayload{Question: "Continue?", RequestID: "r2"},
		})
		if !containsEmoji(s, "❓") {
			t.Errorf("expected ❓ with pending question, got: %s", s)
		}
	})

	t.Run("last error shows cross", func(t *testing.T) {
		s := formatStatus(ControlState{
			Busy:      false,
			LastError: "something broke",
		})
		if !containsEmoji(s, "❌") {
			t.Errorf("expected ❌ with last error, got: %s", s)
		}
	})
}

func containsEmoji(s, emoji string) bool {
	return strings.Contains(s, emoji)
}

func TestConfiguredHeadlessSubscribeEvents(t *testing.T) {
	got := configuredHeadlessSubscribeEvents(&config.Config{})
	wantCore := []string{"assistant_message", "confirm_request", "question_request", "idle", "error", "notification"}
	if strings.Join(got, ",") != strings.Join(wantCore, ",") {
		t.Fatalf("default subscribe events = %v, want %v", got, wantCore)
	}

	got = configuredHeadlessSubscribeEvents(&config.Config{EventVisibility: config.EventVisibility{
		Activity:   true,
		AgentDone:  true,
		Info:       true,
		Toast:      true,
		ToolResult: true,
		Todos:      true,
	}})
	wantAll := []string{"assistant_message", "confirm_request", "question_request", "idle", "error", "notification", "activity", "agent_done", "info", "toast", "tool_result", "todos"}
	if strings.Join(got, ",") != strings.Join(wantAll, ",") {
		t.Fatalf("configured subscribe events = %v, want %v", got, wantAll)
	}
}

// ---------------------------------------------------------------------------
// formatNotification
// ---------------------------------------------------------------------------

func TestFormatNotification_NotificationEvent(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		msg    string
		emoji  string
	}{
		{name: "idle", reason: "idle", msg: "Chord: Ready for input", emoji: "✅"},
		{name: "error", reason: "error", msg: "something failed", emoji: "⚠️"},
		{name: "cancelled", reason: "cancelled", msg: "Chord: Ready for input", emoji: "⚠️"},
		{name: "confirm", reason: "confirm_request", msg: "Chord: Permission confirmation required", emoji: "🔧"},
		{name: "question", reason: "question_request", msg: "Chord: Question requires your input", emoji: "❓"},
		{name: "plain", reason: "other", msg: "plain text", emoji: "plain text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRouter()
			msg := r.formatNotification("test", "notification", ControlState{
				LastNotification: &NotificationPayload{Reason: tt.reason, Message: tt.msg},
			})
			if msg == "" {
				t.Fatal("notification should push")
			}
			if !strings.Contains(msg, tt.emoji) {
				t.Fatalf("expected %s in notification, got: %s", tt.emoji, msg)
			}
		})
	}
}

func TestFormatNotification_StateEventsDoNotDuplicate(t *testing.T) {
	r := newTestRouter()
	if msg := r.formatNotification("test", "confirm_request", ControlState{PendingConfirm: &ConfirmPayload{ToolName: "Bash"}}); msg == "" {
		t.Fatal("confirm_request should push")
	}
	if msg := r.formatNotification("test", "question_request", ControlState{PendingQuestion: &QuestionPayload{Question: "Continue?"}}); msg == "" {
		t.Fatal("question_request should push")
	}
	if msg := r.formatNotification("test", "error", ControlState{LastError: "boom"}); msg != "" {
		t.Fatalf("error = %q, want empty", msg)
	}
	if msg := r.formatNotification("test", "idle", ControlState{LastOutcome: "completed"}); msg != "" {
		t.Fatalf("idle = %q, want empty", msg)
	}
	if msg := r.formatNotification("test", "agent_done", ControlState{}); msg != "" {
		t.Fatalf("agent_done = %q, want empty", msg)
	}
	if msg := r.formatNotification("test", "todos", ControlState{}); msg != "" {
		t.Fatalf("todos = %q, want empty", msg)
	}
	if msg := r.formatNotification("test", "unknown", ControlState{}); msg != "" {
		t.Fatalf("unknown = %q, want empty", msg)
	}
}

func TestFormatBindingStatus(t *testing.T) {
	ws := &config.Workspace{ID: "project-a", Path: "/tmp/project-a"}
	msg := formatBindingStatus(ws, "feishu", "oc_chat_a", ControlState{
		Busy:            true,
		SessionID:       "sess-1",
		Phase:           "planning",
		PhaseDetail:     "writing plan",
		LastOutcome:     "completed",
		PendingConfirm:  &ConfirmPayload{ToolName: "Bash"},
		PendingQuestion: &QuestionPayload{Question: "Continue?"},
		LastToolResult:  &ToolResultInfo{Name: "Read", Status: "success"},
		Todos: []TodoItem{
			{ID: "1", Content: "A", Status: "completed"},
			{ID: "2", Content: "B", Status: "pending"},
		},
	})
	for _, want := range []string{"project-a", "oc_chat_a", "sess-1", "planning", "completed", "Pending confirm", "Pending question", "Last tool", "1/2 completed"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected %q in %q", want, msg)
		}
	}
}

func TestWorkspaceDisplayName(t *testing.T) {
	if got := workspaceDisplayName(nil); got != "(unknown)" {
		t.Fatalf("nil workspace = %q", got)
	}
	if got := workspaceDisplayName(&config.Workspace{Path: "/tmp/project/"}); got != "project" {
		t.Fatalf("trimmed path = %q", got)
	}
	if got := workspaceDisplayName(&config.Workspace{Path: "/"}); got != "/" {
		t.Fatalf("root path = %q", got)
	}
}

func TestFormatConfirmNotification(t *testing.T) {
	r := newTestRouter()

	t.Run("includes tool args summary for Bash", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{
				ToolName:  "Bash",
				ArgsJSON:  `{"command":"ls -la"}`,
				RequestID: "abc123",
			},
		})
		if !strings.Contains(msg, "$ ls -la") {
			t.Errorf("expected command summary, got: %s", msg)
		}
	})

	t.Run("includes tool args summary for Write", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{
				ToolName:  "Write",
				ArgsJSON:  `{"path":"src/main.go"}`,
				RequestID: "def456",
			},
		})
		if !strings.Contains(msg, "📝 src/main.go") {
			t.Errorf("expected path summary, got: %s", msg)
		}
	})

	t.Run("includes approval bullets", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{
				ToolName:      "Bash",
				ArgsJSON:      `{"command":"echo hi"}`,
				NeedsApproval: []string{"filesystem", "network"},
			},
		})
		for _, want := range []string{"filesystem", "network"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("expected %q in %q", want, msg)
			}
		}
	})

	t.Run("does not include request ID", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{
				ToolName:  "Bash",
				ArgsJSON:  `{"command":"echo hi"}`,
				RequestID: "hex123abc",
			},
		})
		if strings.Contains(msg, "hex123abc") {
			t.Errorf("request ID should not appear in notification, got: %s", msg)
		}
	})

	t.Run("reply prompt is simple", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{
				ToolName: "Bash",
				ArgsJSON: `{"command":"echo hi"}`,
			},
		})
		if !strings.Contains(msg, "Reply /allow or /deny") {
			t.Errorf("expected simple reply prompt, got: %s", msg)
		}
	})

	t.Run("empty ArgsJSON does not crash", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{
			PendingConfirm: &ConfirmPayload{ToolName: "Bash"},
		})
		if !strings.Contains(msg, "Bash") {
			t.Errorf("expected tool name, got: %s", msg)
		}
	})

	t.Run("nil PendingConfirm returns empty", func(t *testing.T) {
		msg := r.formatConfirmNotification(ControlState{})
		if msg != "" {
			t.Errorf("expected empty, got: %s", msg)
		}
	})
}

func TestSummarizeToolArgs(t *testing.T) {
	long := strings.Repeat("x", 350)
	tests := []struct {
		name     string
		toolName string
		argsJSON string
		contains []string
	}{
		{name: "empty", toolName: "Bash", argsJSON: "", contains: []string{""}},
		{name: "invalid json", toolName: "Bash", argsJSON: "{not-json", contains: []string{"{not-json"}},
		{name: "spawn command", toolName: "Spawn", argsJSON: `{"command":"echo hi"}`, contains: []string{"$ echo hi"}},
		{name: "edit path", toolName: "Edit", argsJSON: `{"path":"a.go"}`, contains: []string{"📝 a.go"}},
		{name: "delete paths", toolName: "Delete", argsJSON: `{"paths":["a.go","b.go"]}`, contains: []string{"🗑️ a.go"}},
		{name: "read path", toolName: "Read", argsJSON: `{"path":"README.md"}`, contains: []string{"📖 README.md"}},
		{name: "glob pattern", toolName: "Glob", argsJSON: `{"pattern":"**/*.go"}`, contains: []string{"🔍 **/*.go"}},
		{name: "webfetch url", toolName: "WebFetch", argsJSON: `{"url":"https://example.com"}`, contains: []string{"🌐 https://example.com"}},
		{name: "lsp summary", toolName: "Lsp", argsJSON: `{"operation":"definition","path":"main.go"}`, contains: []string{"🔎 definition main.go"}},
		{name: "generic fallback", toolName: "Other", argsJSON: `{"description":"desc","path":"x","content":"y"}`, contains: []string{"path=x", "content=y"}},
		{name: "truncate command", toolName: "Bash", argsJSON: fmt.Sprintf(`{"command":%q}`, long), contains: []string{"$ ", "…"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeToolArgs(tt.toolName, tt.argsJSON)
			if len(tt.contains) == 1 && tt.contains[0] == "" {
				if got != "" {
					t.Fatalf("got %q, want empty", got)
				}
				return
			}
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Fatalf("expected %q in %q", want, got)
				}
			}
		})
	}
}

func TestTruncateLineAndTruncate(t *testing.T) {
	if got := truncateLine("a\nb", 10); got != `a\nb` {
		t.Fatalf("newline replacement = %q", got)
	}
	if got := truncateLine("abcdef", 4); got != "abc…" {
		t.Fatalf("truncateLine = %q", got)
	}
	if got := truncate(strings.Repeat("a", maxNotificationLen+10)); !strings.HasSuffix(got, "...") || len(got) != maxNotificationLen {
		t.Fatalf("truncate len/suffix failed: len=%d got=%q", len(got), got)
	}
}

func TestFormatQuestionNotification(t *testing.T) {
	r := newTestRouter()

	t.Run("shows question text", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{
			PendingQuestion: &QuestionPayload{Question: "Continue?", RequestID: "req-1"},
		})
		if !strings.Contains(msg, "Continue?") {
			t.Errorf("expected question text, got: %s", msg)
		}
	})

	t.Run("shows options with details", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{
			PendingQuestion: &QuestionPayload{
				Question:      "How to proceed?",
				Options:       []string{"yes", "no"},
				OptionDetails: []string{"Yes, proceed", "No, stop"},
				RequestID:     "req-2",
			},
		})
		if !strings.Contains(msg, "yes — Yes, proceed") {
			t.Errorf("expected option with detail, got: %s", msg)
		}
		if !strings.Contains(msg, "no — No, stop") {
			t.Errorf("expected option with detail, got: %s", msg)
		}
	})

	t.Run("shows header default and multi-select", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{
			PendingQuestion: &QuestionPayload{
				Header:        "Pick",
				Question:      "Select files",
				Options:       []string{"A", "B"},
				DefaultAnswer: "A",
				Multiple:      true,
			},
		})
		for _, want := range []string{"Pick: Select files", "Default: A", "multi-select"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("expected %q in %q", want, msg)
			}
		}
	})

	t.Run("skips option detail when same as label", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{
			PendingQuestion: &QuestionPayload{
				Question:      "Pick one",
				Options:       []string{"A", "B"},
				OptionDetails: []string{"A", "B"},
			},
		})
		if strings.Contains(msg, "A — A") {
			t.Errorf("should not repeat label as detail, got: %s", msg)
		}
	})

	t.Run("nil PendingQuestion returns empty", func(t *testing.T) {
		msg := r.formatQuestionNotification(ControlState{})
		if msg != "" {
			t.Errorf("expected empty, got: %s", msg)
		}
	})
}

func TestResolveQuestionAnswers(t *testing.T) {
	opts := []string{"yes", "no", "maybe"}
	q := &QuestionPayload{Options: opts}

	t.Run("single valid index", func(t *testing.T) {
		got := resolveQuestionAnswers("1", q)
		if len(got) != 1 || got[0] != "yes" {
			t.Errorf("got %v, want [yes]", got)
		}
	})

	t.Run("comma-separated indices for multi-select", func(t *testing.T) {
		q := &QuestionPayload{Options: opts, Multiple: true}
		got := resolveQuestionAnswers("1,3", q)
		if len(got) != 2 || got[0] != "yes" || got[1] != "maybe" {
			t.Errorf("got %v, want [yes maybe]", got)
		}
	})

	t.Run("non-numeric content becomes custom text", func(t *testing.T) {
		got := resolveQuestionAnswers("yes, please", q)
		if len(got) != 1 || got[0] != "yes, please" {
			t.Errorf("got %v, want [yes, please]", got)
		}
	})

	t.Run("out-of-range index becomes custom text", func(t *testing.T) {
		got := resolveQuestionAnswers("5", q)
		if len(got) != 1 || got[0] != "5" {
			t.Errorf("got %v, want [5]", got)
		}
	})

	t.Run("mixed numeric and non-numeric becomes custom text", func(t *testing.T) {
		got := resolveQuestionAnswers("1,yes", q)
		if len(got) != 1 || got[0] != "1,yes" {
			t.Errorf("got %v, want [1,yes]", got)
		}
	})

	t.Run("single-select with multiple indices becomes custom text", func(t *testing.T) {
		q := &QuestionPayload{Options: opts, Multiple: false}
		got := resolveQuestionAnswers("1,3", q)
		if len(got) != 1 || got[0] != "1,3" {
			t.Errorf("got %v, want [1,3]", got)
		}
	})

	t.Run("nil question passes through", func(t *testing.T) {
		got := resolveQuestionAnswers("hello", nil)
		if len(got) != 1 || got[0] != "hello" {
			t.Errorf("got %v, want [hello]", got)
		}
	})

	t.Run("empty options passes through", func(t *testing.T) {
		got := resolveQuestionAnswers("1", &QuestionPayload{Options: nil})
		if len(got) != 1 || got[0] != "1" {
			t.Errorf("got %v, want [1]", got)
		}
	})

	t.Run("free text /answer becomes custom text", func(t *testing.T) {
		got := resolveQuestionAnswers("yes", q)
		if len(got) != 1 || got[0] != "yes" {
			t.Errorf("got %v, want [yes]", got)
		}
	})
}

func TestFormatNotification_AssistantInfoToastAndLongRunning(t *testing.T) {
	r := newTestRouter()
	if msg := r.formatNotification("test", "assistant_message", ControlState{LastAssistantText: "final answer"}); msg != "final answer" {
		t.Fatalf("assistant_message = %q, want final answer", msg)
	}
	if msg := r.formatNotification("test", "assistant_message", ControlState{}); msg != "" {
		t.Fatalf("assistant_message empty = %q, want empty", msg)
	}
	if msg := r.formatNotification("test", "activity", ControlState{Busy: true}); msg != "" {
		t.Fatalf("activity should not push: %q", msg)
	}
	if msg := r.formatNotification("test", "info", ControlState{InfoMessage: "something happened"}); !strings.Contains(msg, "ℹ️") {
		t.Fatalf("info notification = %q", msg)
	}
	if msg := r.formatNotification("test", "info", ControlState{}); msg != "" {
		t.Fatalf("empty info = %q", msg)
	}
	if msg := r.formatNotification("test", "toast", ControlState{ToastMessage: "careful", ToastLevel: "warn"}); !strings.Contains(msg, "🔔") {
		t.Fatalf("warn toast = %q", msg)
	}
	if msg := r.formatNotification("test", "toast", ControlState{ToastMessage: "broken", ToastLevel: "error"}); !strings.Contains(msg, "🔔") {
		t.Fatalf("error toast = %q", msg)
	}
	if msg := r.formatNotification("test", "toast", ControlState{ToastMessage: "just info", ToastLevel: "info"}); msg != "" {
		t.Fatalf("info toast = %q, want empty", msg)
	}
	if msg := r.formatNotification("test", "long_running", ControlState{Phase: "thinking", PhaseDetail: "analyzing", ToolCallsSinceLastPush: 2}); !strings.Contains(msg, "Still working") {
		t.Fatalf("long_running = %q", msg)
	}
}

func TestFormatOtherNotifications(t *testing.T) {
	r := newTestRouter()
	if got := r.formatIdleNotification("ws", ControlState{LastOutcome: "completed"}); got != "" {
		t.Fatalf("idle completed = %q", got)
	}
	if got := r.formatIdleNotification("ws", ControlState{LastOutcome: "error", LastError: "boom"}); !strings.Contains(got, "boom") {
		t.Fatalf("idle error = %q", got)
	}
	if got := r.formatIdleNotification("ws", ControlState{LastOutcome: "cancelled"}); got != "" {
		t.Fatalf("idle cancelled = %q", got)
	}
	if got := r.formatErrorNotification("ws", ControlState{LastError: "boom"}); !strings.Contains(got, "boom") {
		t.Fatalf("error notification = %q", got)
	}
	if got := r.formatInfoNotification(ControlState{}); got != "" {
		t.Fatalf("empty info = %q", got)
	}
	if got := r.formatToastNotification(ControlState{}); got != "" {
		t.Fatalf("empty toast = %q", got)
	}
	if got := r.formatExitNotification("ws", ControlState{Busy: true}); got == "" {
		t.Fatal("busy exit should notify")
	}
	if got := r.formatExitNotification("ws", ControlState{Busy: false}); got != "" {
		t.Fatalf("idle exit = %q", got)
	}
	if got := r.formatToolResultNotification(ControlState{}); got != "" {
		t.Fatalf("nil tool result = %q", got)
	}
	if got := r.formatToolResultNotification(ControlState{LastToolResult: &ToolResultInfo{Name: "Bash", Status: "success"}}); got != "" {
		t.Fatalf("successful tool result = %q", got)
	}
	if got := r.formatToolResultNotification(ControlState{LastToolResult: &ToolResultInfo{Name: "Bash", Status: "error"}}); !strings.Contains(got, "Tool Bash failed") {
		t.Fatalf("error tool result = %q", got)
	}
}

func TestFormatTodosNotification(t *testing.T) {
	r := newTestRouter()
	key := "ws|wechat|chat"
	if got := r.formatTodosNotification(key, ControlState{Todos: []TodoItem{{ID: "1", Content: "task", Status: "pending"}}}); got != "" {
		t.Fatalf("pending todo should not notify: %q", got)
	}
	got := r.formatTodosNotification(key, ControlState{Todos: []TodoItem{{ID: "1", Content: "task", Status: "in_progress"}}})
	if !strings.Contains(got, "task") {
		t.Fatalf("expected in-progress notification, got %q", got)
	}
	if got := r.formatTodosNotification(key, ControlState{Todos: []TodoItem{{ID: "1", Content: "task", Status: "in_progress"}}}); got != "" {
		t.Fatalf("unchanged in-progress should not notify: %q", got)
	}
}

func TestChatIDLookupHelpers(t *testing.T) {
	r := &NotificationRouter{
		cfg: &config.Config{
			IMs: []config.IMAdapterConfig{{Type: "wechat"}, {Type: "feishu"}},
			Workspaces: []config.Workspace{
				{ID: "ws1", Path: "/tmp/ws1", IMChatID: "feishu-config"},
				{ID: "ws2", Path: "/tmp/ws2", IMChatID: "feishu-two"},
			},
		},
		lastKeyChatID: map[string]string{
			(processKey{workspaceID: "ws1", imType: "wechat", chatID: "wechat-chat"}).String():    "wechat-chat",
			(processKey{workspaceID: "ws1", imType: "feishu", chatID: "feishu-tracked"}).String(): "feishu-tracked",
		},
	}

	chatIDs := r.chatIDsForWorkspace("ws1")
	if got := chatIDs["wechat"]; got != "wechat-chat" {
		t.Fatalf("wechat chatID = %q", got)
	}
	if got := chatIDs["feishu"]; got != "feishu-tracked" {
		t.Fatalf("feishu chatID = %q", got)
	}
	if got := r.chatIDForAdapter("weixin"); got != "wechat-chat" {
		t.Fatalf("chatIDForAdapter weixin = %q", got)
	}
	if got := r.chatIDForAdapter("lark"); got != "feishu-tracked" {
		t.Fatalf("chatIDForAdapter lark = %q", got)
	}
	if got := r.adapterTypeForChatID("wechat-chat"); got != "wechat" {
		t.Fatalf("adapterTypeForChatID tracked wechat = %q", got)
	}
	if got := r.adapterTypeForChatID("feishu-two"); got != "feishu" {
		t.Fatalf("adapterTypeForChatID config feishu = %q", got)
	}
	if got := r.adapterTypeForChatID("unknown"); got != "" {
		t.Fatalf("adapterTypeForChatID unknown = %q", got)
	}
}

func TestFindAdapterByTypeAndAvailableLoginTargets(t *testing.T) {
	wechat := &stubIMAdapter{typ: "wechat", startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
	feishu := &stubIMAdapter{typ: "feishu", startLoginFunc: func() (string, error) { return "", ErrLoginNotSupported }}
	multi := &MultiAdapter{adapters: []namedAdapter{{IMAdapter: wechat}, {IMAdapter: feishu}}}
	r := &NotificationRouter{adapter: multi}

	if got := r.findAdapterByType("wx"); got != wechat {
		t.Fatalf("findAdapterByType wx failed")
	}
	if got := r.findAdapterByType("lark"); got != feishu {
		t.Fatalf("findAdapterByType lark failed")
	}
	if got := r.findAdapterByType("unknown"); got != nil {
		t.Fatalf("findAdapterByType unknown = %#v", got)
	}

	targets := r.availableLoginTargets()
	if len(targets) != 1 || targets[0] != "weixin" {
		t.Fatalf("availableLoginTargets = %v", targets)
	}
}

func TestHandleLogin(t *testing.T) {
	ws := &config.Workspace{ID: "ws", Path: "/tmp/ws"}

	t.Run("no supported targets", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: sender}
		r.handleLogin(ws, "chat", "wechat", "")
		if got := sender.lastMessage().text; !strings.Contains(got, "当前没有支持登录续期") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("show usage when target missing", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat", startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
		r := &NotificationRouter{adapter: sender}
		r.handleLogin(ws, "chat", "wechat", "")
		if got := sender.lastMessage().text; !strings.Contains(got, "/login <平台>") || !strings.Contains(got, "weixin") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("adapter not found", func(t *testing.T) {
		sender := &stubIMAdapter{typ: "wechat", startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
		r := &NotificationRouter{adapter: sender}
		r.handleLogin(ws, "chat", "wechat", "feishu")
		if got := sender.lastMessage().text; !strings.Contains(got, "未找到 feishu 适配器") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("login not supported", func(t *testing.T) {
		loginless := &stubIMAdapter{typ: "feishu"}
		sender := &stubIMAdapter{typ: "wechat"}
		r := &NotificationRouter{adapter: &MultiAdapter{adapters: []namedAdapter{{IMAdapter: sender}, {IMAdapter: loginless}}}}
		r.handleLogin(ws, "chat", "wechat", "feishu")
		if got := sender.lastMessage().text; !strings.Contains(got, "飞书 不支持通过 /login 续期") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("login failure", func(t *testing.T) {
		loginErr := errors.New("boom")
		loginAdapter := &stubIMAdapter{typ: "wechat", startLoginFunc: func() (string, error) { return "", loginErr }}
		r := &NotificationRouter{adapter: loginAdapter}
		r.handleLogin(ws, "chat", "wechat", "weixin")
		if got := loginAdapter.lastMessage().text; !strings.Contains(got, "获取 微信 登录链接失败") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("login success", func(t *testing.T) {
		loginAdapter := &stubIMAdapter{typ: "wechat", startLoginFunc: func() (string, error) { return "https://wx-login", nil }}
		r := &NotificationRouter{adapter: loginAdapter}
		r.handleLogin(ws, "chat", "wechat", "wx")
		if got := loginAdapter.lastMessage().text; !strings.Contains(got, "https://wx-login") {
			t.Fatalf("message = %q", got)
		}
	})
}

func TestHandleSessionExpiredAndLoginResult(t *testing.T) {
	wechat := &stubIMAdapter{typ: "wechat"}
	feishu := &stubIMAdapter{typ: "feishu"}
	multi := &MultiAdapter{adapters: []namedAdapter{{IMAdapter: wechat}, {IMAdapter: feishu}}}
	r := &NotificationRouter{
		adapter:       multi,
		cfg:           &config.Config{Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1", IMChatID: "feishu-chat"}}},
		lastKeyChatID: map[string]string{(processKey{workspaceID: "ws1", imType: "wechat", chatID: "wechat-chat"}).String(): "wechat-chat"},
	}

	r.HandleSessionExpired("wechat")
	if msgs := feishu.sentMessages(); len(msgs) != 1 || !strings.Contains(msgs[0].text, "/login weixin") {
		t.Fatalf("feishu messages after wechat expiry = %#v", msgs)
	}
	if got := len(wechat.sentMessages()); got != 0 {
		t.Fatalf("wechat should not receive self expiry notification, got %d", got)
	}

	r.HandleSessionExpired("feishu")
	if msgs := wechat.sentMessages(); len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].text, "飞书会话已失效") {
		t.Fatalf("wechat messages after feishu expiry = %#v", msgs)
	}

	r.HandleLoginResult("wechat", true, "")
	if msgs := feishu.sentMessages(); !strings.Contains(msgs[len(msgs)-1].text, "微信 登录已续期") {
		t.Fatalf("feishu messages after login success = %#v", msgs)
	}

	r.HandleLoginResult("wechat", false, "network")
	if msgs := feishu.sentMessages(); !strings.Contains(msgs[len(msgs)-1].text, "登录续期失败") {
		t.Fatalf("feishu messages after login failure = %#v", msgs)
	}
}

func TestSendTextAndBroadcastHelpers(t *testing.T) {
	r := &NotificationRouter{
		cfg:           &config.Config{Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1", IMChatID: "feishu-config"}}},
		lastKeyChatID: map[string]string{(processKey{workspaceID: "ws1", imType: "wechat", chatID: "wechat-chat"}).String(): "wechat-chat"},
	}

	r.sendText("chat", "hello")

	single := &stubIMAdapter{typ: "wechat"}
	r.adapter = single
	r.sendText("chat", "hello")
	if got := single.lastMessage(); got.chatID != "chat" || got.text != "hello" {
		t.Fatalf("sendText got %#v", got)
	}

	r.sendTextAll("ws1", "workspace-msg")
	if got := len(single.sentMessages()); got != 2 {
		t.Fatalf("single sendTextAll count = %d", got)
	}

	wechat := &stubIMAdapter{typ: "wechat"}
	feishu := &stubIMAdapter{typ: "feishu"}
	r.adapter = &MultiAdapter{adapters: []namedAdapter{{IMAdapter: wechat}, {IMAdapter: feishu}}, router: r}
	r.sendTextAll("ws1", "workspace-msg")
	if got := wechat.lastMessage().chatID; got != "wechat-chat" {
		t.Fatalf("wechat broadcast chatID = %q", got)
	}
	if got := feishu.lastMessage().chatID; got != "feishu-config" {
		t.Fatalf("feishu broadcast chatID = %q", got)
	}
}

func TestBuildFeishuCardsAndButton(t *testing.T) {
	confirm := buildFeishuConfirmCard("chat-1", &ConfirmPayload{ToolName: "Bash", ArgsJSON: `{"command":"pwd"}`, RequestID: "req-1"})
	body := confirm["body"].(map[string]any)
	elements := body["elements"].([]any)
	if len(elements) < 2 {
		t.Fatalf("confirm card elements = %v", elements)
	}
	if action := elements[len(elements)-1].(map[string]any); action["tag"] != "action" {
		t.Fatalf("confirm card action block missing: %v", action)
	}

	question := buildFeishuQuestionCard("chat-2", &QuestionPayload{Question: "Continue?", Options: []string{"yes", "no"}, RequestID: "req-2"})
	qBody := question["body"].(map[string]any)
	qElements := qBody["elements"].([]any)
	if len(qElements) != 2 {
		t.Fatalf("question card elements = %v", qElements)
	}

	btn := feishuCardButton("Allow", "primary", map[string]any{"request_id": "req"})
	behaviors := btn["behaviors"].([]any)
	if len(behaviors) != 1 {
		t.Fatalf("button behaviors = %v", behaviors)
	}
}

func TestProcessEnvelopeNotification(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	payload := []byte(`{"message":"blocked by missing input","reason":"blocked_error"}`)
	p.processEnvelope(&HeadlessEnvelope{Type: "notification", Payload: payload})
	state := p.State()
	if state.LastNotification == nil || state.LastNotification.Reason != "blocked_error" || state.LastNotification.Message != "blocked by missing input" {
		t.Fatalf("LastNotification = %#v", state.LastNotification)
	}
}

// ---------------------------------------------------------------------------
// processEnvelope: todos parsing (new format {"todos": [...]} and old [...])
// ---------------------------------------------------------------------------

func TestProcessEnvelopeTodosNewFormat(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	payload := []byte(`{"todos":[{"id":"1","content":"Build X","status":"in_progress","active_form":"building X"},{"id":"2","content":"Test Y","status":"completed"}]}`)
	p.processEnvelope(&HeadlessEnvelope{Type: "todos", Payload: payload})
	state := p.State()
	if len(state.Todos) != 2 {
		t.Fatalf("expected 2 todos, got %d", len(state.Todos))
	}
	if state.Todos[0].Content != "Build X" || state.Todos[0].Status != "in_progress" {
		t.Errorf("todo[0] = %+v", state.Todos[0])
	}
	if state.Todos[1].Status != "completed" {
		t.Errorf("todo[1].status = %q, want completed", state.Todos[1].Status)
	}
}

func TestProcessEnvelopeTodosOldFormat(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	payload := []byte(`[{"id":"1","content":"Do it","status":"pending"}]`)
	p.processEnvelope(&HeadlessEnvelope{Type: "todos", Payload: payload})
	state := p.State()
	if len(state.Todos) != 1 || state.Todos[0].Content != "Do it" {
		t.Fatalf("todos = %+v", state.Todos)
	}
}

func TestProcessEnvelopeTodosEmptyWrapper(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	payload := []byte(`{"todos":[]}`)
	p.processEnvelope(&HeadlessEnvelope{Type: "todos", Payload: payload})
	state := p.State()
	if state.Todos == nil {
		t.Fatal("expected empty todos slice, got nil")
	}
	if len(state.Todos) != 0 {
		t.Fatalf("expected 0 todos, got %d", len(state.Todos))
	}
}

// ---------------------------------------------------------------------------
// processEnvelope: tool_result updates LastToolResult and increments counter
// ---------------------------------------------------------------------------

func TestProcessEnvelopeToolResult(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	p.state.ToolCallsSinceLastPush = 3

	payload := []byte(`{"call_id":"call-123","name":"Bash","status":"success","agent_id":"a1"}`)
	p.processEnvelope(&HeadlessEnvelope{Type: "tool_result", Payload: payload})
	state := p.State()

	if state.LastToolResult == nil {
		t.Fatal("LastToolResult is nil")
	}
	if state.LastToolResult.CallID != "call-123" {
		t.Errorf("CallID = %q", state.LastToolResult.CallID)
	}
	if state.LastToolResult.Name != "Bash" {
		t.Errorf("Name = %q", state.LastToolResult.Name)
	}
	if state.LastToolResult.Status != "success" {
		t.Errorf("Status = %q", state.LastToolResult.Status)
	}
	if state.ToolCallsSinceLastPush != 4 {
		t.Errorf("ToolCallsSinceLastPush = %d, want 4", state.ToolCallsSinceLastPush)
	}
}

func TestProcessEnvelopeToolResultError(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	payload := []byte(`{"call_id":"c2","name":"Write","status":"error","agent_id":"a1"}`)
	p.processEnvelope(&HeadlessEnvelope{Type: "tool_result", Payload: payload})
	state := p.State()
	if state.LastToolResult == nil || state.LastToolResult.Status != "error" {
		t.Fatalf("expected error tool result, got %+v", state.LastToolResult)
	}
	if state.ToolCallsSinceLastPush != 1 {
		t.Errorf("ToolCallsSinceLastPush = %d, want 1", state.ToolCallsSinceLastPush)
	}
}

// ---------------------------------------------------------------------------
// processEnvelope: status_response reads last_outcome and updates LastStatusResponseAt
// ---------------------------------------------------------------------------

func TestProcessEnvelopeStatusResponse(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	payload := []byte(`{"session_id":"s1","busy":true,"phase":"tool","phase_detail":"running","last_error":"","last_outcome":"","updated_at":"2025-01-01T00:00:00Z"}`)
	prevTime := p.State().LastStatusResponseAt
	if !prevTime.IsZero() {
		t.Fatalf("expected zero LastStatusResponseAt initially")
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Payload: payload})
	state := p.State()
	if state.LastOutcome != "" {
		t.Errorf("LastOutcome = %q, want empty", state.LastOutcome)
	}
	if state.LastStatusResponseAt.IsZero() {
		t.Error("LastStatusResponseAt should be set after status_response")
	}
}

func TestProcessEnvelopeStatusResponseWithOutcome(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	payload := []byte(`{"session_id":"s1","busy":false,"phase":"","phase_detail":"","last_error":"","last_outcome":"completed","updated_at":"2025-01-01T00:00:00Z"}`)
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Payload: payload})
	state := p.State()
	if state.LastOutcome != "completed" {
		t.Errorf("LastOutcome = %q, want completed", state.LastOutcome)
	}
}

// ---------------------------------------------------------------------------
// /status freshness: LastStatusResponseAt changes signal real response
// ---------------------------------------------------------------------------

func TestStatusFreshness_LastStatusResponseAtAdvances(t *testing.T) {
	p := &ChordProcess{
		key:         "ws|wechat|chat",
		workspaceID: "ws",
	}
	p.state.UpdatedAt = "2025-01-01T00:00:00Z"
	p.state.LastStatusResponseAt = time.Time{}

	prev := p.State().LastStatusResponseAt
	if !prev.IsZero() {
		t.Fatalf("expected zero LastStatusResponseAt")
	}

	p.mu.Lock()
	p.state.LastOutcome = "completed"
	p.state.LastStatusResponseAt = time.Now()
	p.mu.Unlock()

	cur := p.State().LastStatusResponseAt
	if cur.IsZero() {
		t.Fatal("LastStatusResponseAt should not be zero after status_response")
	}
	if cur.Equal(prev) {
		t.Error("LastStatusResponseAt did not advance")
	}
}

func waitForStatusResponse(proc *ChordProcess, prev time.Time, waitFn func()) bool {
	for i := 0; i < 5; i++ {
		waitFn()
		cur := proc.State().LastStatusResponseAt
		if !cur.IsZero() && (prev.IsZero() || !cur.Equal(prev)) {
			return true
		}
	}
	return false
}

func TestWaitForStatusResponse_AdvancesAfterSimulatedResponse(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	prev := p.State().LastStatusResponseAt

	step := 0
	waitFn := func() {
		step++
		if step == 3 {
			p.mu.Lock()
			p.state.LastStatusResponseAt = time.Now()
			p.mu.Unlock()
		}
	}
	got := waitForStatusResponse(p, prev, waitFn)
	if !got {
		t.Error("expected status response to be detected")
	}
}

func TestWaitForStatusResponse_StaysZero_Timeout(t *testing.T) {
	p := &ChordProcess{key: "ws|wechat|chat", workspaceID: "ws"}
	prev := p.State().LastStatusResponseAt
	got := waitForStatusResponse(p, prev, func() {})
	if got {
		t.Error("should timeout when no status_response arrives")
	}
}

func TestNormalizeIMTypeAndNames(t *testing.T) {
	if got := normalizeIMType(" wx "); got != "wechat" {
		t.Fatalf("normalize wx = %q", got)
	}
	if got := normalizeIMType("LARK"); got != "feishu" {
		t.Fatalf("normalize lark = %q", got)
	}
	if got := loginCommandName("wechat"); got != "weixin" {
		t.Fatalf("loginCommandName wechat = %q", got)
	}
	if got := loginCommandName("feishu"); got != "feishu" {
		t.Fatalf("loginCommandName feishu = %q", got)
	}
	if got := imDisplayName("wechat"); got != "微信" {
		t.Fatalf("imDisplayName wechat = %q", got)
	}
	if got := imDisplayName("lark"); got != "飞书" {
		t.Fatalf("imDisplayName lark = %q", got)
	}
}

func TestHandleChordCommandAndViews(t *testing.T) {
	newRouterAndProcess := func(state ControlState) (*NotificationRouter, *ChordProcess, *stubIMAdapter, *captureWriteCloser, string, *config.Workspace) {
		cfg := &config.Config{Workspaces: []config.Workspace{{ID: "ws1", Path: "/tmp/ws1", IMChatID: "chat-1"}}}
		mgr := &ChordManager{cfg: cfg, procs: make(map[string]*ChordProcess)}
		sender := &stubIMAdapter{typ: "wechat"}
		stdin := &captureWriteCloser{}
		key := (processKey{workspaceID: "ws1", imType: "wechat", chatID: "chat-1"}).String()
		proc := &ChordProcess{key: key, workspaceID: "ws1", stdin: stdin, state: state}
		mgr.procs[key] = proc
		r := &NotificationRouter{mgr: mgr, cfg: cfg, adapter: sender, lastKeyChatID: make(map[string]string), lastTodos: make(map[string][]TodoItem)}
		return r, proc, sender, stdin, key, &cfg.Workspaces[0]
	}

	t.Run("cancel sends command and ack", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "cancel"}, "wechat")
		if got := sender.lastMessage().text; got != "🛑 Cancel requested." {
			t.Fatalf("message = %q", got)
		}
		if !strings.Contains(stdin.String(), `"type":"cancel"`) {
			t.Fatalf("stdin = %q", stdin.String())
		}
	})

	t.Run("confirm uses explicit request id", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "confirm", Action: "allow", RequestID: "req-1"}, "wechat")
		if got := sender.lastMessage().text; got != "✅ allowed" {
			t.Fatalf("message = %q", got)
		}
		if !strings.Contains(stdin.String(), `"request_id":"req-1"`) || !strings.Contains(stdin.String(), `"action":"allow"`) {
			t.Fatalf("stdin = %q", stdin.String())
		}
	})

	t.Run("confirm falls back to pending request id", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{PendingConfirm: &ConfirmPayload{RequestID: "req-pending"}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "confirm", Action: "deny"}, "wechat")
		if got := sender.lastMessage().text; got != "✅ denied" {
			t.Fatalf("message = %q", got)
		}
		if !strings.Contains(stdin.String(), `"request_id":"req-pending"`) || !strings.Contains(stdin.String(), `"action":"deny"`) {
			t.Fatalf("stdin = %q", stdin.String())
		}
	})

	t.Run("confirm without pending request warns", func(t *testing.T) {
		r, _, sender, _, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "confirm", Action: "allow"}, "wechat")
		if got := sender.lastMessage().text; !strings.Contains(got, "No pending confirmation") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("question maps numeric answers", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{PendingQuestion: &QuestionPayload{RequestID: "req-q", Options: []string{"yes", "no"}}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "question", Answers: []string{"1"}}, "wechat")
		if got := sender.lastMessage().text; got != "💬 Answered: yes" {
			t.Fatalf("message = %q", got)
		}
		if !strings.Contains(stdin.String(), `"answers":["yes"]`) {
			t.Fatalf("stdin = %q", stdin.String())
		}
	})

	t.Run("question without pending request warns", func(t *testing.T) {
		r, _, sender, _, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "question", Answers: []string{"hi"}}, "wechat")
		if got := sender.lastMessage().text; !strings.Contains(got, "No pending question") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("plain send with pending question auto-redirects to answer", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{PendingQuestion: &QuestionPayload{RequestID: "req-auto"}})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "send", Content: "free text answer"}, "wechat")
		if got := sender.lastMessage().text; got != "💬 Answered: free text answer" {
			t.Fatalf("message = %q", got)
		}
		if !strings.Contains(stdin.String(), `"type":"question"`) || !strings.Contains(stdin.String(), `"free text answer"`) {
			t.Fatalf("stdin = %q", stdin.String())
		}
	})

	t.Run("send blocks local-only slash commands", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "send", Content: "/model"}, "wechat")
		if got := sender.lastMessage().text; !strings.Contains(got, "only available in local TUI") {
			t.Fatalf("message = %q", got)
		}
		if got := stdin.String(); got != "" {
			t.Fatalf("stdin should be empty, got %q", got)
		}
	})

	t.Run("send writes user message and requests status", func(t *testing.T) {
		r, _, sender, stdin, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "send", Content: "hello"}, "wechat")
		if len(sender.sentMessages()) != 0 {
			t.Fatalf("unexpected notification messages: %#v", sender.sentMessages())
		}
		out := stdin.String()
		if !strings.Contains(out, `"type":"send"`) || !strings.Contains(out, `"content":"hello"`) || !strings.Contains(out, `"type":"status"`) {
			t.Fatalf("stdin = %q", out)
		}
	})

	t.Run("unknown command type warns", func(t *testing.T) {
		r, _, sender, _, _, ws := newRouterAndProcess(ControlState{})
		r.handleChordCommand(ws, "chat-1", IMCommand{Type: "mystery"}, "wechat")
		if got := sender.lastMessage().text; !strings.Contains(got, "Unknown command") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("handleCurrent without alive process shows idle binding", func(t *testing.T) {
		r, proc, sender, _, _, ws := newRouterAndProcess(ControlState{})
		proc.cmd = nil
		r.handleCurrent(ws, "chat-1", "wechat")
		if got := sender.lastMessage().text; !strings.Contains(got, "⏸️ Idle") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("handleCurrent with alive process shows state", func(t *testing.T) {
		r, proc, sender, _, _, ws := newRouterAndProcess(ControlState{Busy: true, SessionID: "sess-1"})
		proc.cmd = &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
		r.handleCurrent(ws, "chat-1", "wechat")
		if got := sender.lastMessage().text; !strings.Contains(got, "sess-1") {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("handleTodos without active process", func(t *testing.T) {
		r, proc, sender, _, _, ws := newRouterAndProcess(ControlState{})
		proc.cmd = nil
		r.handleTodos(ws, "chat-1", "wechat")
		if got := sender.lastMessage().text; got != "⏸️ No active session." {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("handleTodos with empty list", func(t *testing.T) {
		r, proc, sender, _, _, ws := newRouterAndProcess(ControlState{})
		proc.cmd = &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
		r.handleTodos(ws, "chat-1", "wechat")
		if got := sender.lastMessage().text; got != "📋 No todos." {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("handleTodos renders items", func(t *testing.T) {
		r, proc, sender, _, _, ws := newRouterAndProcess(ControlState{Todos: []TodoItem{{Content: "one", Status: "pending"}, {Content: "two", Status: "in_progress", ActiveForm: "doing two"}, {Content: "three", Status: "completed"}, {Content: "four", Status: "cancelled"}}})
		proc.cmd = &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
		r.handleTodos(ws, "chat-1", "wechat")
		msg := sender.lastMessage().text
		for _, want := range []string{"📋 Todos:", "⬜ one", "🔄 two (doing two)", "✅ three", "❌ four"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("expected %q in %q", want, msg)
			}
		}
	})
}

func TestFeishuCardJSONRoundTrip(t *testing.T) {
	card := buildFeishuQuestionCard("chat", &QuestionPayload{Question: "Q?", Options: []string{"A"}, RequestID: "req"})
	data, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	if !strings.Contains(string(data), `"schema":"2.0"`) {
		t.Fatalf("unexpected card json: %s", data)
	}
}
