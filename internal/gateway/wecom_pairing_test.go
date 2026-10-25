package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"

	wecom "github.com/keakon/chord-gateway/internal/platform/wecom"
)

func TestWecomPairingOwnerAccountAndSecretRotation(t *testing.T) {
	for _, tc := range []struct {
		name, bot, secret string
		allowed           bool
	}{
		{"original", "example-bot", "original-secret", true},
		{"secret_rotation", "example-bot", "rotated-secret", true},
		{"different_bot", "other-bot", "rotated-secret", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testConfigStore(t, fmt.Sprintf("ims: {wecom: {bot_id: %s, secret: %s, owner: {account_id: example-bot, user_id: example-owner}}}\n", tc.bot, tc.secret))
			var terminal bytes.Buffer
			pairing := NewPairingService(store, true, &terminal)
			err := pairing.PlatformReady("wecom", tc.bot)
			if (err == nil) != tc.allowed || !tc.allowed && !errors.Is(err, im.ErrPairingDenied) {
				t.Fatalf("owner account authorization: %v", err)
			}
			if terminal.Len() != 0 {
				t.Fatal("existing owner generated new pairing credential")
			}
		})
	}
}

func TestWecomPairingAccountChangeRevokesPendingCode(t *testing.T) {
	store := testConfigStore(t, "ims: {wecom: {bot_id: example-bot, secret: example-secret}}\n")
	var terminal bytes.Buffer
	pairing := NewPairingService(store, true, &terminal)
	admission := NewAdmission(pairing)
	t.Cleanup(admission.Close)
	if err := pairing.PlatformReady("wecom", "example-bot"); err != nil {
		t.Fatal(err)
	}
	var code string
	fields := strings.Fields(terminal.String())
	for i, field := range fields {
		if field == `"<directory>"` {
			code = fields[i+1]
		}
	}
	chat, _ := wecom.EncodeWecomTarget("private", "example-owner")
	msg := im.IncomingMessage{IMType: "wecom", AccountID: "example-bot", ChatKind: "private", ChatID: chat, SenderID: "example-owner", MessageID: "message:example", Text: "/bind /project " + code}
	ticket, ok := admission.Authorize(msg)
	if !ok {
		t.Fatal("pairing denied before account changed")
	}
	if err := pairing.PlatformReady("wecom", "other-bot"); !errors.Is(err, im.ErrPairingDenied) {
		t.Fatalf("account change error: %v", err)
	}
	if admission.Valid(ticket) {
		t.Fatal("old pairing ticket survived bot account change")
	}
	if err := pairing.PlatformReady("wecom", "example-bot"); !errors.Is(err, im.ErrPairingDenied) {
		t.Fatal("returning to old account silently reopened pairing")
	}
}

func TestWecomLocalResetClearsIdentityAndPreservesCredentials(t *testing.T) {
	store := testConfigStore(t, "ims: {wecom: {bot_id: new-bot, secret: example-secret, owner: {account_id: old-bot, user_id: example-owner}, chat_bindings: {example-chat: project}}, feishu: {app_id: example-app, app_secret: other-secret, owner: {account_id: example-app, user_id: other-owner}}}\nworkspaces: {project: {path: /project}}\n")
	paths := &config.Paths{StateDir: t.TempDir()}
	if err := resetPairing(context.Background(), store, paths, "wecom"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewConfigStore(store.path)
	if err != nil {
		t.Fatal(err)
	}
	imCfg := reloaded.Snapshot().IMConfigByType("wecom")
	if imCfg.OwnerIdentity() != nil || len(imCfg.Bindings()) != 0 || imCfg.Wecom.BotID != "new-bot" || imCfg.Wecom.Secret != "example-secret" || reloaded.Snapshot().WorkspaceByID("project") == nil || reloaded.Snapshot().IMConfigByType("feishu").OwnerIdentity().UserID != "other-owner" {
		t.Fatal("reset changed credentials, unrelated platform or workspace")
	}
	pairing := NewPairingService(reloaded, false, nil)
	if err = pairing.PlatformReady("wecom", "new-bot"); err != nil {
		t.Fatalf("reset did not permit new bot account: %v", err)
	}
}
