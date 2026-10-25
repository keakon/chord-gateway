package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
