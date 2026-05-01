package main

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/keakon/chord-gateway/config"
)

// newTestChordManager creates a ChordManager with cfg pre-loaded for tests.
// Avoids inline struct literals that would conflict with cfg's atomic.Pointer type.
func newTestChordManager(cfg *config.Config) *ChordManager {
	mgr := &ChordManager{procs: make(map[string]*ChordProcess)}
	mgr.cfg.Store(cfg)
	return mgr
}

type sentMessage struct {
	chatID string
	text   string
}

type stubIMAdapter struct {
	typ string

	connectFunc    func() error
	sendFunc       func(chatID, text string) error
	disconnectFunc func()
	startLoginFunc func() (string, error)

	mu              sync.Mutex
	connectCalls    int
	disconnectCalls int
	startLoginCalls int
	sent            []sentMessage
}

func (a *stubIMAdapter) Connect() error {
	a.mu.Lock()
	a.connectCalls++
	fn := a.connectFunc
	a.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return nil
}

func (a *stubIMAdapter) SendText(chatID, text string) error {
	a.mu.Lock()
	a.sent = append(a.sent, sentMessage{chatID: chatID, text: text})
	fn := a.sendFunc
	a.mu.Unlock()
	if fn != nil {
		return fn(chatID, text)
	}
	return nil
}

func (a *stubIMAdapter) Disconnect() {
	a.mu.Lock()
	a.disconnectCalls++
	fn := a.disconnectFunc
	a.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (a *stubIMAdapter) Type() string { return a.typ }

func (a *stubIMAdapter) StartLogin() (string, error) {
	a.mu.Lock()
	a.startLoginCalls++
	fn := a.startLoginFunc
	a.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return "", ErrLoginNotSupported
}

func (a *stubIMAdapter) sentMessages() []sentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]sentMessage, len(a.sent))
	copy(out, a.sent)
	return out
}

func (a *stubIMAdapter) lastMessage() sentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sent) == 0 {
		return sentMessage{}
	}
	return a.sent[len(a.sent)-1]
}

type captureWriteCloser struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
}

func (w *captureWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *captureWriteCloser) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *captureWriteCloser) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func makeFakeChordBinary(t testingT, behavior string) string {
	path, _ := makeFakeChordBinaryWithArgsFile(t, behavior)
	return path
}

func makeFakeChordBinaryWithArgsFile(t testingT, behavior string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/fake-chord.sh"
	argsFile := dir + "/args.txt"
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsFile) + "\n" +
		"case " + shellQuote(behavior) + " in\n" +
		"  fail) exit 42 ;;\n" +
		"esac\n" +
		"while IFS= read -r line; do :; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake chord binary: %v", err)
	}
	return path, argsFile
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

type testingT interface {
	Helper()
	TempDir() string
	Fatalf(string, ...any)
}

func readFakeChordArgs(t testingT, path string) string {
	t.Helper()
	var data []byte
	var err error
	for i := 0; i < 50; i++ {
		data, err = os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("read fake chord args %s: %v", path, err)
	return ""
}

func requireContains(t testingT, got, want string) {
	t.Helper()
	if !bytes.Contains([]byte(got), []byte(want)) {
		t.Fatalf("got %q, want to contain %q", got, want)
	}
}
