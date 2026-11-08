//go:build !windows

package gateway

import (
	"bufio"
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestBindingStopWaitsAfterForcedKill(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `trap '' TERM; printf 'ready\n'; exec sleep 30`)
	configureProcessGroup(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatal("child did not initialize")
	}
	p := &ChordProcess{cmd: cmd, pgid: processGroupID(cmd), waitDone: make(chan struct{})}
	go p.waitAndRecordExit(cmd)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err = p.StopAndWait(ctx, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.waitDone:
	default:
		t.Fatal("returned before Wait completed")
	}
	if bindingGroupAlive(p.pgid) {
		t.Fatal("process group survived")
	}
	if err = p.StopAndWait(ctx, 20*time.Millisecond); err != nil {
		t.Fatal("second observer could not confirm exit")
	}
}

// StopAndWait shares the stop bookkeeping with TerminateGroup: an interrupted
// transfer must be flagged no matter which stop path wins the exitOnce race.
func TestStopAndWaitMarksTransferInterrupted(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	m := newTestChordManager(testConfig())
	m.transfers = newSessionTransferStore(t.TempDir())
	key := (processKey{workspaceID: "test", imType: "wechat", chatID: "chat"}).String()
	record := sessionTransfer{Direction: "outgoing", BindingInstanceID: "binding-1", Status: TransferStatus{TransferID: "transfer-1", State: "waiting"}}
	if err := m.transfers.update(key, &record, ""); err != nil {
		t.Fatalf("seed transfer record: %v", err)
	}
	p := &ChordProcess{mgr: m, key: key, cmd: cmd, pgid: processGroupID(cmd), waitDone: make(chan struct{}), bindingInstanceID: "binding-1"}
	go p.waitAndRecordExit(cmd)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.StopAndWait(ctx, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	got, ok, err := m.transfers.Get(key)
	if err != nil || !ok {
		t.Fatalf("get transfer record: ok=%v err=%v", ok, err)
	}
	if !got.Interrupted {
		t.Fatal("StopAndWait did not mark the transfer interrupted")
	}
	// The loser of the exitOnce race must neither panic nor undo bookkeeping.
	p.TerminateGroup(10 * time.Millisecond)
	if got, _, _ = m.transfers.Get(key); !got.Interrupted {
		t.Fatal("TerminateGroup reset the interrupted marker")
	}
}
