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
		"/COMPACT", "/Yolo ON", "  /export  ",
	}
	for _, content := range refused {
		if !isChordLocalOnlySlashCommand(content) {
			t.Errorf("isChordLocalOnlySlashCommand(%q) = false, want true", content)
		}
	}

	// These look similar but Chord routes them as ordinary user messages, so
	// refusing them would swallow legitimate input.
	// /queue, /models and /role are Chord local-only slashes, so they would
	// normally belong to chordLocalOnlySlash. They are absent because the
	// gateway implements the input_queue, models and role protocol commands now.
	forwarded := []string{
		"/skill review-pr",     // only bare /skill opens the selector
		"/compact now",         // Chord matches /compact exactly
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

func TestGatewayOwnedSlashIncludesModelsAndRole(t *testing.T) {
	for _, content := range []string{"/models", "/models dev-pool", "/role", "/role planner", "/queue", "/queue clear", "/queue after hi"} {
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
	if gatewayRoutes := len(chordSet) - len(chordLocalOnlySlash); gatewayRoutes != 4 {
		t.Errorf("%d Chord command(s) unaccounted for, want exactly 4 mapped to gateway routes (/role, /mcp, /models, /queue)", gatewayRoutes)
	}
	// The four Chord considers local-only but the gateway owns must actually
	// route to gateway handling, not fall through to the process.
	for _, command := range []string{"/role", "/models", "/mcp", "/queue"} {
		if chordLocalOnlySlash[command] {
			t.Errorf("%s has a gateway route and must not be refused as local-only", command)
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
