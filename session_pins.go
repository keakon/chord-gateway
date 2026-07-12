package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type sessionPinStore struct {
	mu        sync.Mutex
	persistMu sync.Mutex
	path      string
	pins      map[string]string // processKey.String() -> sessionID
	writer    func(path string, data []byte, perm os.FileMode) error
}

func newSessionPinStore(storageDir string) *sessionPinStore {
	return &sessionPinStore{
		path: filepath.Join(storageDir, "session-pins.json"),
		pins: make(map[string]string),
		writer: func(path string, data []byte, _ os.FileMode) error {
			return writePrivateFileAtomically(path, data)
		},
	}
}

func (s *sessionPinStore) Load() error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read pins: %w", err)
	}
	var pins map[string]string
	if err := json.Unmarshal(data, &pins); err != nil {
		return fmt.Errorf("parse pins: %w", err)
	}
	if pins == nil {
		pins = make(map[string]string)
	}
	s.mu.Lock()
	s.pins = pins
	s.mu.Unlock()
	return nil
}

func (s *sessionPinStore) Save() error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.Lock()
	pins := maps.Clone(s.pins)
	s.mu.Unlock()
	return s.savePins(pins)
}

func (s *sessionPinStore) savePins(pins map[string]string) error {
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pins: %w", err)
	}
	if err := s.writer(s.path, data, 0o600); err != nil {
		return fmt.Errorf("write pins: %w", err)
	}
	return nil
}

// Get returns the pinned sessionID for a process key (workspaceID|imType|chatID).
// Returns the empty string if no pin exists.
func (s *sessionPinStore) Get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pins[key]
}

// Set persists a pinned sessionID for the given process key. Passing an empty
// sessionID removes the pin.
func (s *sessionPinStore) Set(key, sessionID string) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	s.mu.Lock()
	current, exists := s.pins[key]
	remove := strings.TrimSpace(sessionID) == ""
	if remove && !exists || !remove && exists && current == sessionID {
		s.mu.Unlock()
		return nil
	}
	updated := maps.Clone(s.pins)
	s.mu.Unlock()
	if updated == nil {
		updated = make(map[string]string)
	}
	if remove {
		delete(updated, key)
	} else {
		updated[key] = sessionID
	}
	if err := s.savePins(updated); err != nil {
		return err
	}
	s.mu.Lock()
	s.pins = updated
	s.mu.Unlock()
	return nil
}
