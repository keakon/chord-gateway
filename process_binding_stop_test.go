//go:build !windows

package main

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
