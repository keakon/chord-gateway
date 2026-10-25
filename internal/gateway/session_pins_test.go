package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func TestChordManager_SpawnArgsForWorkspace_UsesPinnedSessionID(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Wechat: &config.WechatConfig{}}}, SessionPinsFile: filepath.Join(dir, "pins.json")}
	paths, err := config.Resolve("", filepath.Join(dir, "config.yaml"), dir, filepath.Join(dir, "gateway.log"), "", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	m := NewChordManager(cfg, paths)

	if got := m.spawnArgsForKey(processKey{workspaceID: "default", imType: "wechat", chatID: "chat"}.String()); got != nil {
		t.Fatalf("expected nil args when no pin, got %v", got)
	}

	if err := m.pins.Set(pinKeyForBinding(processKey{workspaceID: "default", imType: "wechat", chatID: "chat"}.String()), "123"); err != nil {
		t.Fatalf("set pin: %v", err)
	}
	got := m.spawnArgsForKey(processKey{workspaceID: "default", imType: "wechat", chatID: "chat"}.String())
	want0, want1 := "--resume", "123"
	if len(got) != 2 || got[0] != want0 || got[1] != want1 {
		t.Fatalf("args = %v, want [%q %q]", got, want0, want1)
	}

	// Ensure persistence file exists.
	if _, err := os.Stat(filepath.Join(dir, "pins.json")); err != nil {
		t.Fatalf("pins file missing: %v", err)
	}
}

// Entries under retired layouts can never be addressed by a binding again.
// Loading drops them instead of rewriting them onto some other identity, and
// leaves every remaining entry untouched. Pipe-joined pre-v2 keys are retired
// the same way: parseProcessKey no longer addresses them and Get can never
// match them, so retaining them only pollutes ClearPlatform scans.
func TestSessionPinLoadDiscardsRetiredScopedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-pins.json")
	keptKey := processKey{workspaceID: "default", imType: "wechat", chatID: "chat"}.String()
	raw := `{"paired:v1:Zm9v:YmFy:default|wechat|chat":"scoped","paired:unowned:default|feishu|chat":"unowned","ws-plain|wechat|chat":"legacy-pipe",` + `"` + keptKey + `":"kept"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newSessionPinStore(dir)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if got := store.Get(keptKey); got != "kept" {
		t.Fatalf("addressable pin = %q, want kept", got)
	}
	for _, key := range []string{"paired:v1:Zm9v:YmFy:default|wechat|chat", "paired:unowned:default|feishu|chat", "ws-plain|wechat|chat"} {
		if got := store.Get(key); got != "" {
			t.Fatalf("retired key %q retained %q", key, got)
		}
	}
}
