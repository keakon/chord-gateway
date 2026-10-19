package main

import (
	"errors"
	"maps"
)

// retiredPinKeyPrefix marks the removed identity-scoped pin layout. Keys under
// it are dropped on load instead of being rewritten to a different identity.
const retiredPinKeyPrefix = "paired:"

// A pin records which Chord session a binding resumes next. It identifies the
// workspace and route, not the platform account: changing the platform account
// on an existing route keeps the conversation. Local `pairing reset` is the
// exception: it clears the platform's automatic pins along with the owner and
// bindings, so a reset route starts without the old session.
// Re-pairing to a different owner inherits the previous session by design for
// single-user deployments: use /new or delete the pin for isolation.
//
// Scoping the key to the paired identity was tried and reverted: it silently
// dropped sessions whenever an operator changed the account, while access to a
// session is already gated by the binding's owner. The mapping is therefore the
// identity, but it stays the single place that decides the pin namespace so a
// future change cannot drift across the twenty-odd call sites.
func pinKeyForBinding(key string) string {
	return key
}

func (p *ChordProcess) sessionPinKey() string {
	return p.key
}

func pinPlatform(key string) (string, error) {
	workspace, platform, chat := parseProcessKey(key)
	if workspace == "" || chat == "" || (!supportedIMPlatform(platform) && platform != "console") {
		return "", errors.New("unrecognized session pin key")
	}
	return platform, nil
}

// ClearPlatform persists the entire removal before publishing it in memory.
// Call Load and check its error before using this method for local reset.
// Entries that name no platform are already unreachable after load and are
// skipped rather than failing the reset.
func (s *sessionPinStore) ClearPlatform(platform string) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.Lock()
	next := maps.Clone(s.pins)
	s.mu.Unlock()
	for key := range next {
		p, err := pinPlatform(key)
		if err != nil {
			continue
		}
		if p == platform {
			delete(next, key)
		}
	}
	if err := s.savePins(next); err != nil {
		return err
	}
	s.mu.Lock()
	s.pins = next
	s.mu.Unlock()
	return nil
}
