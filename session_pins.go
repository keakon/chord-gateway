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
	mu   sync.Mutex
	path string
	pins map[string]string // processKey.String() -> sessionID
}

func newSessionPinStore(storageDir string) *sessionPinStore {
	return &sessionPinStore{
		path: filepath.Join(storageDir, "session-pins.json"),
		pins: make(map[string]string),
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
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir pins dir: %w", err)
	}
	data, err := json.MarshalIndent(s.pins, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pins: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0o644); err != nil {
		return fmt.Errorf("write pins: %w", err)
	}
	return nil
}

func (s *sessionPinStore) Get(workspaceID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pins[workspaceID]
}

func (s *sessionPinStore) Set(workspaceID, sessionID string) error {
	s.mu.Lock()
	if strings.TrimSpace(sessionID) == "" {
		delete(s.pins, workspaceID)
	} else {
		s.pins[workspaceID] = sessionID
	}
	s.mu.Unlock()
	return s.Save()
}
