package gateway

import "testing"

// TestIsChordLocalOnlySlashCommand pins the surface against Chord's own
// IsTUILocalOnlySlashCommand predicate. A drift here silently reopens the
// permission hole: anything Chord runs locally but the gateway forwards takes
// effect without the gateway ever learning about it.
func TestIsChordLocalOnlySlashCommand(t *testing.T) {
	refused := []string{
		"/export", "/export report.md",
		"/tier", "/tier cheap",
		"/rename", "/rename nightly",
		"/yolo", "/yolo on", "/yolo off",
		"/compact", "  /export  ",
	}
	for _, content := range refused {
		if !isChordLocalOnlySlashCommand(content) {
			t.Errorf("isChordLocalOnlySlashCommand(%q) = false, want true", content)
		}
	}

	// These look similar but Chord routes them as ordinary user messages, so
	// refusing them would swallow legitimate input.
	// /queue, /models, /role and /mcp are Chord local-only slashes, so they
	// would normally belong to chordLocalOnlySlash. Gateway-owned commands
	// are checked separately when input bypasses command parsing.
	forwarded := []string{
		"/skill review-pr",     // only bare /skill opens the selector
		"/compact now",         // Chord matches /compact exactly
		"/COMPACT", "/Yolo ON", // Chord's local-only predicate is case-sensitive
		"/model", "/model gpt", // no such command in Chord
		"/models", "/models-info",
		"/queue", "/queue clear", "/queue, clearing",
		"/yoloing",
		"", "   ", "plain text", "/status",
	}
	for _, content := range forwarded {
		if isChordLocalOnlySlashCommand(content) {
			t.Errorf("isChordLocalOnlySlashCommand(%q) = true, want false", content)
		}
	}
}

func TestGatewayOwnedSlashIncludesGatewayCommandRoutes(t *testing.T) {
	for _, content := range []string{"/models", "/models dev-pool", "/role", "/role planner", "/mcp", "/mcp status", "/mcp enable", "/queue", "/queue clear", "/queue after hi"} {
		if !isGatewayOwnedSlashCommand(content) {
			t.Errorf("isGatewayOwnedSlashCommand(%q) = false, want true", content)
		}
	}
}

// TestLocalOnlySlashCommandsMirrorChord guards the list itself. Entries here
// must stay a subset of IsTUILocalOnlySlashCommand in ../chord, minus the
// commands the gateway answers itself: /role, /mcp, /models and /queue all have
// gateway routes, so forwarding them verbatim would bypass those rather than
// reaching Chord's local handler.
func TestLocalOnlySlashCommandsMirrorChord(t *testing.T) {
	// The set Chord treats as TUI-local-only.
	chordSet := map[string]bool{
		"/queue": true, "/export": true, "/models": true, "/tier": true,
		"/rename": true, "/compact": true, "/yolo": true, "/mcp": true,
		"/skill": true, "/role": true,
	}
	for command := range chordLocalOnlySlash {
		if !chordSet[command] {
			t.Errorf("%s is not local-only for Chord; refusing it would reject ordinary messages", command)
		}
	}
	gatewayRoutes := map[string]bool{"/role": true, "/mcp": true, "/models": true, "/queue": true}
	for command := range chordSet {
		_, localOnly := chordLocalOnlySlash[command]
		switch {
		case localOnly:
		case gatewayRoutes[command]:
			if !isGatewayOwnedSlashCommand(command) {
				t.Errorf("%s has a gateway route but is not guarded against ForceText", command)
			}
		default:
			t.Errorf("%s is neither refused nor mapped to a gateway route", command)
		}
	}
}

func TestIsGatewayOwnedSlashCommand(t *testing.T) {
	for _, content := range []string{"/new", "/resume", "/resume session-9", "/NEW"} {
		if !isGatewayOwnedSlashCommand(content) {
			t.Errorf("isGatewayOwnedSlashCommand(%q) = false, want true", content)
		}
	}
	for _, content := range []string{"/newy", "/resumed", "", "/roles"} {
		if isGatewayOwnedSlashCommand(content) {
			t.Errorf("isGatewayOwnedSlashCommand(%q) = true, want false", content)
		}
	}
}
