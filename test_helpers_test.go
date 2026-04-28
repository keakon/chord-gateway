package main

import (
	"bytes"
	"sync"
)

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
