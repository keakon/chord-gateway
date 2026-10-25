package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func mcpSnapshotFixture(binding string, version uint64) *MCPStatusSnapshot {
	return &MCPStatusSnapshot{SchemaVersion: 1, BindingID: binding, Version: version, Servers: []MCPServerStatus{{Ref: MCPServerRef{WorkspaceID: "workspace", Owner: "main", ServerName: "sample"}, DesiredEnabled: true, Auth: "missing", Connection: "error", Catalog: "unknown", Operations: []MCPOperation{}, ScopeRequirements: []MCPScopeRequirement{{Scopes: []string{"files:read"}}}}}}
}
func TestMCPStatusEnvelopeAndBindingOrdering(t *testing.T) {
	events := 0
	p := &ChordProcess{onEvent: func(_ string, kind string, _ ControlState) {
		if kind == mcpStatusChangedEvent {
			events++
		}
	}}
	apply := func(seq uint64, binding string, version uint64) {
		raw, err := json.Marshal(mcpSnapshotFixture(binding, version))
		if err != nil {
			t.Fatal(err)
		}
		p.processEnvelope(&HeadlessEnvelope{Type: mcpStatusChangedEvent, Seq: seq, Payload: raw})
	}
	apply(10, "view-1", 5)
	apply(9, "view-2", 100)
	if p.State().MCP.BindingID != "view-1" {
		t.Fatal("old envelope installed a different view")
	}
	apply(11, "view-1", 4)
	if p.State().MCP.Version != 5 {
		t.Fatal("same-view version regressed")
	}
	apply(12, "view-2", 1)
	if p.State().MCP.BindingID != "view-2" || p.State().MCP.Version != 1 || events != 2 {
		t.Fatalf("new view was compared against old version: %+v, events %d", p.State().MCP, events)
	}
	snapshot := p.State()
	snapshot.MCP.Servers[0].ScopeRequirements[0].Scopes[0] = "changed"
	if p.State().MCP.Servers[0].ScopeRequirements[0].Scopes[0] != "files:read" {
		t.Fatal("state snapshot aliases cached scopes")
	}
}

func TestMCPStatusResponsePreservesNewerProjection(t *testing.T) {
	state := ControlState{SessionID: "session", MCP: mcpSnapshotFixture("view", 4)}
	state.applyStatusResponse(&StatusResponse{SessionID: "session", MCP: mcpSnapshotFixture("view", 3)})
	if state.MCP.Version != 4 {
		t.Fatal("status regressed same-view version")
	}
	state.applyStatusResponse(&StatusResponse{SessionID: "new-session"})
	if state.MCP != nil {
		t.Fatal("session transition retained previous MCP visibility")
	}
}
func TestMCPStatusAndRemoteCommandGuidance(t *testing.T) {
	text := formatMCPStatus(mcpSnapshotFixture("view", 1))
	if !strings.Contains(text, "chord mcp login sample") || !strings.Contains(text, "connection=error") || !strings.Contains(text, "session's working directory") {
		t.Fatalf("missing host instruction: %s", text)
	}
	for _, command := range []string{"/mcp", "/mcp status", "/mcp login sample", "/mcp logout sample", "/mcp reconnect sample", "/mcp reauth sample", "/mcp cancel sample"} {
		if _, ok := remoteMCPCommand(command); !ok {
			t.Fatalf("remote command not intercepted: %s", command)
		}
	}
	for _, command := range []string{"/mcp enable sample", "/mcp disable sample", "ordinary message"} {
		if _, ok := remoteMCPCommand(command); ok {
			t.Fatalf("unrelated command intercepted: %s", command)
		}
	}
}

