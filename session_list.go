package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/keakon/chord-gateway/config"
)

type sessionListReport struct {
	SchemaVersion int                `json:"schema_version"`
	Workspace     string             `json:"workspace"`
	Sessions      []sessionListEntry `json:"sessions"`
}
type sessionListEntry struct {
	ID         string    `json:"id"`
	LastActive time.Time `json:"last_active"`
	Locked     bool      `json:"locked"`
}

func querySessionList(ctx context.Context, cfg *config.Config, directory string) ([]sessionListEntry, error) {
	args := []string{"sessions", "list", "--json", "--limit", "10", "-d", directory}
	if cfg.ChordStateDir != "" {
		args = append(args, "--state-dir", config.Expand(cfg.ChordStateDir))
	}
	cmd := exec.CommandContext(ctx, cfg.ChordBinary(), args...)
	cmd.Env = loginShellEnv()
	cmd.WaitDelay = 2 * time.Second
	output := &boundedCLIOutput{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("query Chord sessions: %w", err)
	}
	var report sessionListReport
	if err := json.Unmarshal(output.data, &report); err != nil {
		return nil, fmt.Errorf("decode Chord sessions: %w", err)
	}
	if report.SchemaVersion != 1 || report.Workspace == "" || report.Sessions == nil || len(report.Sessions) > 10 {
		return nil, errors.New("invalid Chord session list")
	}
	seen := map[string]bool{}
	for _, session := range report.Sessions {
		if !validPausedInputID(session.ID) || seen[session.ID] || session.LastActive.IsZero() {
			return nil, errors.New("invalid Chord session entry")
		}
		seen[session.ID] = true
	}
	return report.Sessions, nil
}

// Chord owns path resolution, repository sharing and session filtering.
func (r *NotificationRouter) handleSessions(ws *config.Workspace, chatID string, platform ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := querySessionList(ctx, r.getConfig(), ws.Path)
	if err != nil {
		r.sendTextForIM(r.replyPlatform(platform), chatID, "⚠️ Could not query sessions. Check the host and ensure Chord supports sessions list --json.")
		return
	}
	r.sendTextForIM(r.replyPlatform(platform), chatID, formatSessionList(sessions))
}

func formatSessionList(sessions []sessionListEntry) string {
	if len(sessions) == 0 {
		return "📂 No sessions found."
	}
	var lines []string
	for _, session := range sessions {
		suffix := ""
		if session.Locked {
			suffix = " · in use"
		}
		lines = append(lines, fmt.Sprintf("• %s (%s)%s", session.ID, session.LastActive.Local().Format("2006-01-02 15:04"), suffix))
	}
	return "📂 Recent sessions:\n" + strings.Join(lines, "\n")
}
