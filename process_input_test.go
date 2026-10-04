package main

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestInputReceiptTracksConsumption(t *testing.T) {
	for _, status := range []string{inputHandled, inputQueued, inputStarted, inputRejected} {
		t.Run(status, func(t *testing.T) {
			stdin := &captureWriteCloser{}
			p := &ChordProcess{stdin: stdin, eventLogf: func(string, ...any) {}}
			if err := p.SendUserMessage("request"); err != nil {
				t.Fatal(err)
			}
			if p.State().Busy {
				t.Fatal("write marked process busy")
			}
			var cmd map[string]string
			if err := json.Unmarshal([]byte(stdin.String()), &cmd); err != nil {
				t.Fatal(err)
			}
			if cmd["request_id"] == "" {
				t.Fatal("missing correlation ID")
			}
			payload, _ := json.Marshal(InputResultPayload{RequestID: cmd["request_id"], Status: status})
			p.processEnvelope(&HeadlessEnvelope{Type: "input_result", Seq: 2, Payload: payload})
			if p.State().Busy != (status == inputStarted) || len(p.pendingInputs) != 0 {
				t.Fatalf("state = %#v", p.State())
			}
			p.processEnvelope(&HeadlessEnvelope{Type: "idle", Seq: 3})
			p.processEnvelope(&HeadlessEnvelope{Type: "input_result", Seq: 4, Payload: payload})
			if p.State().Busy {
				t.Fatal("duplicate receipt restored busy")
			}
		})
	}
}

func TestInputReceiptDoesNotClearActiveWork(t *testing.T) {
	p := &ChordProcess{stdin: &captureWriteCloser{}, eventLogf: func(string, ...any) {}}
	p.processEnvelope(&HeadlessEnvelope{Type: "activity", Seq: 5})
	for _, status := range []string{inputHandled, inputQueued, inputRejected} {
		if err := p.SendUserMessage("next"); err != nil {
			t.Fatal(err)
		}
		var id string
		for key := range p.pendingInputs {
			id = key
		}
		payload, _ := json.Marshal(InputResultPayload{RequestID: id, Status: status})
		p.processEnvelope(&HeadlessEnvelope{Type: "input_result", Seq: 6, Payload: payload})
		if !p.State().Busy {
			t.Fatal("receipt cleared active work")
		}
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "idle", Seq: 8})
	if err := p.SendUserMessage("late"); err != nil {
		t.Fatal(err)
	}
	var id string
	for key := range p.pendingInputs {
		id = key
	}
	payload, _ := json.Marshal(InputResultPayload{RequestID: id, Status: inputStarted})
	p.processEnvelope(&HeadlessEnvelope{Type: "input_result", Seq: 7, Payload: payload})
	p.processEnvelope(&HeadlessEnvelope{Type: "activity", Seq: 7})
	if p.State().Busy {
		t.Fatal("stale signal restored busy")
	}
}

func TestInputAdmissionBoundAndWriteFailure(t *testing.T) {
	p := &ChordProcess{stdin: &errorWriteCloser{err: errors.New("write failed")}}
	if p.SendUserMessage("request") == nil || len(p.pendingInputs) != 0 {
		t.Fatal("failed write retained input")
	}
	p.stdin = &captureWriteCloser{}
	for range maxPendingInputs {
		if err := p.SendUserMessage("request"); err != nil {
			t.Fatal(err)
		}
	}
	if p.SendUserMessage("overflow") == nil {
		t.Fatal("unbounded pending inputs")
	}
	p.TerminateGroup(0)
	if len(p.pendingInputs) != 0 || p.State().UnconfirmedInputs != maxPendingInputs {
		t.Fatal("disconnect lost unconfirmed count")
	}
}
