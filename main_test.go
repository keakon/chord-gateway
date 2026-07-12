package main

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/buildinfo"
)

func TestRootCommandVersionUsesBuildIdentity(t *testing.T) {
	paths := &config.Paths{ConfigFile: "config.yaml"}
	flagConfig := paths.ConfigFile
	cmd := newRootCmd(paths, &flagConfig)

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}

	want := "chord-gateway version " + buildinfo.Current().Short() + "\n"
	if got := out.String(); got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestShutdownGatewayDrainsNotificationsBeforeDisconnect(t *testing.T) {
	var mu sync.Mutex
	disconnected := false
	adapter := &stubIMAdapter{
		typ: "wechat",
		disconnectFunc: func() {
			mu.Lock()
			disconnected = true
			mu.Unlock()
		},
		sendFunc: func(string, string) error {
			mu.Lock()
			defer mu.Unlock()
			if disconnected {
				t.Fatal("queued notification sent after adapter disconnect")
			}
			return nil
		},
	}
	router := NewNotificationRouter(nil)
	router.SetAdapter(adapter)
	if got := router.outbound.enqueue("ws|wechat|chat", func() {
		if err := adapter.SendText("chat", "notification"); err != nil {
			t.Errorf("SendText error: %v", err)
		}
	}); got != outboundQueued {
		t.Fatalf("enqueue result = %v, want outboundQueued", got)
	}

	shutdownGateway(adapter, &ChordManager{procs: make(map[string]*ChordProcess)}, router, time.Millisecond)

	if got := len(adapter.sentMessages()); got != 1 {
		t.Fatalf("sent messages = %d, want 1", got)
	}
	adapter.mu.Lock()
	disconnectCalls := adapter.disconnectCalls
	adapter.mu.Unlock()
	if disconnectCalls != 1 {
		t.Fatalf("Disconnect calls = %d, want 1", disconnectCalls)
	}
}
