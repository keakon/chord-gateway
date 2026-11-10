package gateway

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func TestChordHeadlessContractToolAskPolicyDenied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-repo contract test in short mode")
	}
	chordBin := mustChordBinary(t)
	workspaceDir := t.TempDir()
	configHome := filepath.Join(t.TempDir(), "chord-config")
	mustWriteChordTestConfig(t, configHome)
	overrideLoginShellEnv(t, configHome, t.TempDir())
	if err := os.MkdirAll(filepath.Join(workspaceDir, ".chord"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, ".chord", "config.yaml"), []byte("confirm_ask_policy: deny\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspaceDir, "target.txt")
	if err := os.WriteFile(target, []byte("sample"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := newMockOpenAIProvider()
	provider.enqueue(sseScenario{chunks: []string{
		`data: {"id":"sample","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"delete","arguments":"{\"paths\":[\"target.txt\"],\"reason\":\"sample cleanup\"}"}}]}}]}`,
		`data: {"id":"sample","model":"test-model","choices":[{"index":0,"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}})
	provider.enqueue(sseScenario{chunks: []string{
		`data: {"id":"sample","model":"test-model","choices":[{"index":0,"delta":{"content":"Action deferred."}}]}`,
		`data: {"id":"sample","model":"test-model","choices":[{"index":0,"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}})
	server := httptest.NewServer(provider)
	defer server.Close()
	mustRewriteChordTestConfigAPIURL(t, configHome, server.URL+"/v1/chat/completions")
	mgr := NewChordManager(&config.Config{
		IMs:             []config.IMAdapterConfig{{}},
		Workspaces:      []config.Workspace{{ID: "sample", Path: workspaceDir}},
		ChordPath:       chordBin,
		SessionPinsFile: filepath.Join(t.TempDir(), "session-pins.json"),
	}, testPaths(t))
	type observed struct {
		kind  string
		state ControlState
	}
	events := make(chan observed, 128)
	mgr.SetOnEvent(func(_ string, kind string, state ControlState) {
		select {
		case events <- observed{kind: kind, state: state}:
		default:
			t.Error("event buffer overflow")
		}
	})
	key := processKey{workspaceID: "sample", imType: "console", chatID: "sample-chat"}.String()
	proc, err := mgr.GetOrSpawnForKey(key)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.StopAll(2 * time.Second)
	waitForCondition(t, 20*time.Second, func() bool { return proc.State().SessionID != "" }, "ready", procDiagnostics(proc))
	if err := proc.SendUserMessage("Delete target.txt"); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	decisions := 0
	for {
		select {
		case event := <-events:
			switch event.kind {
			case "confirm_request", "confirm_resolved":
				t.Fatalf("policy denial produced interaction %s", event.kind)
			case toolAskPolicyDeniedEvent:
				decisions++
				p := event.state.LastToolAskPolicyDenied
				if p == nil || p.CallID != "call-1" || p.ToolName != "delete" || p.AgentID != "main" || p.Policy != "deny" {
					t.Fatalf("policy decision = %+v", p)
				}
				if event.state.PendingConfirm != nil || event.state.ExpiredConfirm != nil {
					t.Fatal("policy denial created confirmation state")
				}
			case "assistant_message":
				if decisions != 1 {
					t.Fatalf("decisions = %d, want 1 before model continuation", decisions)
				}
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "sample" {
					t.Fatalf("denied tool changed target: data=%q err=%v", data, err)
				}
				provider.assertRequests(t, 2)
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for policy denial and continuation\n%s", procDiagnostics(proc)())
		}
	}
}
