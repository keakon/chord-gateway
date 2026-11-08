package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A child that stops consuming stdin must not wedge writeCommandLocked: the
// write is bounded by a deadline, and the deadline failure closes stdin so
// subsequent commands fail fast instead of blocking again.
func TestWriteCommandLockedStdinDeadline(t *testing.T) {
	oldTimeout := stdinWriteTimeout
	stdinWriteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { stdinWriteTimeout = oldTimeout })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()

	p := &ChordProcess{key: "sample"}
	p.stdin = w

	// Nobody drains r and the payload far exceeds the pipe buffer, so the
	// write blocks until the deadline aborts it.
	p.mu.Lock()
	err = p.writeCommandLocked(bytes.Repeat([]byte("a"), 1<<18))
	p.mu.Unlock()
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("writeCommandLocked err = %v, want deadline exceeded", err)
	}
	if p.stdin != nil {
		t.Fatal("stdin must be cleared after a deadline write failure")
	}

	start := time.Now()
	p.mu.Lock()
	err = p.writeCommandLocked([]byte(`{"type":"status"}`))
	p.mu.Unlock()
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("subsequent write err = %v, want io.ErrClosedPipe", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("subsequent write took %v, want fast failure", elapsed)
	}
}

type stdinWithoutDeadline struct{ io.WriteCloser }

type stdinUnsupportedDeadline struct{ io.WriteCloser }

func (stdinUnsupportedDeadline) SetWriteDeadline(time.Time) error { return os.ErrNoDeadline }

func TestWriteCommandLockedUnsupportedStdinDeadline(t *testing.T) {
	swapTestValue(t, &stdinWriteTimeout, 20*time.Millisecond)
	for _, tc := range []struct {
		name string
		wrap func(io.WriteCloser) io.WriteCloser
	}{
		{"unsupported", func(w io.WriteCloser) io.WriteCloser { return stdinUnsupportedDeadline{w} }},
		{"unavailable", func(w io.WriteCloser) io.WriteCloser { return stdinWithoutDeadline{w} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
			p := &ChordProcess{key: "sample", stdin: tc.wrap(w)}
			done := make(chan error, 1)
			go func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				done <- p.writeCommandLocked(bytes.Repeat([]byte("a"), 1<<18))
			}()
			select {
			case err := <-done:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("write error = %v, want deadline exceeded", err)
				}
			case <-time.After(time.Second):
				_ = r.Close()
				<-done
				t.Fatal("unsupported deadline left the write blocked")
			}
			if p.stdin != nil {
				t.Fatal("timed-out stdin was not cleared")
			}
			if err := p.writeCommandLocked([]byte(`{"type":"status"}`)); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("next write error = %v, want closed pipe", err)
			}
		})
	}
}

type stdinWriteResult struct {
	bytes.Buffer
	n      int
	err    error
	closed atomic.Bool
}

func (w *stdinWriteResult) Write(b []byte) (int, error) {
	if w.n >= 0 {
		return w.n, w.err
	}
	return w.Buffer.Write(b)
}

func (w *stdinWriteResult) Close() error { w.closed.Store(true); return nil }

func TestWriteCommandLockedStopsFallbackWatchdogOnReturn(t *testing.T) {
	writeErr := errors.New("write failed")
	for _, tc := range []struct {
		name string
		n    int
		err  error
		want error
	}{
		{"complete", -1, nil, nil},
		{"short", 1, nil, io.ErrShortWrite},
		{"failed", 0, writeErr, writeErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				writer := &stdinWriteResult{n: tc.n, err: tc.err}
				p := &ChordProcess{key: "sample", stdin: writer}
				if err := p.writeCommandLocked([]byte(`{"type":"status"}`)); !errors.Is(err, tc.want) {
					t.Fatalf("write error = %v, want %v", err, tc.want)
				}
				if tc.want == nil && writer.String() != "{\"type\":\"status\"}\n" {
					t.Fatalf("written command = %q", writer.String())
				}
				time.Sleep(stdinWriteTimeout + time.Second)
				synctest.Wait()
				if writer.closed.Load() || p.stdin != writer {
					t.Fatal("watchdog closed stdin after the write had returned")
				}
			})
		})
	}
}

func TestStdinBlockedHelperProcess(t *testing.T) {
	if os.Getenv("CHORD_GATEWAY_STDIN_TEST_HELPER") != "1" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	time.Sleep(time.Hour)
	os.Exit(0)
}

func TestWriteCommandLockedDeadlineKillsChild(t *testing.T) {
	swapTestValue(t, &stdinWriteTimeout, 50*time.Millisecond)
	cmd := exec.Command(os.Args[0], "-test.run=^TestStdinBlockedHelperProcess$")
	cmd.Env = append(os.Environ(), "CHORD_GATEWAY_STDIN_TEST_HELPER=1")
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = stdin.Close()
		_ = stdout.Close()
		<-waited
	})
	ready := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(stdout, ready); err != nil {
		go func() { waited <- cmd.Wait() }()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	p := &ChordProcess{key: "sample", cmd: cmd, pgid: processGroupID(cmd), stdin: stdinUnsupportedDeadline{stdin}}
	go func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		done <- p.writeCommandLocked(bytes.Repeat([]byte("a"), 1<<18))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("write error = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Error("blocked write did not return")
	}
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		waited <- err
		if err == nil || cmd.ProcessState.Success() {
			t.Fatal("deadline did not kill the blocked child")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("child survived the stdin deadline")
	}
}
