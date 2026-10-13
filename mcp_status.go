package main

import (
	"fmt"
	"slices"
	"strings"
)

const mcpStatusChangedEvent = "mcp_status_changed"

type MCPServerRef struct {
	WorkspaceID string `json:"workspace_id"`
	Owner       string `json:"owner"`
	ServerName  string `json:"server_name"`
}
type MCPDiagnostic struct {
	ErrorClass      string `json:"error_class,omitempty"`
	SuggestedAction string `json:"suggested_action,omitempty"`
}
type MCPOperation struct {
	OperationID string `json:"operation_id"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	StartedAt   string `json:"started_at"`
}
type MCPScopeRequirement struct {
	RemoteTool string   `json:"remote_tool,omitempty"`
	Stage      string   `json:"stage,omitempty"`
	Scopes     []string `json:"scopes"`
	ErrorClass string   `json:"error_class"`
}
type MCPServerStatus struct {
	Ref               MCPServerRef          `json:"ref"`
	DesiredEnabled    bool                  `json:"desired_enabled"`
	Auth              string                `json:"auth"`
	Operations        []MCPOperation        `json:"operations"`
	Connection        string                `json:"connection"`
	Catalog           string                `json:"catalog"`
	ScopeRequirements []MCPScopeRequirement `json:"scope_requirements"`
	Diagnostic        MCPDiagnostic         `json:"diagnostic"`
}
type MCPStatusSnapshot struct {
	SchemaVersion int               `json:"schema_version"`
	BindingID     string            `json:"binding_id"`
	Version       uint64            `json:"version"`
	Servers       []MCPServerStatus `json:"servers"`
}

func (s *MCPStatusSnapshot) clone() *MCPStatusSnapshot {
	if s == nil {
		return nil
	}
	cloned := *s
	cloned.Servers = slices.Clone(s.Servers)
	for i := range cloned.Servers {
		server := &cloned.Servers[i]
		server.Operations = slices.Clone(server.Operations)
		server.ScopeRequirements = slices.Clone(server.ScopeRequirements)
		for j := range server.ScopeRequirements {
			server.ScopeRequirements[j].Scopes = slices.Clone(server.ScopeRequirements[j].Scopes)
		}
	}
	return &cloned
}

// Call only after the enclosing envelope passes its seq/session ordering check.
// A new binding starts its own version namespace; versions never cross views.
func (s *ControlState) applyMCPStatus(snapshot *MCPStatusSnapshot) bool {
	if snapshot == nil || snapshot.SchemaVersion != 1 || snapshot.BindingID == "" {
		return false
	}
	if s.MCP != nil && s.MCP.BindingID == snapshot.BindingID && snapshot.Version <= s.MCP.Version {
		return false
	}
	s.MCP = snapshot.clone()
	return true
}
func formatMCPStatus(snapshot *MCPStatusSnapshot) string {
	if snapshot == nil || len(snapshot.Servers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("MCP:")
	unavailable := 0
	for _, server := range snapshot.Servers {
		authReady := server.Auth == "ready" || server.Auth == "none" || server.Auth == "static"
		if !server.DesiredEnabled || (authReady && len(server.Operations) == 0 && server.Connection == "connected" && server.Catalog == "ready" && len(server.ScopeRequirements) == 0) {
			continue
		}
		unavailable++
		fmt.Fprintf(&b, "\n  %s/%s: auth=%s; connection=%s; catalog=%s", server.Ref.Owner, server.Ref.ServerName, server.Auth, server.Connection, server.Catalog)
		for _, operation := range server.Operations {
			fmt.Fprintf(&b, "\n    %s: %s", operation.Kind, operation.State)
		}
		if len(server.Operations) > 0 && len(server.ScopeRequirements) == 0 {
			continue
		}
		if server.Auth == "missing" || server.Auth == "reauth_required" || len(server.ScopeRequirements) > 0 {
			fmt.Fprintf(&b, "\n    In this session's working directory on the host: chord mcp login %s", shellQuoteMCPName(server.Ref.ServerName))
			if role, ok := strings.CutPrefix(server.Ref.Owner, "agent:"); ok {
				fmt.Fprintf(&b, " --agent %s", shellQuoteMCPName(role))
			}
		} else if server.Auth == "refresh_required" {
			b.WriteString("\n    Saved credentials will be refreshed before the next request.")
		} else {
			b.WriteString("\n    In this session's working directory on the host, inspect with chord doctor mcp --connect; reconnect in the local TUI.")
		}
	}
	if unavailable == 0 {
		b.WriteString(" all enabled servers are available")
	}
	return b.String()
}

// Notifications omit servers with active operations. The complete intermediate
// state remains available in /status, while stable failures on other servers
// are still reported. Never announce full availability while an operation runs.
func formatMCPNotification(snapshot *MCPStatusSnapshot) string {
	if snapshot == nil {
		return ""
	}
	stable := *snapshot
	stable.Servers = nil
	pending := false
	for _, server := range snapshot.Servers {
		if server.DesiredEnabled && len(server.Operations) > 0 {
			pending = true
			continue
		}
		stable.Servers = append(stable.Servers, server)
	}
	text := formatMCPStatus(&stable)
	if pending && text == "MCP: all enabled servers are available" {
		return ""
	}
	return text
}

func shellQuoteMCPName(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
	}) < 0 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
func remoteMCPCommand(content string) (action string, ok bool) {
	fields := strings.Fields(content)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "/mcp") {
		return "", false
	}
	if len(fields) > 1 {
		action := strings.ToLower(fields[1])
		switch action {
		case "status", "login", "logout", "reauth", "reconnect", "cancel":
			return action, true
		}
		return "", false
	}
	return "status", true
}
