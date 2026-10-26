package gateway

import "strings"

// chordLocalOnlySlash mirrors IsTUILocalOnlySlashCommand in the companion Chord
// repository (internal/agent/main_tui_api.go). Every entry is a slash command
// Chord executes on its main-agent event loop rather than routing it to a
// focused SubAgent, and every one of them mutates host-side session state that
// the headless protocol never reports back to this integration.
//
// Two differences from the Chord set are deliberate:
//   - /role and /mcp are absent because the gateway maps them to its own
//     confirmed IM commands instead of forwarding them verbatim.
//   - bareOnly marks commands that only take their local meaning without
//     arguments: Chord treats "/skill <name>" as an ordinary user message, and
//     matches "/compact" exactly, so blocking their argument forms would reject
//     messages IM callers legitimately expect to reach the agent.
var chordLocalOnlySlash = map[string]bool{
	"/queue":   false,
	"/export":  false,
	"/tier":    false,
	"/rename":  false,
	"/yolo":    false,
	"/compact": true,
	"/skill":   true,
}

// gatewayOwnedSlash lists slash commands the gateway answers itself, whether it
// handles them directly (/models through the models protocol command, /role
// through the role one) or routes them to a dedicated handler that applies
// guards first (/new and /resume replace the session). Reaching the process
// verbatim would bypass that handling, so text that skips command parsing
// through ForceText must still not reach the process.
var gatewayOwnedSlash = map[string]bool{
	"/models": false,
	"/role":   false,
	"/new":    false,
	"/resume": false,
}

// isChordLocalOnlySlashCommand reports whether content is a slash command Chord
// would run locally on the main agent. Returning true means forwarding the text
// would mutate host state the gateway cannot observe; refusing it is the only
// way to keep the IM control surface and the actual session in agreement.
func isChordLocalOnlySlashCommand(content string) bool {
	return matchesSlashCommand(content, chordLocalOnlySlash)
}

// isGatewayOwnedSlashCommand reports whether content is a slash command the
// gateway handles through a dedicated route.
func isGatewayOwnedSlashCommand(content string) bool {
	return matchesSlashCommand(content, gatewayOwnedSlash)
}

func matchesSlashCommand(content string, commands map[string]bool) bool {
	trimmed := strings.ToLower(strings.TrimSpace(content))
	if trimmed == "" {
		return false
	}
	for command, bareOnly := range commands {
		if trimmed == command {
			return true
		}
		if !bareOnly && strings.HasPrefix(trimmed, command+" ") {
			return true
		}
	}
	return false
}
