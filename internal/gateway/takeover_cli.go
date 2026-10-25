package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const sessionTransferExitCode = 20
const takeoverMaxJSONBytes = 64 * 1024

type takeoverRequest struct {
	Action           string
	TransferID       string
	SessionID        string
	TargetInstanceID string
	Workspace        string
	WorkDir          string
	StateDir         string
}

// Only invoke the public CLI. Gateway deliberately knows nothing about UDS.
func runTakeoverCLI(ctx context.Context, binary string, req takeoverRequest) (TransferStatus, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return TransferStatus{}, errors.New("unsupported_platform: takeover requires macOS or Linux")
	}
	workDir := req.WorkDir
	if workDir == "" {
		workDir = req.Workspace
	}
	args := []string{"takeover", req.Action, "--session", req.SessionID, "--transfer-id", req.TransferID, "--json", "-d", workDir}
	if req.StateDir != "" {
		args = append(args, "--state-dir", req.StateDir)
	}
	if req.Action == "start" || req.Action == transferStatePreflight {
		args = append(args, "--target-kind", "gateway", "--target-instance", req.TargetInstanceID)
	}
	if req.Action == "start" {
		args = append(args, "--timeout", "0")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = loginShellEnv()
	out := &boundedCLIOutput{}
	cmd.Stdout = out
	// Diagnostics may contain sensitive source state and are never sent to IM.
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return TransferStatus{}, ctx.Err()
	}
	var status TransferStatus
	if decodeErr := json.Unmarshal(out.data, &status); decodeErr != nil {
		return status, fmt.Errorf("takeover CLI returned no valid status: %w", errors.Join(err, decodeErr))
	}
	if status.TransferID != req.TransferID || status.SessionID != req.SessionID || status.Workspace == "" || req.Action != transferStatePreflight && status.Workspace != req.Workspace || req.TargetInstanceID != "" && (status.TargetKind != "gateway" || status.TargetInstanceID != req.TargetInstanceID) {
		return TransferStatus{}, errors.New("stale_source: coordinator identity mismatch")
	}
	if len(status.Blockers) > 64 {
		return TransferStatus{}, errors.New("takeover status exceeds blocker limit")
	}
	if status.State != transferStatePreflight && status.State != "status_unknown" && status.SourceRuntimeID == "" {
		return TransferStatus{}, errors.New("stale_source: missing source runtime identity")
	}
	if req.Action == transferStatePreflight && status.State != transferStatePreflight {
		return TransferStatus{}, errors.New("status_unknown: invalid preflight state")
	}
	if status.State == "" {
		return TransferStatus{}, errors.New("status_unknown: missing transfer state")
	}
	if err != nil && status.ErrorCode == "" {
		return status, fmt.Errorf("takeover CLI failed: %w", err)
	}
	if status.ErrorCode != "" {
		return status, fmt.Errorf("%s: %s", status.ErrorCode, strings.TrimSpace(status.Message))
	}
	return status, nil
}

type boundedCLIOutput struct{ data []byte }

func (w *boundedCLIOutput) Write(p []byte) (int, error) {
	if len(w.data)+len(p) > takeoverMaxJSONBytes {
		return 0, errors.New("takeover CLI output exceeds frame limit")
	}
	w.data = append(w.data, p...)
	return len(p), nil
}

// Capability discovery protects ordinary owned operation with a binary that
// has no takeover support. It does not negotiate a second takeover protocol.
func probeGatewayManaged(ctx context.Context, binary string) bool {
	cmd := exec.CommandContext(ctx, binary, "headless", "--help")
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = loginShellEnv()
	output := &boundedCLIOutput{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	return cmd.Run() == nil && strings.Contains(string(output.data), "--gateway-managed") && strings.Contains(string(output.data), "--gateway-instance")
}
