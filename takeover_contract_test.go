package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
)

func newTakeoverContractManager(t *testing.T, binary, workspace string) (*ChordManager, string) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "gateway-transfer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	configHome, stateDir := filepath.Join(root, "config"), filepath.Join(root, "state")
	mustWriteChordTestConfig(t, configHome)
	overrideLoginShellEnv(t, configHome, stateDir)
	m := NewChordManager(&config.Config{ChordPath: binary, ChordStateDir: stateDir, Workspaces: []config.Workspace{{ID: "sample", Path: workspace}}}, testPaths(t))
	key := processKey{workspaceID: "sample", imType: "wechat", chatID: "chat-sample"}.String()
	t.Cleanup(func() { m.StopAll(2 * time.Second) })
	return m, key
}

func runProductionTakeoverStart(t *testing.T, m *ChordManager, workspace, session, transfer string) (TransferStatus, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.Config().ChordBinary(), "takeover", "start", "-d", workspace, "--state-dir", m.Config().ChordStateDir, "--session", session, "--transfer-id", transfer, "--target-kind", "tui", "--target-instance", "target-sample", "--timeout", "0", "--json")
	cmd.Env = loginShellEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var status TransferStatus
	if decodeErr := json.Unmarshal(stdout.Bytes(), &status); decodeErr != nil {
		t.Fatalf("production takeover returned invalid JSON: %v; stdout=%s stderr=%s", decodeErr, stdout.String(), stderr.String())
	}
	return status, err
}

func TestChordTakeoverContract_LinkedWorktreeAndPreparationRejection(t *testing.T) {
	if testing.Short() || runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("cross-repo process contract requires macOS or Linux and non-short tests")
	}
	binary := mustChordBinary(t)
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "released", true: "preparation rejected"}[reject], func(t *testing.T) {
			root := t.TempDir()
			repo, worktree := filepath.Join(root, "repository"), filepath.Join(root, "worktree")
			// Isolate the fixture from machine-wide git configuration: a global
			// core.hooksPath (for example a code-review hook) or commit.gpgsign
			// would otherwise abort this commit and leave the worktree without a
			// HEAD. Identity is supplied inline.
			gitEnv := append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
			for _, args := range [][]string{{"init", repo}, {"-C", repo, "-c", "user.name=Sample", "-c", "user.email=sample@example.invalid", "commit", "--allow-empty", "-m", "sample"}, {"-C", repo, "worktree", "add", "--detach", worktree}} {
				cmd := exec.Command("git", args...)
				cmd.Env = gitEnv
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git fixture: %v: %s", err, out)
				}
			}
			canonical, err := filepath.EvalSymlinks(repo)
			if err != nil {
				t.Fatal(err)
			}
			m, key := newTakeoverContractManager(t, binary, worktree)
			p, err := m.GetOrSpawnForKey(key)
			if err != nil {
				t.Fatal(err)
			}
			waitForCondition(t, 20*time.Second, func() bool { return p.State().SessionID != "" }, "headless ready", procDiagnostics(p))
			p.mu.RLock()
			workspace := p.workspacePath
			p.mu.RUnlock()
			if workspace != canonical {
				t.Fatalf("ready workspace=%q want canonical repository=%q", workspace, canonical)
			}
			writer := m.transfers.writer
			if reject {
				m.transfers.mu.Lock()
				m.transfers.writer = func(string, []byte) error { return errors.New("storage unavailable") }
				m.transfers.mu.Unlock()
			}
			status, startErr := runProductionTakeoverStart(t, m, worktree, p.State().SessionID, "transfer-sample")
			if reject {
				if startErr == nil || status.State != transferStatePrepareFailed {
					t.Fatalf("rejected preparation state=%#v error=%v", status, startErr)
				}
				m.transfers.mu.Lock()
				m.transfers.writer = writer
				m.transfers.mu.Unlock()
				router := NewNotificationRouter(m)
				t.Cleanup(router.Close)
				adapter := &stubIMAdapter{typ: "wechat"}
				router.SetAdapter(adapter)
				router.observeTakeover(&m.Config().Workspaces[0], "chat-sample", key, false, nil, testChatOperation(t, router.mgr, key))
				if m.transferBlocked(key) || p.transferActive() {
					t.Fatal("production prepare_failed retained input guard")
				}
				if err = p.SendUserMessage("Continue the request"); err != nil {
					t.Fatalf("reopened source rejected input: %v", err)
				}
				return
			}
			if startErr != nil {
				t.Fatalf("production start state=%#v error=%v", status, startErr)
			}
			waitForCondition(t, 20*time.Second, func() bool {
				record, ok, err := m.transfers.Get(key)
				return err == nil && ok && record.Status.State == "released"
			}, "transfer release", procDiagnostics(p))
			if !m.transferBlocked(key) {
				t.Fatal("released outgoing binding lost its guard")
			}
		})
	}
}

