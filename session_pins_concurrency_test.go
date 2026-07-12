package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

func TestSessionPinStoreGetDoesNotBlockOnWrite(t *testing.T) {
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	s := &sessionPinStore{
		path: filepath.Join(t.TempDir(), "pins.json"),
		pins: map[string]string{"existing": "session"},
		writer: func(string, []byte, os.FileMode) error {
			close(writeStarted)
			<-releaseWrite
			return nil
		},
	}
	setDone := make(chan error, 1)
	go func() { setDone <- s.Set("new", "value") }()
	<-writeStarted

	getDone := make(chan string, 1)
	go func() { getDone <- s.Get("existing") }()
	select {
	case got := <-getDone:
		if got != "session" {
			t.Fatalf("Get() = %q, want session", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Get blocked while session pins were being written")
	}
	close(releaseWrite)
	if err := <-setDone; err != nil {
		t.Fatal(err)
	}
}

func TestSessionPinStoreSaveDoesNotBlockGet(t *testing.T) {
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	s := &sessionPinStore{
		path: filepath.Join(t.TempDir(), "pins.json"),
		pins: map[string]string{"existing": "session"},
		writer: func(string, []byte, os.FileMode) error {
			close(writeStarted)
			<-releaseWrite
			return nil
		},
	}
	saveDone := make(chan error, 1)
	go func() { saveDone <- s.Save() }()
	<-writeStarted

	getDone := make(chan string, 1)
	go func() { getDone <- s.Get("existing") }()
	select {
	case got := <-getDone:
		if got != "session" {
			t.Fatalf("Get() = %q, want session", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Get blocked while session pins were saved")
	}
	close(releaseWrite)
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
}

func TestSessionPinStoreSetSkipsUnchangedValue(t *testing.T) {
	writes := 0
	s := &sessionPinStore{
		path: filepath.Join(t.TempDir(), "pins.json"),
		pins: map[string]string{"key": "session"},
		writer: func(string, []byte, os.FileMode) error {
			writes++
			return nil
		},
	}
	if err := s.Set("key", "session"); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("unchanged pin writes = %d, want 0", writes)
	}
	if got := s.Get("key"); got != "session" {
		t.Fatalf("pin = %q, want session", got)
	}
}

func TestSessionPinStoreSetSkipsMissingRemoval(t *testing.T) {
	writes := 0
	s := &sessionPinStore{
		path: filepath.Join(t.TempDir(), "pins.json"),
		pins: make(map[string]string),
		writer: func(string, []byte, os.FileMode) error {
			writes++
			return nil
		},
	}
	if err := s.Set("missing", "  "); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("missing removal writes = %d, want 0", writes)
	}
}

func BenchmarkSessionPinStoreSetUnchanged(b *testing.B) {
	s := newSessionPinStore(b.TempDir())
	s.pins["key"] = "session"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := s.Set("key", "session"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSessionPinStoreSet(b *testing.B) {
	for _, entries := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("entries-%d", entries), func(b *testing.B) {
			s := newSessionPinStore(b.TempDir())
			for i := 0; i < entries; i++ {
				s.pins[fmt.Sprintf("key-%d", i)] = fmt.Sprintf("session-%d", i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.Set("updated", fmt.Sprintf("session-%d", i)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
