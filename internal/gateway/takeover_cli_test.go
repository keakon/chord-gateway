package gateway

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func takeoverTestCLI(t *testing.T, stdout string, exitCode string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake coordinator uses a POSIX shell")
	}
	dir := t.TempDir()
	binary, argsPath := filepath.Join(dir, "fake-coordinator"), filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\nprintf '%s' " + shellQuote(stdout) + "\nprintf '%s' 'private diagnostic' >&2\nexit " + exitCode + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, argsPath
}

func TestTakeoverCLIContractAndErrors(t *testing.T) {
	req := takeoverRequest{Action: "start", SessionID: "session-1", TransferID: "transfer-1", TargetInstanceID: "target-1", Workspace: "/workspace", StateDir: "/state"}
	base := takeoverTestRecord("draining").Status
	base.TargetKind, base.TargetInstanceID = "gateway", req.TargetInstanceID
	tests := []struct {
		name   string
		change func(*TransferStatus)
		exit   string
		want   string
	}{
		{name: "accepted", exit: "0"},
		{name: "wrong transfer", change: func(s *TransferStatus) { s.TransferID = "other" }, exit: "0", want: "stale_source"},
		{name: "wrong target kind", change: func(s *TransferStatus) { s.TargetKind = "tui" }, exit: "0", want: "stale_source"},
		{name: "wrong target instance", change: func(s *TransferStatus) { s.TargetInstanceID = "other" }, exit: "0", want: "stale_source"},
		{name: "wrong session", change: func(s *TransferStatus) { s.SessionID = "other" }, exit: "0", want: "stale_source"},
		{name: "wrong workspace", change: func(s *TransferStatus) { s.Workspace = "/other" }, exit: "0", want: "stale_source"},
		{name: "source rejected", change: func(s *TransferStatus) {
			s.ErrorCode = "interaction_blocked"
			s.Message = "finish active confirmation"
			s.State = transferStatePreflight
		}, exit: "1", want: "interaction_blocked"},
		{name: "nonzero without error DTO", exit: "1", want: "CLI failed"},
		{name: "missing state", change: func(s *TransferStatus) { s.State = "" }, exit: "0", want: "status_unknown"},
		{name: "too many blockers", change: func(s *TransferStatus) { s.Blockers = make([]TransferBlocker, 65) }, exit: "0", want: "blocker limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := base
			if tt.change != nil {
				tt.change(&status)
			}
			data, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			binary, argsFile := takeoverTestCLI(t, string(data), tt.exit)
			got, err := runTakeoverCLI(context.Background(), binary, req)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("status=%#v error=%v, want %q", got, err, tt.want)
				}
				return
			}
			if err != nil || !sameTransferIdentity(got, status) {
				t.Fatalf("status=%#v error=%v", got, err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{"takeover\nstart\n", "--session\nsession-1\n", "--transfer-id\ntransfer-1\n", "--target-kind\ngateway\n", "--target-instance\ntarget-1\n", "--timeout\n0\n", "--state-dir\n/state\n", "-d\n/workspace\n"} {
				requireContains(t, string(args), expected)
			}
		})
	}
}

func TestTakeoverCLIBoundedAndSingleJSON(t *testing.T) {
	req := takeoverRequest{Action: "status", SessionID: "session-1", TransferID: "transfer-1", Workspace: "/workspace"}
	data, _ := json.Marshal(takeoverTestRecord("draining").Status)
	for _, stdout := range []string{string(data) + "\n{}", strings.Repeat("x", takeoverMaxJSONBytes+1), ""} {
		binary, _ := takeoverTestCLI(t, stdout, "0")
		if _, err := runTakeoverCLI(context.Background(), binary, req); err == nil {
			t.Fatal("invalid or oversized coordinator output accepted")
		}
	}
}

func TestTakeoverCLIPreflightReturnsCanonicalWorkspaceAndKeepsExecutionDirectory(t *testing.T) {
	req := takeoverRequest{Action: transferStatePreflight, SessionID: "session-1", TransferID: "transfer-1", TargetInstanceID: "target-1", Workspace: "/repository/worktree", WorkDir: "/repository/worktree"}
	status := takeoverTestRecord(transferStatePreflight).Status
	status.TargetKind, status.TargetInstanceID, status.Workspace = "gateway", req.TargetInstanceID, "/repository"
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	binary, argsFile := takeoverTestCLI(t, string(data), "0")
	got, err := runTakeoverCLI(t.Context(), binary, req)
	if err != nil || got.Workspace != "/repository" {
		t.Fatalf("preflight=%#v error=%v", got, err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	requireContains(t, string(args), "--target-kind\ngateway\n")
	requireContains(t, string(args), "-d\n/repository/worktree\n")
	req.Action, req.Workspace = "start", got.Workspace
	if _, err = runTakeoverCLI(t.Context(), binary, req); err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(argsFile)
	requireContains(t, string(args), "-d\n/repository/worktree\n")
}

// Cancelling a takeover CLI invocation must kill the whole CLI process group,
// including background children the script left behind.
func TestTakeoverCLIKillsProcessGroupOnCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are not used on Windows")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-coordinator")
	childPidPath := filepath.Join(dir, "child-pid")
	publishPath := filepath.Join(dir, "publish-pid")
	// Hold the file open and empty until the reader observes it, so this
	// exercises the create-before-write window without a scheduling guess.
	script := "#!/bin/sh\nsleep 30 &\nchild=$!\n{ while [ ! -e " + shellQuote(publishPath) + " ]; do sleep 0.01; done\nprintf '%s' \"$child\"; } > " + shellQuote(childPidPath) + "\nexec sleep 30\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runTakeoverCLI(ctx, binary, takeoverRequest{Action: "status", SessionID: "session", TransferID: "transfer", Workspace: dir})
		done <- err
	}()

	var childPid int
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(childPidPath)
		if err == nil {
			if len(data) == 0 {
				if writeErr := os.WriteFile(publishPath, nil, 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			childPid, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && childPid > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("fake CLI did not spawn its background child")
		}
		time.Sleep(5 * time.Millisecond)
	}
	child, err := os.FindProcess(childPid)
	if err != nil {
		t.Fatalf("find background child: %v", err)
	}
	if err := child.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("background child is not alive before cancellation")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runTakeoverCLI did not return after cancellation")
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		if err := child.Signal(syscall.Signal(0)); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background child survived the process group kill")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCLIProcessCancellationKillsChildPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Reuse the idle child used by the stdin tests: it reports readiness and
	// then waits without spawning descendants.
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStdinBlockedHelperProcess$")
	cmd.Env = append(os.Environ(), "CHORD_GATEWAY_STDIN_TEST_HELPER=1")
	cmd.WaitDelay = 2 * time.Second
	configureCLIProcessGroup(cmd)
	stdout, childStdout, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	defer childStdout.Close()
	cmd.Stdout = childStdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = childStdout.Close()
	waited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(waited)
	}()
	ready := make(chan error, 1)
	go func() {
		data := make([]byte, len("ready\n"))
		_, err := io.ReadFull(stdout, data)
		ready <- err
	}()
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			t.Error("child did not exit during cleanup")
		}
	})
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("child did not initialize: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not report readiness")
	}
	cancel()
	select {
	case <-waited:
		if waitErr == nil || cmd.ProcessState.Success() {
			t.Fatalf("child exited without being killed: %v", waitErr)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for WaitDelay instead of killing the child")
	}
}
