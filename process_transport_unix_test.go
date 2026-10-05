//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestHeadlessTransportHelperProcess(t *testing.T) {
	mode := os.Getenv("CHORD_GATEWAY_TRANSPORT_TEST_MODE")
	if mode == "" {
		return
	}
	// Require the parent's force-kill path, including while stdout is blocked.
	signal.Ignore(syscall.SIGTERM)
	fmt.Fprintln(os.Stdout, `{"type":"ready"}`)
	if mode == "oversized" {
		io.Copy(os.Stdout, strings.NewReader(strings.Repeat("x", headlessMaxEnvelopeBytes+64*1024)))
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestReadLoopStopsRealProcessBeforeBlockedNotification(t *testing.T) {
	for _, mode := range []string{"oversized", "read failure"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHeadlessTransportHelperProcess$")
			cmd.WaitDelay = 2 * time.Second
			cmd.Env = append(os.Environ(), "CHORD_GATEWAY_TRANSPORT_TEST_MODE="+mode)
			configureProcessGroup(cmd)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = newTailBuffer(4096)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			p := &ChordProcess{cmd: cmd, stdin: stdin, pgid: processGroupID(cmd), autoRestart: true}
			t.Cleanup(func() { p.TerminateGroup(100 * time.Millisecond) })
			reader := bufio.NewReader(stdout)
			// The handshake proves SIGTERM is ignored before the failure starts.
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			var stream io.Reader = reader
			if mode == "read failure" {
				stream = protocolErrorReader{}
			}
			entered := make(chan ControlState, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var events []string
			p.onEvent = func(_ string, eventType string, state ControlState) {
				events = append(events, eventType)
				if eventType == "transport_error" {
					entered <- state
					<-release // Model an IM adapter that cannot send yet.
				}
			}
			done := make(chan struct{})
			go func() {
				p.readLoop(t.Context(), stream)
				close(done)
			}()
			select {
			case state := <-entered:
				if p.Alive() || !p.stoppedByGateway || !state.LastTransportError.Fatal {
					t.Fatal("transport notification started before the child was stopped")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("transport failure did not stop the child within the grace period")
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child did not reach force-kill: %v", cmd.ProcessState)
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("read loop remained blocked waiting for process exit")
			}
			if strings.Join(events, ",") != "transport_error,exit" || p.Alive() || p.state.Busy {
				t.Fatalf("failed stream did not settle: events=%v state=%+v", events, p.state)
			}
		})
	}
}
