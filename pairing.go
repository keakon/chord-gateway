package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	pairingLifetime = 10 * time.Minute

	// Hint replies answer unpaired private senders. The bucket is separate
	// from the pairing budget so hint chatter cannot drain pairing attempts.
	// The reverse is not symmetric: an attempt is charged before its args are
	// parsed, so while the pairing budget is exhausted a private sender gets
	// no hint until the budget replenishes.
	hintBucketSize     = 10
	hintRefillInterval = 10 * time.Second

	// Bound the per-chat hint buckets so one platform cannot accumulate
	// unbounded state from many unpaired private chats.
	hintChatBucketMax = 256
)

var errPairingDenied = errors.New("pairing authorization is no longer valid")

type pairingPlatform struct {
	ctx    context.Context
	cancel context.CancelFunc

	presentation   PlatformPresentation
	account        string
	epoch          uint64
	ready, changed bool
	tokens         int
	refilled       time.Time

	// Hint budgets are keyed by private chat, not shared across the platform:
	// a spammer in one chat must not silence hints for unpaired users elsewhere.
	hints map[string]*pairingHint
}

// pairingHint is the bounded hint-reply budget for one unpaired private chat.
type pairingHint struct {
	tokens   int
	refilled time.Time
}

// PairingService keeps credentials in memory and writes them only to the
// controlling terminal. The configuration remains the owner authority.
//
// A single pairing code is generated once (BeginPairing, or lazily by the first
// unpaired PlatformReady) and is valid across every active platform for the
// pairing window. It is never consumed early, so it can bind several platforms.
type PairingService struct {
	mu        sync.Mutex
	store     *ConfigStore
	enabled   bool
	terminal  io.Writer
	random    io.Reader
	now       func() time.Time
	platforms map[string]*pairingPlatform

	// Universal pairing code, generated once and valid across all platforms
	// until codeExpires. It is never consumed early.
	codeNonce   [16]byte
	codeID      string
	codeExpires time.Time
}

func NewPairingService(store *ConfigStore, enabled bool, terminal io.Writer) *PairingService {
	return &PairingService{store: store, enabled: enabled, terminal: terminal, random: rand.Reader, now: time.Now, platforms: map[string]*pairingPlatform{}}
}

func (p *PairingService) PlatformReady(platform, accountID string) error {
	return p.PlatformReadyWithPresentation(platform, accountID, PlatformPresentation{Platform: platform})
}

func (p *PairingService) PlatformReadyWithPresentation(platform, accountID string, presentation PlatformPresentation) error {
	p.mu.Lock()
	if accountID == "" {
		p.mu.Unlock()
		return errPairingDenied
	}
	state := p.platforms[platform]
	if state == nil {
		accountCtx, cancel := context.WithCancel(context.Background())
		// Seed the bind-attempt budget once, when the platform first becomes
		// known. Reconnecting must not refill it: the budget exists to cap
		// guessing against the pairing code.
		state = &pairingPlatform{account: accountID, epoch: 1, ctx: accountCtx, cancel: cancel, tokens: 30, refilled: p.now()}
		p.platforms[platform] = state
	} else if state.account != accountID {
		if state.cancel != nil {
			state.cancel()
		}
		state.epoch++
		state.ready = false
		state.changed = true
		state.account = accountID
		p.mu.Unlock()
		return errPairingDenied
	}
	presentation.Platform = platform
	state.presentation = presentation
	if state.changed {
		p.mu.Unlock()
		return errPairingDenied
	}
	cfg := p.store.Snapshot()
	im := cfg.IMConfigByType(platform)
	if im == nil {
		p.mu.Unlock()
		return errPairingDenied
	}
	if owner := im.OwnerIdentity(); owner != nil {
		if owner.AccountID != accountID {
			p.mu.Unlock()
			return errPairingDenied
		}
		state.ready = true
		p.mu.Unlock()
		return nil
	}
	state.ready = true
	message, err := p.preparePairingCodeLocked()
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if message != "" {
		if err := writePairingMessage(p.terminal, message); err != nil {
			p.mu.Lock()
			p.codeNonce = [16]byte{}
			p.codeID = ""
			p.codeExpires = time.Time{}
			p.mu.Unlock()
			return err
		}
	}
	return nil
}

// BeginPairing generates and displays the universal pairing code immediately at
// gateway startup, before any platform connects. It is a no-op when pairing is
// disabled or the code is already live.
func (p *PairingService) BeginPairing() error {
	p.mu.Lock()
	message, err := p.preparePairingCodeLocked()
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if message == "" {
		return nil
	}
	if err := writePairingMessage(p.terminal, message); err != nil {
		p.mu.Lock()
		p.codeNonce = [16]byte{}
		p.codeID = ""
		p.codeExpires = time.Time{}
		p.mu.Unlock()
		return err
	}
	return nil
}

