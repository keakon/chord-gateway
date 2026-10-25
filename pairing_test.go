package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
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
		if v == "\"<directory>\"" {
			code = fields[i+1]
		}
	}
	if len(code) != 26 {
		t.Fatal("invalid terminal instruction")
	}
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "user", MessageID: "message", Text: "/bind /tmp/project " + code}
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
	if !p.codeExpires.IsZero() {
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

// TestUniversalPairingCodeIsImmediateAndCrossPlatform verifies the pairing code
// is generated once at BeginPairing (before any platform connects), is valid on
// every platform for the window, and is not consumed after a single bind.
func TestUniversalPairingCodeIsImmediateAndCrossPlatform(t *testing.T) {
	s := testConfigStore(t, "ims: {wechat: {}, telegram: {bot_token: x}, discord: {bot_token: x, application_id: '123456789012345678'}}\n")
	var terminal bytes.Buffer
	p := NewPairingService(s, true, &terminal)
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }

	// BeginPairing generates the code immediately, listing every active platform.
	if err := p.BeginPairing(); err != nil {
		t.Fatal(err)
	}
	if p.codeExpires.IsZero() {
		t.Fatal("code not generated at BeginPairing")
	}
	fields := strings.Fields(terminal.String())
	var code string
	for i, v := range fields {
		if v == "\"<directory>\"" {
			code = fields[i+1]
		}
	}
	if code == "" || len(code) != 26 {
		t.Fatal("code not shown on terminal immediately")
	}
	if !strings.Contains(terminal.String(), "works on all platforms") {
		t.Fatal("code prompt must advertise cross-platform validity")
	}
	// Every active platform must be named exactly once, and platforms that
	// render the same private-chat command must share a single line.
	prompt := terminal.String()
	if n := strings.Count(prompt, "/bind \"<directory>\""); n != 1 {
		t.Fatalf("expected one grouped bind line, got %d: %q", n, prompt)
	}
	for _, plat := range []string{"discord", "telegram", "wechat"} {
		if n := strings.Count(prompt, plat); n != 1 {
			t.Fatalf("platform %s listed %d times: %q", plat, n, prompt)
		}
	}

	// A later platform becoming ready must not regenerate or invalidate it.
	if err := p.PlatformReady("telegram", "telegram-bot"); err != nil {
		t.Fatal(err)
	}
	if now.Add(pairingLifetime).Before(p.codeExpires) {
		t.Fatal("ready event changed the code expiry")
	}

	// The same code authenticates a bind on either platform during the window.
	for _, platform := range []string{"wechat", "telegram"} {
		if !p.codeValid(code) {
			t.Fatalf("universal code rejected on platform %s", platform)
		}
	}

	// The code survives a single successful bind and stays valid for the window.
	if err := p.PlatformReady("wechat", "wechat-bot"); err != nil {
		t.Fatal(err)
	}
	pairing := NewAdmission(p)
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "wechat-bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "bind", Text: fmt.Sprintf("/bind /tmp/project %s", code)}
	ticket, ok := pairing.Authorize(msg)
	if !ok || ticket.Kind != "paired_bind" {
		t.Fatal("valid universal code rejected")
	}
	// After the window expires the code is no longer valid anywhere.
	now = now.Add(pairingLifetime)
	if p.codeValid(code) {
		t.Fatal("expired universal code still accepted")
	}
}

// A platform reconnect must not refill the bind-attempt budget: that budget caps
// guessing against the pairing code, so only the first readiness may seed it.
func TestPairingReadyDoesNotRefillBindAttempts(t *testing.T) {
	s := testConfigStore(t, "ims: {wechat: {}}\n")
	var terminal bytes.Buffer
	p := NewPairingService(s, true, &terminal)
	p.now = func() time.Time { return time.Unix(1000, 0) }
	for range 2 {
		if err := p.PlatformReadyWithPresentation("wechat", "bot", im.PlatformPresentation{}); err != nil {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	tokens := p.platforms["wechat"].tokens
	p.mu.Unlock()
	if tokens != 30 {
		t.Fatalf("bind attempts after reconnect = %d, want the single seeded budget", tokens)
	}
	p.mu.Lock()
	p.platforms["wechat"].tokens = 4
	p.mu.Unlock()
	if err := p.PlatformReadyWithPresentation("wechat", "bot", im.PlatformPresentation{}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	remaining := p.platforms["wechat"].tokens
	p.mu.Unlock()
	if remaining != 4 {
		t.Fatalf("reconnect refilled the bind budget: %d", remaining)
	}
}
