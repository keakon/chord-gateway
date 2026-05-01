package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDedupeStore_TryBeginAndCommit(t *testing.T) {
	dir := t.TempDir()
	ds, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	key := "app1|chat1|msg1"

	// First TryBegin should succeed.
	if !ds.TryBegin(key) {
		t.Fatal("first TryBegin should return true")
	}

	// Second TryBegin for same key should fail (in-flight).
	if ds.TryBegin(key) {
		t.Fatal("second TryBegin should return false (duplicate)")
	}

	// Commit the key.
	ds.Commit(key)

	// After commit, TryBegin should still fail (committed).
	if ds.TryBegin(key) {
		t.Fatal("TryBegin after commit should return false")
	}
}

func TestDedupeStore_TryBeginAndRelease(t *testing.T) {
	dir := t.TempDir()
	ds, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	key := "app1|chat1|msg2"

	// Reserve it.
	if !ds.TryBegin(key) {
		t.Fatal("first TryBegin should return true")
	}

	// Release it (simulating owner rejection or queue full).
	ds.Release(key)

	// Now TryBegin should succeed again.
	if !ds.TryBegin(key) {
		t.Fatal("TryBegin after Release should return true")
	}
}

func TestDedupeStore_PersistenceAndLoad(t *testing.T) {
	dir := t.TempDir()

	// Create store, commit a key, close it.
	ds1, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "app1|chat1|msg3"
	ds1.TryBegin(key)
	ds1.Commit(key)
	ds1.Close()

	// Reopen store — it should load from file.
	ds2, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ds2.Close()

	// The committed key should still be known.
	if !ds2.Contains(key) {
		t.Fatal("committed key should persist across restarts")
	}

	// TryBegin should fail since it's already committed.
	if ds2.TryBegin(key) {
		t.Fatal("TryBegin for persisted key should return false")
	}
}

func TestDedupeStore_TTLExpiry(t *testing.T) {
	dir := t.TempDir()
	ds, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	// Override the TTL to a very short duration for testing.
	ds.mu.Lock()
	ds.ttl = 100 * time.Millisecond
	ds.mu.Unlock()

	key := "app1|chat1|msg4"
	ds.TryBegin(key)
	ds.Commit(key)

	// Immediately it should be known.
	if !ds.Contains(key) {
		t.Fatal("key should be known immediately after commit")
	}

	// Wait for TTL to expire.
	time.Sleep(200 * time.Millisecond)

	// After expiry, it should be gone.
	if ds.Contains(key) {
		t.Fatal("key should expire after TTL")
	}

	// TryBegin should succeed after expiry.
	if !ds.TryBegin(key) {
		t.Fatal("TryBegin after TTL expiry should return true")
	}
}

func TestDedupeStore_PersistedTTLExpiry(t *testing.T) {
	dir := t.TempDir()
	key := "app1|chat1|msg5"

	// Create store with short TTL, commit a key, close it.
	ds1, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ds1.mu.Lock()
	ds1.ttl = 50 * time.Millisecond
	ds1.mu.Unlock()
	ds1.TryBegin(key)
	ds1.Commit(key)
	ds1.Close()

	// Wait for the entry to expire.
	time.Sleep(100 * time.Millisecond)

	// Reopen store — expired entries should not be loaded.
	ds2, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ds2.Close()

	if ds2.Contains(key) {
		t.Fatal("expired persisted key should not be loaded")
	}

	if !ds2.TryBegin(key) {
		t.Fatal("TryBegin should succeed for expired persisted key")
	}
}

func TestDedupeStore_ContainsExpiryMarksDirtyForPersistence(t *testing.T) {
	dir := t.TempDir()
	key := "app1|chat1|msg6"

	ds, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	ds.mu.Lock()
	ds.ttl = 50 * time.Millisecond
	ds.mu.Unlock()

	if !ds.TryBegin(key) {
		t.Fatal("TryBegin should succeed")
	}
	ds.Commit(key)

	data, err := os.ReadFile(ds.storagePath)
	if err != nil {
		t.Fatalf("read persisted dedupe file before expiry: %v", err)
	}
	if !strings.Contains(string(data), key) {
		t.Fatalf("persisted dedupe file should contain key %q before expiry: %s", key, data)
	}

	time.Sleep(100 * time.Millisecond)

	if ds.Contains(key) {
		t.Fatal("Contains should drop expired key")
	}
	if !ds.dirty {
		t.Fatal("Contains should mark store dirty after dropping expired key")
	}

	ds.mu.Lock()
	if err := ds.saveToFileLocked(); err != nil {
		ds.mu.Unlock()
		t.Fatalf("save cleaned dedupe file: %v", err)
	}
	ds.dirty = false
	ds.mu.Unlock()

	data, err = os.ReadFile(ds.storagePath)
	if err != nil {
		t.Fatalf("read persisted dedupe file after cleanup: %v", err)
	}
	if strings.Contains(string(data), key) {
		t.Fatalf("persisted dedupe file should not contain expired key after cleanup: %s", data)
	}
}

func TestDedupeStore_EmptyStorageDirErrors(t *testing.T) {
	// Empty storageDir is not valid; NewDedupeStore should return an error.
	ds, err := NewDedupeStore("")
	if err == nil {
		t.Fatal("expected error for empty storageDir")
	}
	if ds != nil {
		t.Fatal("expected nil DedupeStore on error")
	}
}

func TestDedupeStore_DefaultStorageDir(t *testing.T) {
	// With a valid storageDir, NewDedupeStore should succeed.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	stateDir := filepath.Join(home, ".local", "state", "chord-gateway")
	ds, err := NewDedupeStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	expectedPath := filepath.Join(stateDir, "dedupe.json")
	if ds.storagePath != expectedPath {
		t.Fatalf("storagePath = %q, want %q", ds.storagePath, expectedPath)
	}
}

func TestDedupeStore_Contains(t *testing.T) {
	dir := t.TempDir()
	ds, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	// Unknown key should not be contained.
	if ds.Contains("nonexistent") {
		t.Fatal("nonexistent key should not be contained")
	}

	// In-flight key should be contained.
	ds.TryBegin("inflight")
	if !ds.Contains("inflight") {
		t.Fatal("in-flight key should be contained")
	}

	// Committed key should be contained.
	ds.Commit("inflight")
	if !ds.Contains("inflight") {
		t.Fatal("committed key should be contained")
	}
}

func TestDedupeStore_CloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	ds, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ds.Close()
	// Should not panic on repeated close.
	ds.Close()
}
