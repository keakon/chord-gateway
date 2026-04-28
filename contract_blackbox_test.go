package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func TestChordHeadlessContract_StatusAndOptionalEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-repo contract test in short mode")
	}

	chordRepo := mustChordRepoRoot(t)
	chordBin := buildChordBinary(t, chordRepo)
	workspaceDir := t.TempDir()
	chordConfigHome := filepath.Join(t.TempDir(), "chord-config")
	chordStateDir := filepath.Join(t.TempDir(), "chord-state")
	mustWriteChordTestConfig(t, chordConfigHome)
	overrideLoginShellEnv(t, chordConfigHome, chordStateDir)

	provider := newMockOpenAIProvider()
	provider.enqueue(sseScenario{
		chunks: []string{
			`data: {"id":"chatcmpl-test","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"TodoWrite"}}]}}]}`,
			`data: {"id":"chatcmpl-test","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"todos\":[{\"id\":\"todo-1\",\"content\":\"Review current state\",\"status\":\"in_progress\",\"active_form\":\"reviewing current state\"}]}"}}]}}]}`,
			`data: {"id":"chatcmpl-test","model":"test-model","choices":[{"index":0,"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		},
	})
	provider.enqueue(sseScenario{
		chunks: []string{
			`data: {"id":"chatcmpl-test","model":"test-model","choices":[{"index":0,"delta":{"content":"Done."}}]}`,
			`data: {"id":"chatcmpl-test","model":"test-model","choices":[{"index":0,"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		},
	})
	server := httptest.NewServer(provider)
	defer server.Close()
	mustRewriteChordTestConfigAPIURL(t, chordConfigHome, server.URL+"/v1/chat/completions")

	cfg := &config.Config{
		IM:              config.IMConfig{Type: "console"},
		Workspaces:      []config.Workspace{{ID: "test", Path: workspaceDir, IMChatID: "console-chat"}},
		ChordPath:       chordBin,
		SessionPinsFile: filepath.Join(t.TempDir(), "session-pins.json"),
		EventVisibility: config.EventVisibility{
			ToolResult: true,
			Todos:      true,
		},
	}

	mgr := NewChordManager(cfg, testPaths(t))
	key := processKey{workspaceID: "test", imType: "console", chatID: "console-chat"}.String()
	proc, err := mgr.GetOrSpawnForKey(key)
	if err != nil {
		t.Fatalf("spawn chord headless: %v", err)
	}
	defer mgr.StopAll(2 * time.Second)

	waitForCondition(t, 20*time.Second, func() bool {
		return proc.State().SessionID != ""
	}, "ready/session_id")

	if got := proc.State().SessionID; got == "" {
		t.Fatal("expected ready/session_id to be populated")
	}

	statusBefore := requestStatusSnapshot(t, proc)
	if statusBefore.SessionID == "" {
		t.Fatal("status_response missing session_id")
	}
	if statusBefore.LastStatusResponseAt.IsZero() {
		t.Fatal("expected LastStatusResponseAt after status request")
	}
	if statusBefore.LastOutcome != "" {
		t.Fatalf("initial last_outcome = %q, want empty", statusBefore.LastOutcome)
	}

	if err := proc.SendUserMessage("please update todos and then finish"); err != nil {
		t.Fatalf("send user message: %v", err)
	}

	waitForCondition(t, 20*time.Second, func() bool {
		state := proc.State()
		return state.LastToolResult != nil && state.LastToolResult.Name == "TodoWrite"
	}, "tool_result TodoWrite")

	stateAfterTool := proc.State()
	if stateAfterTool.LastToolResult == nil {
		t.Fatal("expected last tool result")
	}
	if stateAfterTool.LastToolResult.Name != "TodoWrite" {
		t.Fatalf("tool_result name = %q, want TodoWrite", stateAfterTool.LastToolResult.Name)
	}
	if stateAfterTool.LastToolResult.Status != "success" {
		t.Fatalf("tool_result status = %q, want success", stateAfterTool.LastToolResult.Status)
	}

	waitForCondition(t, 20*time.Second, func() bool {
		state := proc.State()
		return len(state.Todos) == 1 && state.Todos[0].ID == "todo-1"
	}, "todos event")

	stateAfterTodos := proc.State()
	if len(stateAfterTodos.Todos) != 1 {
		t.Fatalf("todos len = %d, want 1", len(stateAfterTodos.Todos))
	}
	if stateAfterTodos.Todos[0].Content != "Review current state" {
		t.Fatalf("todo content = %q, want Review current state", stateAfterTodos.Todos[0].Content)
	}
	if stateAfterTodos.Todos[0].ActiveForm != "reviewing current state" {
		t.Fatalf("todo active_form = %q, want reviewing current state", stateAfterTodos.Todos[0].ActiveForm)
	}

	waitForCondition(t, 20*time.Second, func() bool {
		state := proc.State()
		return !state.Busy && state.LastOutcome == "completed" && strings.TrimSpace(state.LastAssistantText) == "Done."
	}, "idle completed assistant message")

	statusAfter := requestStatusSnapshot(t, proc)
	if statusAfter.LastOutcome != "completed" {
		t.Fatalf("status_response last_outcome = %q, want completed", statusAfter.LastOutcome)
	}
	if strings.TrimSpace(statusAfter.LastAssistantText) != "Done." {
		t.Fatalf("last assistant text = %q, want Done.", statusAfter.LastAssistantText)
	}
	if statusAfter.LastStatusResponseAt.IsZero() {
		t.Fatal("expected LastStatusResponseAt after second status request")
	}

	provider.assertRequests(t, 2)
}

func TestChordHeadlessContract_DefaultSubscribeIncludesIdleAndNotification(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-repo contract test in short mode")
	}

	chordRepo := mustChordRepoRoot(t)
	chordBin := buildChordBinary(t, chordRepo)
	workspaceDir := t.TempDir()
	chordConfigHome := filepath.Join(t.TempDir(), "chord-config")
	chordStateDir := filepath.Join(t.TempDir(), "chord-state")
	mustWriteChordTestConfig(t, chordConfigHome)
	overrideLoginShellEnv(t, chordConfigHome, chordStateDir)

	provider := newMockOpenAIProvider()
	provider.enqueue(sseScenario{
		chunks: []string{
			`data: {"id":"chatcmpl-test","model":"test-model","choices":[{"index":0,"delta":{"content":"Done."}}]}`,
			`data: {"id":"chatcmpl-test","model":"test-model","choices":[{"index":0,"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		},
	})
	server := httptest.NewServer(provider)
	defer server.Close()
	mustRewriteChordTestConfigAPIURL(t, chordConfigHome, server.URL+"/v1/chat/completions")

	cfg := &config.Config{
		IM:              config.IMConfig{Type: "console"},
		Workspaces:      []config.Workspace{{ID: "test", Path: workspaceDir, IMChatID: "console-chat"}},
		ChordPath:       chordBin,
		SessionPinsFile: filepath.Join(t.TempDir(), "session-pins.json"),
	}

	eventsCh := make(chan string, 64)
	mgr := NewChordManager(cfg, testPaths(t))
	mgr.SetOnEvent(func(key string, eventType string, state ControlState) {
		select {
		case eventsCh <- eventType:
		default:
		}
	})

	key := processKey{workspaceID: "test", imType: "console", chatID: "console-chat"}.String()
	proc, err := mgr.GetOrSpawnForKey(key)
	if err != nil {
		t.Fatalf("spawn chord headless: %v", err)
	}
	defer mgr.StopAll(2 * time.Second)

	waitForCondition(t, 20*time.Second, func() bool {
		return proc.State().SessionID != ""
	}, "ready/session_id")

	if err := proc.SendUserMessage("say done"); err != nil {
		t.Fatalf("send user message: %v", err)
	}

	seen := collectEvents(t, eventsCh, 20*time.Second, func(set map[string]bool) bool {
		return set["assistant_message"] && set["notification"] && set["idle"]
	})
	for _, want := range []string{"assistant_message", "notification", "idle"} {
		if !seen[want] {
			t.Fatalf("expected event %q in %v", want, mapKeys(seen))
		}
	}

	provider.assertRequests(t, 1)
}

func requestStatusSnapshot(t *testing.T, proc *ChordProcess) ControlState {
	t.Helper()
	before := proc.State().LastStatusResponseAt
	if err := proc.SendCommand(map[string]any{"type": "status"}); err != nil {
		t.Fatalf("send status command: %v", err)
	}
	waitForCondition(t, 10*time.Second, func() bool {
		return proc.State().LastStatusResponseAt.After(before)
	}, "status_response")
	return proc.State()
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool, label string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}

func collectEvents(t *testing.T, ch <-chan string, timeout time.Duration, done func(map[string]bool) bool) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	seen := map[string]bool{}
	for {
		if done(seen) {
			return seen
		}
		select {
		case <-ctx.Done():
			return seen
		case ev := <-ch:
			seen[ev] = true
		}
	}
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func overrideLoginShellEnv(t *testing.T, chordConfigHome, chordStateDir string) {
	t.Helper()
	prev := loginShellEnv
	loginShellEnv = func() []string {
		base := os.Environ()
		out := make([]string, 0, len(base)+2)
		for _, e := range base {
			if strings.HasPrefix(e, "CHORD_HOME=") || strings.HasPrefix(e, "CHORD_CONFIG_HOME=") || strings.HasPrefix(e, "CHORD_STATE_DIR=") {
				continue
			}
			out = append(out, e)
		}
		out = append(out, "CHORD_CONFIG_HOME="+chordConfigHome)
		out = append(out, "CHORD_STATE_DIR="+chordStateDir)
		return out
	}
	t.Cleanup(func() {
		loginShellEnv = prev
	})
}

func mustChordRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", "chord"))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("chord repo root not found at %s: %v", root, err)
	}
	return root
}

func buildChordBinary(t *testing.T, chordRepo string) string {
	t.Helper()
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "chord-test")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, "./cmd/chord")
	cmd.Dir = chordRepo
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build chord binary: %v\n%s", err, string(output))
	}
	return binPath
}

func mustWriteChordTestConfig(t *testing.T, chordHome string) {
	t.Helper()
	if err := os.MkdirAll(chordHome, 0o755); err != nil {
		t.Fatalf("mkdir chord home: %v", err)
	}
	configPath := filepath.Join(chordHome, "config.yaml")
	authPath := filepath.Join(chordHome, "auth.yaml")
	configYAML := `providers:
  test-provider:
    type: chat-completions
    api_url: http://127.0.0.1:1/v1/chat/completions
    models:
      test-model:
        name: test-model
        limit:
          context: 32000
          output: 1024
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write chord config: %v", err)
	}
	authYAML := "test-provider:\n  - test-key\n"
	if err := os.WriteFile(authPath, []byte(authYAML), 0o644); err != nil {
		t.Fatalf("write chord auth: %v", err)
	}
}

func mustRewriteChordTestConfigAPIURL(t *testing.T, chordHome, apiURL string) {
	t.Helper()
	path := filepath.Join(chordHome, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chord config: %v", err)
	}
	updated := strings.Replace(string(data), "http://127.0.0.1:1/v1/chat/completions", apiURL, 1)
	if updated == string(data) {
		t.Fatalf("failed to rewrite api_url in %s", path)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("rewrite chord config: %v", err)
	}
}

type sseScenario struct {
	chunks []string
	status int
}

type mockOpenAIProvider struct {
	mu        sync.Mutex
	scenarios []sseScenario
	requests  []map[string]any
}

func newMockOpenAIProvider() *mockOpenAIProvider {
	return &mockOpenAIProvider{}
}

func (m *mockOpenAIProvider) enqueue(s sseScenario) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scenarios = append(m.scenarios, s)
}

func (m *mockOpenAIProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	if len(m.scenarios) == 0 {
		m.mu.Unlock()
		http.Error(w, "unexpected extra request", http.StatusInternalServerError)
		return
	}
	scenario := m.scenarios[0]
	m.scenarios = m.scenarios[1:]
	m.requests = append(m.requests, body)
	m.mu.Unlock()

	status := scenario.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)
	bw := bufio.NewWriter(w)
	for _, chunk := range scenario.chunks {
		_, _ = bw.WriteString(chunk)
		if !strings.HasSuffix(chunk, "\n") {
			_, _ = bw.WriteString("\n")
		}
		if !strings.HasSuffix(chunk, "\n\n") {
			_, _ = bw.WriteString("\n")
		}
		_ = bw.Flush()
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (m *mockOpenAIProvider) assertRequests(t *testing.T, want int) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) != want {
		var buf bytes.Buffer
		for i, req := range m.requests {
			fmt.Fprintf(&buf, "request %d: %v\n", i, req)
		}
		t.Fatalf("provider requests = %d, want %d\n%s", len(m.requests), want, buf.String())
	}
}

// testPaths returns a Paths for tests, using the test's temp directory.
func testPaths(t *testing.T) *config.Paths {
	t.Helper()
	dir := t.TempDir()
	paths, err := config.Resolve(dir, filepath.Join(dir, "config.yaml"), dir, filepath.Join(dir, "gateway.log"), "", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
