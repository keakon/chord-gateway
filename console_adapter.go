package main

import (
	"bufio"
	"context"
	"io"
	"os"
)

// ConsoleAdapter is an explicit local stdin/stdout control plane.
// It never connects to an IM platform or uses IM credentials.
type ConsoleAdapter struct {
	input  io.ReadCloser
	router MessageRouter
	ctx    context.Context
	cancel context.CancelFunc
}

func newConsoleAdapter(router MessageRouter) *ConsoleAdapter {
	ctx, cancel := context.WithCancel(context.Background())
	return &ConsoleAdapter{input: os.Stdin, router: router, ctx: ctx, cancel: cancel}
}
func (*ConsoleAdapter) Type() string                     { return "console" }
func (*ConsoleAdapter) SupportsLoginRenewal() bool       { return false }
func (*ConsoleAdapter) StartLogin() (string, error)      { return "", ErrLoginNotSupported }
func (*ConsoleAdapter) SendText(chat, text string) error { return sendConsoleText(chat, text) }
func (a *ConsoleAdapter) Disconnect()                    { a.cancel(); _ = a.input.Close() }
func (a *ConsoleAdapter) Connect() error {
	scanner := bufio.NewScanner(a.input)
	for scanner.Scan() {
		if a.ctx.Err() != nil {
			return nil
		}
		if text := scanner.Text(); text != "" {
			a.router.HandleIncomingMessage(IncomingMessage{IMType: "console", ChatID: "console", SenderID: "console", SenderName: "console", Text: text})
		}
	}
	if a.ctx.Err() != nil {
		return nil
	}
	return scanner.Err()
}
