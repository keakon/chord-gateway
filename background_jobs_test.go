package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRunningJobsStateAndNotifications(t *testing.T) {
	p := &ChordProcess{key: "sample"}
	router := &NotificationRouter{}
	p.processEnvelope(&HeadlessEnvelope{Type: "idle", Seq: 1, Payload: json.RawMessage(`{"running_jobs":2}`)})
	if p.state.Busy || p.state.RunningJobs != 2 {
		t.Fatalf("state = %+v", p.state)
	}
	if got := router.formatIdleNotification(p.state); !strings.Contains(got, "background jobs still running: 2") {
		t.Fatal(got)
	}
	if got := formatBindingStatus(nil, "", "", p.state); !strings.Contains(got, "Background jobs: 2") {
		t.Fatal(got)
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 2, Payload: json.RawMessage(`{"running_jobs":1}`)})
	if p.state.RunningJobs != 1 {
		t.Fatal(p.state.RunningJobs)
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 1, Payload: json.RawMessage(`{"running_jobs":0}`)})
	if p.state.RunningJobs != 1 {
		t.Fatal("stale status cleared running jobs")
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "idle", Seq: 3, Payload: json.RawMessage(`{"running_jobs":0}`)})
	if p.state.RunningJobs != 0 {
		t.Fatal(p.state.RunningJobs)
	}
	if got := router.formatIdleNotification(p.state); got != "✅ Chord: Ready for input" {
		t.Fatal(got)
	}
	p.state.RunningJobs = 1
	p.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Seq: 4, Payload: json.RawMessage(`{"session_id":"next"}`)})
	if p.state.RunningJobs != 0 {
		t.Fatal("old session jobs leaked")
	}
}

func TestCollectIdleProcessesPreservesRunningJobs(t *testing.T) {
	p := &ChordProcess{key: "sample", lastActivity: time.Now().Add(-time.Hour), state: ControlState{RunningJobs: 1}}
	mgr := &ChordManager{procs: map[string]*ChordProcess{p.key: p}}
	if got := mgr.collectIdleProcesses(time.Minute); len(got) != 0 {
		t.Fatal("running job process removed")
	}
	p.state.RunningJobs = 0
	if got := mgr.collectIdleProcesses(time.Minute); len(got) != 1 || got[0] != p {
		t.Fatal("finished process not collected")
	}
}