func TestChordTakeoverContract_PreflightRejectsWithoutBlockingChat(t *testing.T) {
	if testing.Short() || runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("cross-repo process contract requires macOS or Linux and non-short tests")
	}
	binary := mustChordBinary(t)
	workspace := t.TempDir()
	m, key := newTakeoverContractManager(t, binary, workspace)
	router := NewNotificationRouter(m)
	t.Cleanup(router.Close)
	adapter := &stubIMAdapter{typ: "wechat"}
	router.SetAdapter(adapter)
	router.prepareIncomingTakeover(&m.Config().Workspaces[0], "chat-sample", key, parseTakeoverCommand("missing-session --timeout 0"), testChatOperation(t, router.mgr, key))
	if m.transferBlocked(key) {
		t.Fatal("production preflight rejection blocked ordinary chat")
	}
	requireContains(t, adapter.lastMessage().text, "no transfer was started")
	// An explicit start can still fail preflight after a previously successful
	// read-only check. The machine response must prove no mutating dispatch.
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	record := sessionTransfer{Direction: "incoming", Status: TransferStatus{TransferID: "transfer-sample", SessionID: "missing-session", Workspace: canonical, TargetKind: "gateway", TargetInstanceID: "target-sample", State: "waiting_source"}}
	if err = m.transfers.update(key, &record, ""); err != nil {
		t.Fatal(err)
	}
	router.observeTakeover(&m.Config().Workspaces[0], "chat-sample", key, true, nil, testChatOperation(t, router.mgr, key))
	if m.transferBlocked(key) {
		t.Fatal("production start preflight rejection left a durable guard")
	}
}

