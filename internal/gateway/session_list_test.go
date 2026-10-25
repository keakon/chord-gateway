package gateway

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func TestSessionListUsesChordAuthority(t *testing.T) {
	for _, tc := range []struct{ name, output, exit, want string }{
		{"empty", `{"schema_version":1,"workspace":"/project","sessions":[]}`, "0", "No sessions found"},
		{"entries", `{"schema_version":1,"workspace":"/project","sessions":[{"id":"session-2","last_active":"2026-10-01T00:00:00Z","locked":true}]}`, "0", "session-2"},
		{"unsupported", ``, "1", "Could not query sessions"},
		{"invalid", `{"sessions":[]}`, "0", "Could not query sessions"},
		{"null", `{"schema_version":1,"workspace":"/project","sessions":null}`, "0", "Could not query sessions"},
		{"bad entry", `{"schema_version":1,"workspace":"/project","sessions":[{"id":"bad id"}]}`, "0", "Could not query sessions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary, argsFile := takeoverTestCLI(t, tc.output, tc.exit)
			cfg := testConfig()
			cfg.ChordPath = binary
			cfg.ChordStateDir = t.TempDir()
			adapter := &stubIMAdapter{typ: "wechat"}
			r := &NotificationRouter{mgr: newTestChordManager(cfg), adapter: adapter}
			ws := &config.Workspace{ID: "test", Path: t.TempDir()}
			r.handleSessions(ws, "chat", "wechat")
			if got := adapter.lastMessage().text; !strings.Contains(got, tc.want) || strings.Contains(got, "private diagnostic") {
				t.Fatal(got)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			want := "sessions\nlist\n--json\n--limit\n10\n-d\n" + ws.Path + "\n--state-dir\n" + cfg.ChordStateDir + "\n"
			if string(args) != want {
				t.Fatalf("args=%q", args)
			}
		})
	}
}
func TestSessionListCancellation(t *testing.T) {
	binary, _ := takeoverTestCLI(t, `{}`, "0")
	cfg := testConfig()
	cfg.ChordPath = binary
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := querySessionList(ctx, cfg, t.TempDir()); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestChordSessionListContract(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-repo contract")
	}
	binary := mustChordBinary(t)
	configHome, state := t.TempDir(), t.TempDir()
	mustWriteChordTestConfig(t, configHome)
	overrideLoginShellEnv(t, configHome, state)
	cfg := testConfig()
	cfg.ChordPath = binary
	cfg.ChordStateDir = state
	sessions, err := querySessionList(context.Background(), cfg, t.TempDir())
	if err != nil || len(sessions) != 0 {
		t.Fatalf("sessions=%v err=%v", sessions, err)
	}
}
