package main

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func TestDiscordPairingAndResetPreserveCredentialsAndOtherPlatforms(t *testing.T) {
	r, _ := pairedTestRouterWithConfig(t, "ims: {wechat: {}, discord: {bot_token: example-token, application_id: '123456789012345678'}}\n")
	beforeWechat := r.getConfig().IMConfigByType("wechat")
	var terminal bytes.Buffer
	p := r.admission.pairing
	p.terminal = &terminal
	const botID = "234567890123456789"
	if err := p.PlatformReadyWithPresentation("discord", botID, PlatformPresentation{BotMention: "<@" + botID + ">"}); err != nil {
		t.Fatal(err)
	}
	var code string
	fields := strings.Fields(terminal.String())
	for i, field := range fields {
		if field == "\"~/workspace\"" && i+1 < len(fields) {
			code = fields[i+1]
		}
	}
	if code == "" {
		t.Fatal("Discord pairing instructions missing")
	}
	msg := IncomingMessage{IMType: "discord", AccountID: botID, ChatKind: "private", ChatID: "345678901234567890", SenderID: "456789012345678901", MessageID: "message:567890123456789012", Text: fmt.Sprintf("/bind %q %s", t.TempDir(), code)}
	group := msg
	group.ChatKind = "group"
	if _, ok := r.admission.Authorize(group); ok {
		t.Fatal("Discord first pairing admitted outside DM")
	}
	if !dispatchPairingTest(r, msg) {
		t.Fatal("Discord private pairing rejected")
	}
	paired := r.getConfig().IMConfigByType("discord")
	if owner := paired.OwnerIdentity(); owner == nil || owner.AccountID != botID || owner.UserID != msg.SenderID {
		t.Fatal("Discord pairing did not persist verified bot and owner IDs")
	}
	workspace, err := r.getConfig().ResolveWorkspace("discord", msg.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	workspaceCount := len(r.getConfig().Workspaces)
	paths := &config.Paths{StateDir: t.TempDir()}
	pins := newSessionPinStore(paths.StateDir)
	ownPin := pinKeyForBinding((processKey{workspaceID: workspace.ID, imType: "discord", chatID: msg.ChatID}).String())
	otherPin := (processKey{workspaceID: "other", imType: "wecom", chatID: "other-chat"}).String()
	for key, session := range map[string]string{ownPin: "discord-session", otherPin: "other-session"} {
		if err := pins.Set(key, session); err != nil {
			t.Fatal(err)
		}
	}
	// Resetting another configured platform must tolerate and preserve Discord pins.
	if err := resetPairing(t.Context(), r.mgr.store, paths, "wechat"); err != nil {
		t.Fatal(err)
	}
	if err := pins.Load(); err != nil {
		t.Fatal(err)
	}
	if pins.Get(ownPin) != "discord-session" || pins.Get(otherPin) != "other-session" {
		t.Fatal("resetting WeChat changed another platform's pins")
	}
	if err := resetPairing(t.Context(), r.mgr.store, paths, "discord"); err != nil {
		t.Fatal(err)
	}
	if err := pins.Load(); err != nil {
		t.Fatal(err)
	}
	if pins.Get(ownPin) != "" || pins.Get(otherPin) != "other-session" {
		t.Fatal("Discord reset did not isolate platform pins")
	}
	reloaded, err := NewConfigStore(r.mgr.store.path)
	if err != nil {
		t.Fatal(err)
	}
	after := reloaded.Snapshot().IMConfigByType("discord")
	if after.OwnerIdentity() != nil || len(after.Bindings()) != 0 {
		t.Fatal("Discord reset retained identity or routes")
	}
	if after.Discord.BotToken != "example-token" || after.Discord.ApplicationID != "123456789012345678" {
		t.Fatal("Discord reset changed credentials")
	}
	if !reflect.DeepEqual(beforeWechat, reloaded.Snapshot().IMConfigByType("wechat")) || len(reloaded.Snapshot().Workspaces) != workspaceCount {
		t.Fatal("Discord reset changed other platform or workspaces")
	}
}