// preparePairingCodeLocked creates the universal pairing code if one is not
// already live and returns the terminal message. Callers hold p.mu; the
// terminal write happens outside the lock so a slow terminal cannot block
// admission. An empty message means no display is needed.
func (p *PairingService) preparePairingCodeLocked() (string, error) {
	if !p.enabled || !p.codeExpires.IsZero() {
		return "", nil
	}
	if p.terminal == nil {
		return "", errors.New("pairing requires a controlling terminal")
	}
	if _, err := io.ReadFull(p.random, p.codeNonce[:]); err != nil {
		return "", errors.New("pairing random source failed")
	}
	var id [16]byte
	if _, err := io.ReadFull(p.random, id[:]); err != nil {
		return "", errors.New("pairing random source failed")
	}
	p.codeID = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(id[:])
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(p.codeNonce[:])
	names := make([]string, 0)
	for _, im := range p.store.Snapshot().ActiveIMs() {
		names = append(names, im.Type())
	}
	sort.Strings(names)
	// One code serves every platform, and most platforms render the same
	// private-chat command. Group them so each distinct command is printed once
	// with its platforms instead of repeating an identical line per platform.
	bind := fmt.Sprintf("/bind \"<directory>\" %s", code)
	order := make([]string, 0, len(names))
	grouped := make(map[string][]string, len(names))
	for _, plat := range names {
		hint := FormatCommandHint(PlatformPresentation{Platform: plat}, CommandTarget{ChatKind: "private"}, bind)
		if _, seen := grouped[hint]; !seen {
			order = append(order, hint)
		}
		grouped[hint] = append(grouped[hint], plat)
	}
	width := 0
	for _, hint := range order {
		if n := len(strings.Join(grouped[hint], ", ")); n > width {
			width = n
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Pairing code (Valid for 10 minutes, works on all platforms): %s\n", code)
	b.WriteString("In a private chat with the bot, send on the matching platform:\n")
	for _, hint := range order {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, strings.Join(grouped[hint], ", "), hint)
	}
	b.WriteString("Later bindings within the window do not need a new code.\n")
	p.codeExpires = p.now().Add(pairingLifetime)
	return b.String(), nil
}

func writePairingMessage(terminal io.Writer, message string) error {
	if terminal == nil {
		return errors.New("pairing requires a controlling terminal")
	}
	n, err := io.WriteString(terminal, message)
	if err != nil || n != len(message) {
		return errors.New("could not display pairing instructions on the controlling terminal")
	}
	return nil
}
func (p *PairingService) PlatformUnavailable(platform string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.platforms[platform]; s != nil {
		s.ready = false
	}
}

func (p *PairingService) codeValid(code string) bool {
	if !p.enabled || p.codeExpires.IsZero() || !p.now().Before(p.codeExpires) {
		return false
	}
	code = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - 32
		}
		return r
	}, code)
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(code)
	return err == nil && len(decoded) == 16 && subtle.ConstantTimeCompare(decoded, p.codeNonce[:]) == 1
}

// hintAllowed spends one bounded hint-reply token from the private chat's own
// bucket. Callers hold p.mu.
func (p *PairingService) hintAllowed(s *pairingPlatform, chatID string) bool {
	if s == nil || chatID == "" {
		return false
	}
	if s.hints == nil {
		s.hints = make(map[string]*pairingHint)
	}
	h := s.hints[chatID]
	if h == nil {
		if len(s.hints) >= hintChatBucketMax {
			// Drop buckets that have fully refilled: idle chats cost nothing and
			// a burst of distinct chats cannot evict active ones.
			for id, idle := range s.hints {
				if p.now().Sub(idle.refilled) >= time.Duration(hintBucketSize)*hintRefillInterval {
					delete(s.hints, id)
				}
			}
			if len(s.hints) >= hintChatBucketMax {
				return false
			}
		}
		h = &pairingHint{tokens: hintBucketSize, refilled: p.now()}
		s.hints[chatID] = h
	}
	if replenished := int(p.now().Sub(h.refilled) / hintRefillInterval); replenished > 0 {
		h.tokens = min(hintBucketSize, h.tokens+replenished)
		h.refilled = h.refilled.Add(time.Duration(replenished) * hintRefillInterval)
	}
	if h.tokens == 0 {
		return false
	}
	h.tokens--
	return true
}

// codeLive reports whether the universal pairing code is currently usable.
// It is platform-independent: one code serves every platform.
func (p *PairingService) codeLive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enabled && !p.codeExpires.IsZero() && p.now().Before(p.codeExpires)
}
