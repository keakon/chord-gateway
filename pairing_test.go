package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

type failedPairingWriter struct{}

func (failedPairingWriter) Write([]byte) (int, error) { return 0, errors.New("failure") }

func TestPairingExpiryAndAdmission(t *testing.T) {
	s := testConfigStore(t, "ims: {wechat: {}}\n")
	var terminal bytes.Buffer
	p := NewPairingService(s, true, &terminal)
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	a := NewAdmission(p)
	if err := p.PlatformReady("wechat", "bot"); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(terminal.String())
	var code string
	for i, v := range fields {
		if v == "\"~/workspace\"" {
			code = fields[i+1]
		}
	}
	if len(code) != 26 {
		t.Fatal("invalid terminal instruction")
	}
	msg := IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "user", MessageID: "message", Text: "/bind /tmp/project " + code}
	ticket, ok := a.Authorize(msg)
	if !ok || ticket.Kind != "paired_bind" {
		t.Fatal("valid code rejected")
	}
	group := msg
	group.ChatKind = "group"
	if _, ok = a.Authorize(group); ok {
		t.Fatal("group pairing allowed")
	}
	other := msg
	other.AccountID = "other"
	if _, ok = a.Authorize(other); ok {
		t.Fatal("wrong account allowed")
	}
	now = now.Add(pairingLifetime)
	if a.Valid(ticket) {
		t.Fatal("expired ticket valid")
	}
	if _, ok = a.Authorize(msg); ok {
		t.Fatal("expired code accepted")
	}
	if err := p.PlatformReady("wechat", "bot"); err != nil {
		t.Fatal(err)
	}
	if _, ok = a.Authorize(msg); ok {
		t.Fatal("reconnect renewed code")
	}
}
func TestPairingOutputFailureClosesWindow(t *testing.T) {
	s := testConfigStore(t, "ims: {wechat: {}}\n")
	p := NewPairingService(s, true, failedPairingWriter{})
	if p.PlatformReady("wechat", "bot") == nil {
		t.Fatal("output error ignored")
	}
	if !p.platforms["wechat"].consumed {
		t.Fatal("hidden credential active")
	}
}
func TestPairedBindParser(t *testing.T) {
	for _, input := range []string{"/bind", "/bind \"/tmp/space\"suffix", "/bind /tmp/path code extra", "/bind /tmp/path\ncode", "/bind \"unterminated"} {
		if _, err := parsePairedBindArgs(input); err == nil {
			t.Fatalf("accepted malformed input %q", input)
		}
	}
	args, err := parsePairedBindArgs(`/BiNd "/tmp/space and\\slash\q" code`)
	if err != nil || len(args) != 2 || args[0] != `/tmp/space and\slash\q` {
		t.Fatal("quoted path parsing failed")
	}
	for _, path := range []string{"relative", "~", "~other/path", "$HOME/project"} {
		if _, _, err := canonicalBindingDirectory(path); err == nil {
			t.Fatal("accepted unsupported path")
		}
	}
}
