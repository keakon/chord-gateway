package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const pairingLifetime = 10 * time.Minute

var errPairingDenied = errors.New("pairing authorization is no longer valid")

type pairingPlatform struct {
	ctx    context.Context
	cancel context.CancelFunc

	account                  string
	epoch                    uint64
	ready, changed, consumed bool
	nonce                    [16]byte
	nonceID                  string
	expires                  time.Time
	tokens                   int
	refilled                 time.Time
}

// PairingService keeps credentials in memory and writes them only to the
// controlling terminal. The configuration remains the owner authority.
type PairingService struct {
	mu        sync.Mutex
	store     *ConfigStore
	enabled   bool
	terminal  io.Writer
	random    io.Reader
	now       func() time.Time
	platforms map[string]*pairingPlatform
}

func NewPairingService(store *ConfigStore, enabled bool, terminal io.Writer) *PairingService {
	return &PairingService{store: store, enabled: enabled, terminal: terminal, random: rand.Reader, now: time.Now, platforms: map[string]*pairingPlatform{}}
}

func (p *PairingService) PlatformReady(platform, accountID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if accountID == "" {
		return errPairingDenied
	}
	state := p.platforms[platform]
	if state == nil {
		accountCtx, cancel := context.WithCancel(context.Background())
		state = &pairingPlatform{account: accountID, epoch: 1, ctx: accountCtx, cancel: cancel}
		p.platforms[platform] = state
	} else if state.account != accountID {
		if state.cancel != nil {
			state.cancel()
		}
		state.epoch++
		state.ready = false
		state.changed = true
		state.account = accountID
		state.nonce = [16]byte{}
		return errPairingDenied
	}
	if state.changed {
		return errPairingDenied
	}
	cfg := p.store.Snapshot()
	im := cfg.IMConfigByType(platform)
	if im == nil {
		return errPairingDenied
	}
	if owner := im.OwnerIdentity(); owner != nil {
		if owner.AccountID != accountID {
			return errPairingDenied
		}
		state.ready = true
		return nil
	}
	state.ready = true
	if !p.enabled || !state.expires.IsZero() || state.consumed {
		return nil
	}
	// Mark the attempt before randomness/output: failure never opens a hidden code.
	state.consumed = true
	if p.terminal == nil {
		return errors.New("pairing requires a controlling terminal")
	}
	if _, err := io.ReadFull(p.random, state.nonce[:]); err != nil {
		return errors.New("pairing random source failed")
	}
	var id [16]byte
	if _, err := io.ReadFull(p.random, id[:]); err != nil {
		return errors.New("pairing random source failed")
	}
	state.nonceID = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(id[:])
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(state.nonce[:])
	prompt := fmt.Sprintf("%s: In a private chat, send /bind \"~/workspace\" %s\nValid for 10 minutes. Later bindings do not need a code.\n", platform, code)
	n, err := io.WriteString(p.terminal, prompt)
	if err != nil || n != len(prompt) {
		state.nonce = [16]byte{}
		return errors.New("could not display pairing instructions on the controlling terminal")
	}
	state.expires = p.now().Add(pairingLifetime)
	state.tokens = 30
	state.refilled = p.now()
	state.consumed = false
	return nil
}
func (p *PairingService) PlatformUnavailable(platform string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.platforms[platform]; s != nil {
		s.ready = false
	}
}

func (p *PairingService) codeValid(s *pairingPlatform, code string) bool {
	if s == nil || !s.ready || s.changed || s.consumed || !p.now().Before(s.expires) {
		return false
	}
	code = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - 32
		}
		return r
	}, code)
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(code)
	return err == nil && len(decoded) == 16 && subtle.ConstantTimeCompare(decoded, s.nonce[:]) == 1
}

func (p *PairingService) consume(platform string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.platforms[platform]; s != nil {
		s.consumed = true
		s.nonce = [16]byte{}
	}
}
