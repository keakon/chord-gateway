package main

import (
	"io"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

type consoleMessageRecorder struct{ messages chan im.IncomingMessage }

func (r *consoleMessageRecorder) HandleIncomingMessage(m im.IncomingMessage) { r.messages <- m }

func TestConsoleInputAndDisconnect(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	recorder := &consoleMessageRecorder{messages: make(chan im.IncomingMessage, 1)}
	a := newConsoleAdapter(recorder)
	a.input = reader
	defer a.Disconnect()
	done := make(chan error, 1)
	go func() { done <- a.Connect() }()
	if _, err := io.WriteString(writer, "\n/status\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-recorder.messages:
		if msg.IMType != "console" || msg.ChatID != "console" || msg.Text != "/status" {
			t.Fatalf("message=%+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("input not dispatched")
	}
	a.Disconnect()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stdin reader did not stop")
	}
}
