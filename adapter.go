// Package main defines the IM adapter interface.
package main

import "errors"

// ErrLoginNotSupported is returned by adapters that don't support interactive login.
var ErrLoginNotSupported = errors.New("login not supported by this adapter type")

// IMAdapter is the interface that each IM platform must implement.
type IMAdapter interface {
	// Connect starts the IM connection and blocks until disconnect.
	Connect() error
	// SendText sends a plain text message to the given chat.
	SendText(chatID, text string) error
	// Disconnect shuts down the IM connection.
	Disconnect()
	// Type returns the adapter type identifier (e.g. "wechat", "feishu").
	Type() string
	// StartLogin initiates a login flow and returns a URL for the user to
	// scan/confirm. Only supported by adapters that require interactive login.
	// Returns ("", ErrLoginNotSupported) for adapters that don't need login.
	StartLogin() (qrURL string, err error)
}
