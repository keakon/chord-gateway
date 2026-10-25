package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestBindingSwitchAndOutgoingTransferAreExclusive(t *testing.T) {
	for _, first := range []string{"binding", "transfer"} {
		t.Run(first, func(t *testing.T) {
			r, code := pairedTestRouter(t)
			msg, key := bindTestOwner(t, r, code, t.TempDir())
			ready := make(chan struct{})
			close(ready)
			writer := &captureWriteCloser{}
			p := &ChordProcess{mgr: r.mgr, key: key, readySignal: ready, stdin: writer, runtimeID: "runtime", sessionEpoch: 1, bindingInstanceID: "binding", workspacePath: "/workspace", state: ControlState{SessionID: "session"}}
			r.mgr.procs[key] = p
			msg.Text = fmt.Sprintf("/bind %q", t.TempDir())
			msg.MessageID = "switch"
			ticket, ok := r.admission.Authorize(msg)
			if !ok {
				t.Fatal("binding denied")
			}
			lease, _, _ := ticket.Gate.Snapshot()
			status := TransferStatus{TransferID: "transfer", SessionID: "session", SourceRuntimeID: "runtime", SourceSessionEpoch: 1, Workspace: "/workspace", TargetKind: "tui", TargetInstanceID: "target", BindingInstanceID: "binding"}
			envelope := takeoverEnvelope(t, "session_transfer_preparing", status)
			args, _ := parsePairedBindArgs(msg.Text)
			if first == "transfer" {
				p.processTransferEnvelope(envelope)
				_, err := (&BindingService{router: r}).Bind(t.Context(), msg, ticket, args)
				if !errors.Is(err, errBindBusy) || lease.Context().Err() != nil || p.stoppedByGateway {
					t.Fatalf("binding interrupted admitted transfer: %v", err)
				}
				return
			}
			if !lease.BeginSend() {
				t.Fatal("lease unavailable")
			}
			result := make(chan error, 1)
			go func() {
				_, err := (&BindingService{router: r}).Bind(t.Context(), msg, ticket, args)
				result <- err
			}()
			select {
			case <-lease.Context().Done():
			case <-time.After(3 * time.Second):
				lease.EndSend()
				t.Fatal("binding did not close admission")
			}
			p.processTransferEnvelope(envelope)
			commands := takeoverCommands(t, writer)
			lease.EndSend()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("binding did not complete")
			}
			if r.mgr.transferBlocked(key) || p.State().Transfer != nil {
				t.Fatal("retiring source accepted takeover")
			}
			if len(commands) != 1 || commands[0]["accepted"] != false || commands[0]["error_code"] != "target_not_ready" {
				t.Fatalf("expected correlated rejection: %+v", commands)
			}
		})
	}
}

func TestPausedInputOmissionPreservesBindingGuard(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	p := &ChordProcess{readySignal: ready, state: ControlState{SessionID: "session", PausedInputs: &PausedInputs{PendingInputCount: 1, Inputs: []PausedInputSummary{{InputID: "input", OwnerTaskID: "main"}}}}}
	if p.CanRebind() {
		t.Fatal("pending input did not block binding")
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 1, Payload: json.RawMessage(`{"session_id":"session","busy":false}`)})
	if p.CanRebind() || p.State().PausedInputs.PendingInputCount != 1 {
		t.Fatal("missing projection erased known pending work")
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 2, Payload: json.RawMessage(`{"session_id":"session","paused_inputs":{"pending_input_count":0,"inputs":[]}}`)})
	if !p.CanRebind() {
		t.Fatal("explicit empty queue did not release binding guard")
	}
	p.state.PausedInputs = &PausedInputs{PendingInputCount: 1}
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 3, Payload: json.RawMessage(`{"session_id":"another-session"}`)})
	if p.State().PausedInputs != nil {
		t.Fatal("new session retained old pending inputs")
	}
}

