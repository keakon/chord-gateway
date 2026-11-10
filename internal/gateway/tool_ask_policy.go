package gateway

import "fmt"

const toolAskPolicyDeniedEvent = "tool_ask_policy_denied"

type ToolAskPolicyDeniedPayload struct {
	CallID   string `json:"call_id"`
	ToolName string `json:"tool_name"`
	AgentID  string `json:"agent_id"`
	Policy   string `json:"policy"`
	Reason   string `json:"reason"`
}

func formatToolAskPolicyDenied(payload *ToolAskPolicyDeniedPayload) string {
	if payload == nil {
		return ""
	}
	return truncate(fmt.Sprintf("Chord automatically denied %s for %s (confirm_ask_policy=%s). No approval is pending for this call.", payload.ToolName, payload.AgentID, payload.Policy))
}
