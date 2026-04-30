package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSessionPinStoreSetDoesNotMutateMemoryOnWriteFailure(t *testing.T) {
	tmp := t.TempDir()
	s := &sessionPinStore{
		path: filepath.Join(tmp, "pins.json"),
		pins: map[string]string{"keep": "old"},
		writer: func(path string, data []byte, perm os.FileMode) error {
			return errors.New("disk full")
		},
	}

	err := s.Set("new", "value")
	if err == nil {
		t.Fatal("Set() error = nil, want write failure")
	}
	if got := s.Get("keep"); got != "old" {
		t.Fatalf("keep pin = %q, want old", got)
	}
	if got := s.Get("new"); got != "" {
		t.Fatalf("new pin = %q, want empty", got)
	}
}

func TestSessionPinStoreConcurrentSetKeepsAllUpdates(t *testing.T) {
	tmp := t.TempDir()
	s := newSessionPinStore(tmp)

	const n = 24
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("ws-%02d", i)
			value := fmt.Sprintf("session-%02d", i)
			if err := s.Set(key, value); err != nil {
				t.Errorf("Set(%q) error = %v", key, err)
			}
		}()
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		key := fmt.Sprintf("ws-%02d", i)
		want := fmt.Sprintf("session-%02d", i)
		if got := s.Get(key); got != want {
			t.Fatalf("pin %q = %q, want %q", key, got, want)
		}
	}

	loaded := newSessionPinStore(tmp)
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("ws-%02d", i)
		want := fmt.Sprintf("session-%02d", i)
		if got := loaded.Get(key); got != want {
			t.Fatalf("loaded pin %q = %q, want %q", key, got, want)
		}
	}
}
