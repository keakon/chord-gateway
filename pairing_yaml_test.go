package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func TestPairingPreservesEffectiveYAMLConfiguration(t *testing.T) {
	for _, platformKey := range []string{"wechat", "WeChat", " weCHAT "} {
		for _, merged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/merged=%t", platformKey, merged), func(t *testing.T) {
				data := fmt.Sprintf(`ims:
  %q: {}
  feishu:
    app_id: app
    app_secret: example-secret
    owner: {account_id: app, user_id: other-owner}
    chat_bindings: {existing-chat: original}
workspaces:
  original: {path: %q}
`, platformKey, t.TempDir())
				if merged {
					// A top-level merge must retain all effective keys.
					data = "<<:\n  " + strings.ReplaceAll(strings.TrimSuffix(data, "\n"), "\n", "\n  ") + "\n"
				}
				r, code := pairedTestRouterWithConfig(t, data)
				before := r.getConfig()
				_, key := bindTestOwner(t, r, code, t.TempDir())
				store := r.mgr.store
				checkOther := func(cfg *config.Config) {
					t.Helper()
					if !reflect.DeepEqual(cfg.IMConfigByType("feishu"), before.IMConfigByType("feishu")) ||
						!reflect.DeepEqual(cfg.WorkspaceByID("original"), before.WorkspaceByID("original")) {
						t.Fatal("unrelated platform, route or workspace changed")
					}
				}
				checkOther(store.Snapshot())
				paths := &config.Paths{StateDir: t.TempDir()}
				pins := newSessionPinStore(paths.StateDir)
				ownPin := pinKeyForBinding(store.Snapshot(), key)
				otherPin := pinKeyForBinding(store.Snapshot(), (processKey{workspaceID: "original", imType: "feishu", chatID: "existing-chat"}).String())
				for pin, session := range map[string]string{ownPin: "own-session", otherPin: "other-session"} {
					if err := pins.Set(pin, session); err != nil {
						t.Fatal(err)
					}
				}
				if err := resetPairing(t.Context(), store, paths, "wechat"); err != nil {
					t.Fatal(err)
				}
				reloaded, err := NewConfigStore(store.path)
				if err != nil {
					t.Fatal(err)
				}
				checkOther(reloaded.Snapshot())
				if reloaded.Snapshot().IMConfigByType("wechat").OwnerIdentity() != nil {
					t.Fatal("reset retained the owner")
				}
				if _, err := reloaded.Snapshot().ResolveWorkspace("wechat", "chat"); err == nil {
					t.Fatal("reset retained the chat binding")
				}
				if err := pins.Load(); err != nil {
					t.Fatal(err)
				}
				if pins.Get(ownPin) != "" || pins.Get(otherPin) != "other-session" {
					t.Fatal("reset did not isolate platform pins")
				}
			})
		}
	}
}

func TestPairingResetPreservesRootMerge(t *testing.T) {
	s := testConfigStore(t, "<<:\n  ims:\n    WeChat: {owner: {account_id: bot, user_id: owner}}\n    feishu: {app_id: app, app_secret: example-secret, owner: {account_id: app, user_id: other-owner}}\n")
	other := s.Snapshot().IMConfigByType("feishu")
	if err := resetPairing(t.Context(), s, &config.Paths{StateDir: t.TempDir()}, "wechat"); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().IMConfigByType("wechat").OwnerIdentity() != nil || !reflect.DeepEqual(other, s.Snapshot().IMConfigByType("feishu")) {
		t.Fatal("reset changed the wrong effective platform configuration")
	}
}
