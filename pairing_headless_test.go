//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPairingHeadlessStartsOnlyAfterOwnerTask(t *testing.T) {
	r, code := pairedTestRouter(t)
	root := t.TempDir()
	binary := filepath.Join(root, "headless.sh")
	argsFile := filepath.Join(root, "arguments")
	inputFile := filepath.Join(root, "input")
	script := `#!/bin/sh
case "$*" in *--help*|*--version*) exit 0;; esac
printf '%s\n' "$@" > ` + shellQuote(argsFile) + `
printf '{"type":"ready","payload":{"session_id":"session-example"}}\n'
while IFS= read -r line; do
 printf '%s\n' "$line" >> ` + shellQuote(inputFile) + `
 case "$line" in
  *'"type":"send"'*)
   request=$(printf '%s\n' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
   printf '{"type":"input_result","seq":1,"payload":{"request_id":"%s","status":"handled"}}\n' "$request"
  ;;
 esac
done
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.mgr.store.Commit(context.Background(), testConfigMutation("chord_path", binary)); err != nil {
		t.Fatal(err)
	}
	first, second := t.TempDir(), t.TempDir()
	msg := IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "bind", Text: fmt.Sprintf("/bind %q %s", first, code)}
	if !dispatchPairingTest(r, msg) {
		t.Fatal("pairing rejected")
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("pairing started headless")
	}
	stranger := msg
	stranger.SenderID = "stranger"
	stranger.MessageID = "unauthorized"
	stranger.Text = "unauthorized task"
	if dispatchPairingTest(r, stranger) {
		t.Fatal("stranger admitted")
	}
	completed := make(chan struct{}, 1)
	r.mgr.SetOnEvent(func(_, event string, _ ControlState) {
		if event == "input_result" {
			select {
			case completed <- struct{}{}:
			default:
			}
		}
	})
	msg.MessageID = "task"
	msg.Text = "authorized task"
	if !dispatchPairingTest(r, msg) {
		t.Fatal("owner task rejected")
	}
	t.Cleanup(func() { r.mgr.StopAll(100 * time.Millisecond) })
	select {
	case <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("headless did not acknowledge owner input")
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _, _ := canonicalBindingDirectory(first)
	if !strings.Contains(string(args), "-d\n"+canonical+"\n") {
		t.Fatal("headless started in the wrong directory")
	}
	input, err := os.ReadFile(inputFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(input), code) || strings.Contains(string(input), "/bind") {
		t.Fatal("binding credential or command reached headless")
	}
	msg.MessageID = "switch"
	msg.Text = fmt.Sprintf("/bind %q", second)
	dispatchPairingTest(r, msg)
	reloaded, err := NewConfigStore(r.mgr.store.path)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := reloaded.Snapshot().ResolveWorkspace("wechat", "chat")
	canonical, _, _ = canonicalBindingDirectory(second)
	if err != nil || ws.Path != canonical {
		t.Fatal("rebound route was not durable")
	}
}