func TestMCPStatusSeparatesAuthenticationAndConnection(t *testing.T) {
	for _, tc := range []struct {
		auth, want string
		available  bool
	}{
		{"ready", "all enabled servers are available", true},
		{"none", "all enabled servers are available", true},
		{"static", "all enabled servers are available", true},
		{"reauth_required", "chord mcp login sample", false},
		{"refresh_required", "refreshed before the next request", false},
		{"error", "chord doctor mcp --connect", false},
	} {
		t.Run(tc.auth, func(t *testing.T) {
			snapshot := mcpSnapshotFixture("view", 1)
			server := &snapshot.Servers[0]
			server.Auth, server.Connection, server.Catalog = tc.auth, "connected", "ready"
			server.ScopeRequirements = nil
			got := formatMCPStatus(snapshot)
			if !strings.Contains(got, tc.want) || strings.Contains(got, "all enabled servers are available") != tc.available {
				t.Fatalf("status=%s", got)
			}
			if tc.auth == "refresh_required" && strings.Contains(got, "mcp login") {
				t.Fatalf("automatic refresh requires manual login: %s", got)
			}
		})
	}
	snapshot := mcpSnapshotFixture("view", 1)
	server := &snapshot.Servers[0]
	server.Auth, server.Connection, server.Catalog = "ready", "connected", "ready"
	server.ScopeRequirements = nil
	server.Operations = []MCPOperation{{Kind: "refresh", State: "refreshing"}}
	if got := formatMCPStatus(snapshot); !strings.Contains(got, "refresh: refreshing") || strings.Contains(got, "all enabled servers are available") {
		t.Fatalf("refresh hidden: %s", got)
	}
}

func TestMCPNotificationsSkipIntermediateAndDuplicateViews(t *testing.T) {
	var messages []string
	p := &ChordProcess{onEvent: func(_ string, event string, state ControlState) {
		if event == mcpStatusChangedEvent {
			messages = append(messages, formatMCPNotification(state.MCP))
		}
	}}
	snapshot := mcpSnapshotFixture("view", 1)
	send := func() {
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		p.processEnvelope(&HeadlessEnvelope{Type: mcpStatusChangedEvent, Seq: snapshot.Version, Payload: raw})
		snapshot.Version++
	}
	send()
	send()
	if len(messages) != 1 {
		t.Fatalf("duplicate status: %v", messages)
	}
	snapshot.Servers[0].Operations = []MCPOperation{{Kind: "login", State: "waiting_for_browser"}}
	send()
	if len(messages) != 1 || !strings.Contains(formatMCPStatus(p.State().MCP), "waiting_for_browser") {
		t.Fatal("intermediate state pushed or lost from status")
	}
	other := mcpSnapshotFixture("view", 1).Servers[0]
	other.Ref.ServerName = "other"
	snapshot.Servers = append(snapshot.Servers, other)
	send()
	if len(messages) != 2 || !strings.Contains(messages[1], "other") || strings.Contains(messages[1], "sample") {
		t.Fatalf("stable failure hidden by another operation: %v", messages)
	}
	snapshot.Servers = snapshot.Servers[:1]
	snapshot.Servers[0].Operations = nil
	snapshot.Servers[0].Auth = "ready"
	snapshot.Servers[0].Connection = "connected"
	snapshot.Servers[0].Catalog = "ready"
	snapshot.Servers[0].ScopeRequirements = nil
	send()
	send()
	if len(messages) != 3 || !strings.Contains(messages[2], "all enabled servers are available") {
		t.Fatalf("recovery not emitted once: %v", messages)
	}
	snapshot.BindingID = "new-view"
	send()
	if len(messages) != 4 {
		t.Fatal("new view inherited notification suppression")
	}
}

func TestMCPNotificationSkipsFirstAvailableSummary(t *testing.T) {
	var messages []string
	p := &ChordProcess{onEvent: func(_ string, event string, state ControlState) {
		if event == mcpStatusChangedEvent {
			messages = append(messages, formatMCPNotification(state.MCP))
		}
	}}
	healthy := mcpSnapshotFixture("view", 1)
	server := &healthy.Servers[0]
	server.Auth, server.Connection, server.Catalog, server.ScopeRequirements = "ready", "connected", "ready", nil
	send := func() {
		raw, err := json.Marshal(healthy)
		if err != nil {
			t.Fatal(err)
		}
		p.processEnvelope(&HeadlessEnvelope{Type: mcpStatusChangedEvent, Seq: healthy.Version, Payload: raw})
		healthy.Version++
	}
	send()
	send()
	if len(messages) != 0 {
		t.Fatalf("fresh process announced the initial summary: %v", messages)
	}
	server.Auth, server.Connection, server.Catalog = "reauth_required", "connected", "ready"
	send()
	if len(messages) != 1 || !strings.Contains(messages[0], "chord mcp login sample") {
		t.Fatalf("problem not reported: %v", messages)
	}
	server.Auth = "ready"
	send()
	if len(messages) != 2 || messages[1] != mcpAllAvailableNotification {
		t.Fatalf("recovery not reported once: %v", messages)
	}
	send()
	if len(messages) != 2 {
		t.Fatalf("recovery repeated: %v", messages)
	}
}
