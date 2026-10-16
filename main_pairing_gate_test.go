package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func pairedOwnerTestConfig(t *testing.T, feishuTail string) *config.Config {
	t.Helper()
	data := "ims:\n  feishu: {app_id: app, app_secret: secret" + feishuTail + "}\n  discord: {bot_token: token, application_id: app}\nworkspaces: {project: {path: /project}}\n"
	cfg, _, _, err := config.ParseDocument([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCheckPairedOwnerRequirement(t *testing.T) {
	bound := pairedOwnerTestConfig(t, ", chat_bindings: {chat: project}")

	if err := checkPairedOwnerRequirement(bound, false); err == nil || !strings.Contains(err.Error(), "restart with --pair") {
		t.Fatalf("unpaired bindings without --pair must fail with guidance: %v", err)
	}
	if err := checkPairedOwnerRequirement(bound, true); err != nil {
		t.Fatalf("--pair must allow dormant bindings: %v", err)
	}

	unbound := pairedOwnerTestConfig(t, "")
	if err := checkPairedOwnerRequirement(unbound, false); err != nil {
		t.Fatalf("platforms without bindings must not block startup: %v", err)
	}

	paired := pairedOwnerTestConfig(t, ", owner: {account_id: app, user_id: owner}, chat_bindings: {chat: project}")
	if err := checkPairedOwnerRequirement(paired, false); err != nil {
		t.Fatalf("paired platforms must not block startup: %v", err)
	}
}

// Console mode never connects IM platforms, so the chat-binding pairing
// prerequisite must not apply there: its --pair remedy is mutually exclusive
// with --console. The gateway must start despite dormant, ownerless bindings.
func TestConsoleModeSkipsPairedOwnerGate(t *testing.T) {
	if path := os.Getenv("GATEWAY_TEST_CONSOLE_GATE_CONFIG"); path != "" {
		os.Args = []string{"chord-gateway", "--console", "--config", path}
		main()
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := "ims:\n  feishu: {app_id: app, app_secret: secret, chat_bindings: {chat: project}}\nworkspaces: {project: {path: " + filepath.ToSlash(dir) + "}}\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestConsoleModeSkipsPairedOwnerGate$")
	cmd.Env = append(os.Environ(), "GATEWAY_TEST_CONSOLE_GATE_CONFIG="+path, "CHORD_GATEWAY_STATE_DIR="+filepath.Join(dir, "state"), "CHORD_GATEWAY_LOG_FILE="+filepath.Join(dir, "gateway.log"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("console startup must skip the IM pairing gate: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "restart with --pair") {
		t.Fatalf("console mode must not ask for --pair: %s", out)
	}
}
