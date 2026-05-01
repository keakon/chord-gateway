package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type sessionPinStore struct {
	mu     sync.Mutex
	path   string
	pins   map[string]string // processKey.String() -> sessionID
	writer func(path string, data []byte, perm os.FileMode) error
}

func newSessionPinStore(storageDir string) *sessionPinStore {
	return &sessionPinStore{
		path:   filepath.Join(storageDir, "session-pins.json"),
		pins:   make(map[string]string),
		writer: writeFileAtomically,
	}
}

func (s *sessionPinStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.pins = pins
	return nil
}

func (s *sessionPinStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.savePinsLocked(s.pins)
}

func (s *sessionPinStore) savePinsLocked(pins map[string]string) error {
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pins: %w", err)
	}
	if err := s.writer(s.path, data, 0o644); err != nil {
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
	s.mu.Lock()
	defer s.mu.Unlock()

	updated := cloneStringMap(s.pins)
	if strings.TrimSpace(sessionID) == "" {
		delete(updated, key)
	} else {
		updated[key] = sessionID
	}
	if err := s.savePinsLocked(updated); err != nil {
		return err
	}
	s.pins = updated
	return nil
}

func cloneStringMap(src map[string]string) map[string]string {
	clone := make(map[string]string, len(src))
	for k, v := range src {
		clone[k] = v
	}
	return clone
}
