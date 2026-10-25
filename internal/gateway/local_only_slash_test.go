package gateway

import "testing"

// TestIsChordLocalOnlySlashCommand pins the surface against Chord's own
// IsTUILocalOnlySlashCommand predicate. A drift here silently reopens the
// permission hole: anything Chord runs locally but the gateway forwards takes
// effect without the gateway ever learning about it.
func TestIsChordLocalOnlySlashCommand(t *testing.T) {
	refused := []string{
		"/queue", "/queue clear",
		"/export", "/export report.md",
		"/models", "/models dev-pool",
		"/tier", "/tier cheap",
		"/rename", "/rename nightly",
		"/yolo", "/yolo on", "/yolo off",
		"/COMPACT", "/Yolo ON", "  /queue  ",
	}
	for _, content := range refused {
		if !isChordLocalOnlySlashCommand(content) {
			t.Errorf("isChordLocalOnlySlashCommand(%q) = false, want true", content)
		}
	}

	// These look similar but Chord routes them as ordinary user messages, so
	// refusing them would swallow legitimate input.
	forwarded := []string{
		"/skill review-pr",     // only bare /skill opens the selector
		"/compact now",         // Chord matches /compact exactly
		"/model", "/model gpt", // no such command in Chord
		"/models-info", "/yoloing",
		"", "   ", "plain text", "/status",
	}
	for _, content := range forwarded {
		if isChordLocalOnlySlashCommand(content) {
			t.Errorf("isChordLocalOnlySlashCommand(%q) = true, want false", content)
		}
	}
}

// TestLocalOnlySlashCommandsMirrorChord guards the list itself. Entries here
// must stay a subset of IsTUILocalOnlySlashCommand in ../chord, minus /role and
// /mcp, which the gateway answers with its own confirmed commands.
func TestLocalOnlySlashCommandsMirrorChord(t *testing.T) {
	// The set Chord treats as TUI-local-only, minus the two the gateway owns.
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
	if gatewayRoles := len(chordSet) - len(chordLocalOnlySlash); gatewayRoles != 2 {
		t.Errorf("%d Chord command(s) unaccounted for, want exactly 2 mapped to gateway routes (/role, /mcp)", gatewayRoles)
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
