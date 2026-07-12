package main

import (
	"fmt"
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

	data, err := os.ReadFile(ds.journalPath)
	if err != nil {
		t.Fatalf("read persisted dedupe journal before expiry: %v", err)
	}
	if !strings.Contains(string(data), key) {
		t.Fatalf("persisted dedupe journal should contain key %q before expiry: %s", key, data)
	}

	time.Sleep(100 * time.Millisecond)

	if ds.Contains(key) {
		t.Fatal("Contains should drop expired key")
	}
	if !ds.dirty {
		t.Fatal("Contains should mark store dirty after dropping expired key")
	}

	if err := ds.compact(); err != nil {
		t.Fatalf("save cleaned dedupe file: %v", err)
	}

	data, err = os.ReadFile(ds.storagePath)
	if err != nil {
		t.Fatalf("read persisted dedupe file after cleanup: %v", err)
	}
	if strings.Contains(string(data), key) {
		t.Fatalf("persisted dedupe file should not contain expired key after cleanup: %s", data)
	}
}

func TestDedupeStore_CleanupKeepsDirtyWhenSaveFails(t *testing.T) {
	dir := t.TempDir()
	ds, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	ds.mu.Lock()
	ds.entries["expired"] = dedupeEntry{
		Key:       "expired",
		Committed: true,
		ExpiresAt: time.Now().Add(-time.Second),
	}
	ds.dirty = true
	ds.writeSnapshot = func(string, []byte) error { return fmt.Errorf("write failed") }
	ds.mu.Unlock()
	if err := ds.compact(); err == nil {
		t.Fatal("compact should fail")
	}
	ds.mu.Lock()
	if !ds.dirty {
		ds.mu.Unlock()
		t.Fatal("dirty should remain true after failed cleanup save")
	}
	ds.mu.Unlock()
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

func TestDedupeStore_LoadsJournalBeforeCleanClose(t *testing.T) {
	dir := t.TempDir()
	ds1, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "app1|chat1|journal"
	ds1.Commit(key)

	ds2, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ds2.Contains(key) {
		t.Fatal("journaled key should survive reopening before clean close")
	}
	ds2.Close()
	ds1.Close()
}

func TestDedupeStore_CloseCompactsJournal(t *testing.T) {
	dir := t.TempDir()
	ds, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "app1|chat1|compact"
	ds.Commit(key)
	ds.Close()

	if _, err := os.Stat(filepath.Join(dir, dedupeFileName+dedupeJournalSuffix)); !os.IsNotExist(err) {
		t.Fatalf("journal should be removed after compaction, stat error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, dedupeFileName))
	if err != nil {
		t.Fatalf("read compacted snapshot: %v", err)
	}
	if !strings.Contains(string(data), key) {
		t.Fatalf("compacted snapshot should contain key %q: %s", key, data)
	}
}

func TestDedupeStore_CommitDoesNotBlockOnSnapshotWrite(t *testing.T) {
	dir := t.TempDir()
	ds, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	ds.Commit("before-compaction")
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	ds.writeSnapshot = func(path string, data []byte) error {
		close(writeStarted)
		<-releaseWrite
		return writePrivateFileAtomically(path, data)
	}
	compactDone := make(chan error, 1)
	go func() { compactDone <- ds.compact() }()
	<-writeStarted

	commitDone := make(chan struct{})
	go func() {
		ds.Commit("during-compaction")
		close(commitDone)
	}()
	select {
	case <-commitDone:
	case <-time.After(time.Second):
		t.Fatal("Commit blocked while snapshot was being written")
	}
	close(releaseWrite)
	if err := <-compactDone; err != nil {
		t.Fatalf("compact: %v", err)
	}
	ds.writeSnapshot = writePrivateFileAtomically

	reopened, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.Contains("before-compaction") || !reopened.Contains("during-compaction") {
		t.Fatal("snapshot and active journal entries should both survive compaction")
	}
}

func TestDedupeStore_LoadsRotatedJournalAfterInterruptedCompaction(t *testing.T) {
	dir := t.TempDir()
	ds, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ds.Commit("rotated-entry")
	ds.mu.Lock()
	rotatedPath, err := ds.rotateJournalLocked()
	ds.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rotatedPath); err != nil {
		t.Fatalf("rotated journal missing: %v", err)
	}

	reopened, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Contains("rotated-entry") {
		t.Fatal("entry from interrupted compaction should be recovered")
	}
	reopened.Close()
	ds.Close()
}

func TestDedupeStore_RetriesFailedCompaction(t *testing.T) {
	dir := t.TempDir()
	ds, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ds.Commit("retry-entry")
	ds.writeSnapshot = func(string, []byte) error { return fmt.Errorf("write failed") }
	if err := ds.compact(); err == nil {
		t.Fatal("first compact should fail")
	}
	rotated, err := filepath.Glob(ds.journalPath + ".compact-*")
	if err != nil || len(rotated) != 1 {
		t.Fatalf("rotated journals after failure = %v, err = %v", rotated, err)
	}
	ds.writeSnapshot = writePrivateFileAtomically
	if err := ds.compact(); err != nil {
		t.Fatalf("retry compact: %v", err)
	}
	rotated, err = filepath.Glob(ds.journalPath + ".compact-*")
	if err != nil || len(rotated) != 0 {
		t.Fatalf("rotated journals after retry = %v, err = %v", rotated, err)
	}
	ds.Close()

	reopened, err := NewDedupeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.Contains("retry-entry") {
		t.Fatal("entry should survive failed and retried compaction")
	}
}

func TestDedupeStore_ReusesJournalFileUntilRotation(t *testing.T) {
	ds, err := NewDedupeStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ds.Commit("first")
	ds.mu.Lock()
	firstFile := ds.journalFile
	ds.mu.Unlock()
	if firstFile == nil {
		t.Fatal("journal file should be open after commit")
	}
	ds.Commit("second")
	ds.mu.Lock()
	secondFile := ds.journalFile
	ds.mu.Unlock()
	if secondFile != firstFile {
		t.Fatal("consecutive commits should reuse the journal file")
	}
	if err := ds.compact(); err != nil {
		t.Fatal(err)
	}
	ds.mu.Lock()
	afterRotation := ds.journalFile
	ds.mu.Unlock()
	if afterRotation != nil {
		t.Fatal("journal file should be closed after rotation")
	}
	ds.Close()
}

func TestDedupeStore_CloseReleasesJournalFile(t *testing.T) {
	ds, err := NewDedupeStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ds.Commit("entry")
	ds.mu.Lock()
	f := ds.journalFile
	ds.mu.Unlock()
	if f == nil {
		t.Fatal("journal file should be open after commit")
	}
	ds.Close()
	if _, err := f.Write([]byte("closed")); err == nil {
		t.Fatal("journal file should be closed after store close")
	}
}

func BenchmarkDedupeStoreCommitJournal(b *testing.B) {
	ds, err := NewDedupeStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(ds.Close)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ds.Commit(fmt.Sprintf("message-%d", i))
	}
}

func BenchmarkDedupeStoreCompaction(b *testing.B) {
	for _, entries := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("entries-%d", entries), func(b *testing.B) {
			ds, err := NewDedupeStore(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(ds.Close)
			expiresAt := time.Now().Add(time.Hour)
			ds.mu.Lock()
			for i := 0; i < entries; i++ {
				key := fmt.Sprintf("message-%d", i)
				ds.entries[key] = dedupeEntry{Key: key, Committed: true, ExpiresAt: expiresAt}
			}
			ds.dirty = true
			ds.mu.Unlock()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := ds.compact(); err != nil {
					b.Fatal(err)
				}
				ds.mu.Lock()
				ds.dirty = true
				ds.mu.Unlock()
			}
		})
	}
}