func TestBindingSwitchCancellationRemainsBlocked(t *testing.T) {
	m, p, _ := newTakeoverTestProcess(t)
	p.readySignal = make(chan struct{})
	close(p.readySignal)
	g := &im.ChatGate{}
	g.Install(im.ChatIdentity{Platform: "wechat", ChatID: "chat"}, "test", 0)
	lease, _, _ := g.Snapshot()
	lease.BeginSend()
	defer lease.EndSend()
	r := NewNotificationRouter(m)
	t.Cleanup(r.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.prepareBindingSwitch(ctx, g, p.key, "replacement"); !errors.Is(err, errBindBlocked) {
		t.Fatalf("cancelled switch = %v", err)
	}
	_, _, phase := g.Snapshot()
	if phase != "blocked" || !p.stoppedByGateway || m.transferBlocked(p.key) {
		t.Fatal("cancelled switch reopened routing or invented transfer guard")
	}
}

func TestBindingRefreshesRestoredInputsBeforeSwitch(t *testing.T) {
	for _, remaining := range []int{0, 1} {
		t.Run(fmt.Sprint(remaining), func(t *testing.T) {
			r, code := pairedTestRouter(t)
			msg, key := bindTestOwner(t, r, code, t.TempDir())
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			ready := make(chan struct{})
			close(ready)
			p := &ChordProcess{key: key, stdin: writer, readySignal: ready, state: ControlState{SessionID: "session", PausedInputs: &PausedInputs{Paused: true, PendingInputCount: 1, Inputs: []PausedInputSummary{{InputID: "input", OwnerTaskID: "worker", Preview: "sample"}}}}}
			r.mgr.procs[key] = p
			response := make(chan error, 1)
			go func() {
				var command map[string]any
				if err := json.NewDecoder(reader).Decode(&command); err != nil {
					response <- err
					return
				}
				if command["type"] != "status" {
					response <- fmt.Errorf("command = %v", command)
					return
				}
				inputs := &PausedInputs{PendingInputCount: remaining}
				if remaining > 0 {
					inputs.Inputs = []PausedInputSummary{{InputID: "input", OwnerTaskID: "worker", Preview: "sample"}}
				}
				raw, _ := json.Marshal(StatusResponse{SessionID: "session", PausedInputs: inputs})
				p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 1, Payload: raw})
				response <- nil
			}()
			msg.Text = fmt.Sprintf("/bind %q", t.TempDir())
			msg.MessageID = "switch"
			ticket, ok := r.admission.Authorize(msg)
			if !ok {
				t.Fatal("authorization failed")
			}
			args, _ := parsePairedBindArgs(msg.Text)
			_, err := (&BindingService{router: r}).Bind(t.Context(), msg, ticket, args)
			if remaining == 0 && err != nil {
				t.Fatal(err)
			}
			if remaining > 0 && !errors.Is(err, errUnresolvedInputs) {
				t.Fatalf("pending worker input allowed switch: %v", err)
			}
			if err := <-response; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBindingStatusRefreshFailurePreservesLease(t *testing.T) {
	r, code := pairedTestRouter(t)
	msg, key := bindTestOwner(t, r, code, t.TempDir())
	ticket, ok := r.admission.Authorize(msg)
	if !ok {
		t.Fatal("missing authorization")
	}
	lease, generation, phase := ticket.Gate.Snapshot()
	ready := make(chan struct{})
	close(ready)
	p := &ChordProcess{key: key, readySignal: ready, state: ControlState{SessionID: "session", PausedInputs: &PausedInputs{PendingInputCount: 1}}}
	r.mgr.procs[key] = p
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.prepareBindingSwitch(ctx, ticket.Gate, key, "replacement"); !errors.Is(err, errInputStatusUnknown) {
		t.Fatalf("refresh = %v", err)
	}
	got, gen, state := ticket.Gate.Snapshot()
	if got != lease || gen != generation || state != phase || lease.Context().Err() != nil || p.stoppedByGateway {
		t.Fatal("failed status refresh retired the old binding")
	}
}