func TestChordTakeoverContract_TUISourceRestoresPausedIntoGateway(t *testing.T) {
	if testing.Short() || runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("cross-repo process contract requires macOS or Linux and non-short tests")
	}
	binary := mustChordBinary(t)
	helper := os.Getenv("CHORD_GATEWAY_CONTRACT_CHORD_HELPER_BIN")
	if helper == "" {
		helper = filepath.Join(t.TempDir(), "chord-source-fixture")
		cmd := exec.Command("go", "test", "-c", "-o", helper, "./cmd/chord")
		cmd.Dir = mustChordRepoRoot(t)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build source fixture: %v: %s", err, output)
		}
	}
	// Older Chord test binaries exit successfully when -test.run matches nothing.
	// Check the fixture exists instead of waiting for a marker it cannot write.
	list := exec.CommandContext(t.Context(), helper, "-test.list=^TestTakeoverProcessRoundTrip$")
	listed, err := list.CombinedOutput()
	if err != nil || !slices.Contains(strings.Fields(string(listed)), "TestTakeoverProcessRoundTrip") {
		t.Fatalf("Chord source fixture is missing TestTakeoverProcessRoundTrip; use the same takeover-capable Chord revision for the CLI and helper: error=%v output=%s", err, listed)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, key := newTakeoverContractManager(t, binary, workspace)
	provider := newMockOpenAIProvider()
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)
	var configHome string
	for _, entry := range loginShellEnv() {
		if value, ok := strings.CutPrefix(entry, "CHORD_CONFIG_HOME="); ok {
			configHome = value
		}
	}
	mustRewriteChordTestConfigAPIURL(t, configHome, server.URL+"/v1/chat/completions")
	configPath := filepath.Join(configHome, "config.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configData = bytes.Replace(configData, []byte("        name: test-model"), []byte("        name: test-model\n        modalities:\n          input: [text, image]"), 1)
	if err = os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	source := exec.CommandContext(ctx, helper, "-test.run=^TestTakeoverProcessRoundTrip$", "-test.timeout=40s")
	source.Env = append(loginShellEnv(), "CHORD_TAKEOVER_TEST_PROCESS=source-tui", "CHORD_TAKEOVER_TEST_WORKSPACE="+workspace, "CHORD_SESSIONS_DIR="+filepath.Join(m.Config().ChordStateDir, "sessions"))
	output := newTailBuffer(32 * 1024)
	source.Stdout, source.Stderr = output, output
	stdin, err := source.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	var sourceErr error
	go func() {
		sourceErr = source.Wait()
		close(finished)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-finished:
		default:
			_ = source.Process.Kill()
			<-finished
		}
	})
	waitForCondition(t, 20*time.Second, func() bool {
		select {
		case <-finished:
			t.Fatalf("TUI source fixture exited before accepting its draft: %v\n%s", sourceErr, output.String())
		default:
		}
		_, err := os.Stat(filepath.Join(workspace, "queued-input"))
		return err == nil
	}, "source accepted draft", output.String)
	router := NewNotificationRouter(m)
	t.Cleanup(router.Close)
	adapter := &stubIMAdapter{typ: "wechat"}
	router.SetAdapter(adapter)
	router.prepareIncomingTakeover(&m.Config().Workspaces[0], "chat-sample", key, parseTakeoverCommand("sample-session --timeout 0"), testChatOperation(t, router.mgr, key))
	waitForCondition(t, 20*time.Second, func() bool {
		record, ok, err := m.transfers.Get(key)
		return err == nil && ok && record.Status.State == "draining"
	}, "accepted source transfer", func() string {
		record, ok, err := m.transfers.Get(key)
		return fmt.Sprintf("record=%#v present=%v error=%v message=%s", record, ok, err, adapter.lastMessage().text)
	})
	if m.GetProcessForKey(key) != nil {
		t.Fatal("zero observer budget spawned target before source release")
	}
	if err = os.WriteFile(filepath.Join(workspace, "release-request"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	<-finished
	err = sourceErr
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != sessionTransferExitCode {
		t.Fatalf("source exit=%v output=%s", err, output.String())
	}
	// Resume from the durable checkpoint after the source endpoint has gone.
	router.observeTakeover(&m.Config().Workspaces[0], "chat-sample", key, false, nil, testChatOperation(t, router.mgr, key))
	p := m.GetProcessForKey(key)
	if p == nil || m.transferBlocked(key) {
		t.Fatalf("restored binding not ready: %s", adapter.lastMessage().text)
	}
	state := p.State()
	if state.PausedInputs == nil || !state.PausedInputs.Paused || state.PausedInputs.PendingInputCount != 1 || len(state.PausedInputs.Inputs) != 1 || state.PausedInputs.Inputs[0].AttachmentCount != 1 {
		t.Fatalf("restored draft projection=%#v", state.PausedInputs)
	}
	provider.assertRequests(t, 0)
	if err = os.Remove(filepath.Join(workspace, "temporary.png")); err != nil {
		t.Fatal(err)
	}
	provider.enqueue(sseScenario{chunks: []string{
		`data: {"id":"sample-response","model":"test-model","choices":[{"index":0,"delta":{"content":"Completed."}}]}`,
		`data: {"id":"sample-response","model":"test-model","choices":[{"index":0,"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}})
	if err = p.SendUserMessage("Continue this conversation."); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 20*time.Second, func() bool { provider.mu.Lock(); defer provider.mu.Unlock(); return len(provider.requests) == 1 }, "explicit continuation request", procDiagnostics(p))
	waitForCondition(t, 20*time.Second, func() bool { return !p.State().Busy }, "continued turn settled", procDiagnostics(p))
	provider.assertRequests(t, 1)
	provider.mu.Lock()
	request, marshalErr := json.Marshal(provider.requests[0])
	provider.mu.Unlock()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, expected := range []string{"Queued request.", "Continue this conversation.", "data:image/png;base64,"} {
		requireContains(t, string(request), expected)
	}
	calls, err := os.ReadFile(filepath.Join(workspace, "source-calls"))
	if err != nil || string(calls) != "1" {
		t.Fatalf("source started another request: %q error=%v", calls, err)
	}
}
