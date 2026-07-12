// Package main implements a lightweight deduplication store for Feishu messages.
// It uses an in-memory hot cache with file-backed persistence for TTL survival across restarts.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/keakon/golog/log"
)

const (
	defaultDedupeTTL    = 24 * time.Hour
	dedupeCleanupPeriod = 5 * time.Minute
	dedupeFileName      = "dedupe.json"
	dedupeJournalSuffix = ".journal"
)

// dedupeEntry tracks a message's deduplication state.
type dedupeEntry struct {
	Key       string    `json:"key"`
	Committed bool      `json:"committed"` // true = fully processed; false = in-flight reservation
	ExpiresAt time.Time `json:"expires_at"`
}

// DedupeStore provides deduplication for incoming messages.
// It supports in-flight reservation (TryBegin → Commit/Release) so that
// a message that is being processed does not get re-enqueued concurrently.
type DedupeStore struct {
	mu            sync.Mutex
	compactMu     sync.Mutex
	entries       map[string]dedupeEntry // key → entry
	ttl           time.Duration
	storagePath   string
	journalPath   string
	stopCleanup   chan struct{}
	cleanupDone   chan struct{}
	closeOnce     sync.Once
	dirty         bool
	journalFile   *os.File
	writeSnapshot func(string, []byte) error
}

// NewDedupeStore creates a new DedupeStore with file persistence.
func NewDedupeStore(storageDir string) (*DedupeStore, error) {
	if err := os.MkdirAll(storageDir, 0700); err != nil {
		return nil, fmt.Errorf("create dedupe storage dir: %w", err)
	}

	ds := &DedupeStore{
		entries:       make(map[string]dedupeEntry),
		ttl:           defaultDedupeTTL,
		storagePath:   filepath.Join(storageDir, dedupeFileName),
		journalPath:   filepath.Join(storageDir, dedupeFileName+dedupeJournalSuffix),
		stopCleanup:   make(chan struct{}),
		cleanupDone:   make(chan struct{}),
		writeSnapshot: writePrivateFileAtomically,
	}

	// Load persisted entries.
	ds.loadFromFiles()

	go ds.cleanupLoop()

	return ds, nil
}

// TryBegin attempts to reserve a key for in-flight processing.
// Returns true if the key is new (reservation acquired), false if already
// in-flight or committed (duplicate).
func (ds *DedupeStore) TryBegin(key string) bool {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if e, ok := ds.entries[key]; ok {
		// Check if expired.
		if time.Now().After(e.ExpiresAt) {
			delete(ds.entries, key)
			ds.dirty = true
			// Treat as new — fall through to add.
		} else {
			// Duplicate: either in-flight or committed.
			return false
		}
	}

	ds.entries[key] = dedupeEntry{
		Key:       key,
		Committed: false,
		ExpiresAt: time.Now().Add(ds.ttl),
	}
	return true
}

// Commit marks a key as fully processed (persisted).
// This ensures that across restarts, the same message is not re-processed.
func (ds *DedupeStore) Commit(key string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	entry := dedupeEntry{
		Key:       key,
		Committed: true,
		ExpiresAt: time.Now().Add(ds.ttl),
	}
	ds.entries[key] = entry
	ds.dirty = true
	if err := ds.appendToJournalLocked(entry); err != nil {
		log.Errorf("dedupe: failed to append journal error=%v", err)
	}
}

// Release removes an in-flight reservation without marking as committed.
// Use this when processing fails or a message is rejected (e.g., owner filter, queue full).
func (ds *DedupeStore) Release(key string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	delete(ds.entries, key)
	ds.dirty = true
}

// Contains returns true if the key is already known (in-flight or committed).
func (ds *DedupeStore) Contains(key string) bool {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	e, ok := ds.entries[key]
	if !ok {
		return false
	}
	if time.Now().After(e.ExpiresAt) {
		delete(ds.entries, key)
		ds.dirty = true
		return false
	}
	return true
}

// Close stops the background cleanup goroutine.
func (ds *DedupeStore) Close() {
	ds.closeOnce.Do(func() {
		close(ds.stopCleanup)
		<-ds.cleanupDone
		if err := ds.compact(); err != nil {
			log.Errorf("dedupe: failed to compact on close error=%v", err)
		}
		ds.mu.Lock()
		if err := ds.closeJournalLocked(); err != nil {
			log.Errorf("dedupe: failed to close journal error=%v", err)
		}
		ds.mu.Unlock()
	})
}

func (ds *DedupeStore) cleanupLoop() {
	defer close(ds.cleanupDone)
	ticker := time.NewTicker(dedupeCleanupPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ds.stopCleanup:
			return
		case <-ticker.C:
			if err := ds.compact(); err != nil {
				log.Errorf("dedupe: failed to compact during cleanup error=%v", err)
			}
		}
	}
}

// cleanExpiredLocked removes expired entries and reports whether anything changed.
// Caller must hold ds.mu.
func (ds *DedupeStore) cleanExpiredLocked() bool {
	now := time.Now()
	dirty := false
	for k, e := range ds.entries {
		if now.After(e.ExpiresAt) {
			delete(ds.entries, k)
			dirty = true
		}
	}
	return dirty
}

// compact rotates the active journal under the store lock, then writes the
// snapshot without blocking message deduplication. Rotated journals remain
// recoverable until the snapshot has been replaced successfully.
func (ds *DedupeStore) compact() error {
	if ds.storagePath == "" {
		return nil
	}
	ds.compactMu.Lock()
	defer ds.compactMu.Unlock()

	ds.mu.Lock()
	if ds.cleanExpiredLocked() {
		ds.dirty = true
	}
	rotated, err := filepath.Glob(ds.journalPath + ".compact-*")
	if err != nil {
		ds.mu.Unlock()
		return err
	}
	if !ds.dirty && len(rotated) == 0 {
		ds.mu.Unlock()
		return nil
	}
	if _, err := os.Stat(ds.journalPath); err == nil {
		rotatedPath, err := ds.rotateJournalLocked()
		if err != nil {
			ds.mu.Unlock()
			return err
		}
		rotated = append(rotated, rotatedPath)
	} else if !os.IsNotExist(err) {
		ds.mu.Unlock()
		return err
	}

	var toSave []dedupeEntry
	for _, e := range ds.entries {
		if e.Committed {
			toSave = append(toSave, e)
		}
	}
	ds.dirty = false
	ds.mu.Unlock()

	data, err := json.Marshal(toSave)
	if err != nil {
		ds.markDirty()
		return err
	}
	if err := ds.writeSnapshot(ds.storagePath, data); err != nil {
		ds.markDirty()
		return err
	}
	for _, path := range rotated {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// rotateJournalLocked gives the current journal a unique recovery name.
// Caller must hold ds.mu.
func (ds *DedupeStore) rotateJournalLocked() (string, error) {
	if err := ds.closeJournalLocked(); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(ds.journalPath), filepath.Base(ds.journalPath)+".compact-*")
	if err != nil {
		return "", err
	}
	path := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	if err := os.Rename(ds.journalPath, path); err != nil {
		return "", err
	}
	return path, nil
}

func (ds *DedupeStore) markDirty() {
	ds.mu.Lock()
	ds.dirty = true
	ds.mu.Unlock()
}

// appendToJournalLocked records one commit without rewriting the snapshot.
// Caller must hold ds.mu.
func (ds *DedupeStore) appendToJournalLocked(entry dedupeEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if ds.journalFile == nil {
		f, err := os.OpenFile(ds.journalPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, privateFileMode)
		if err != nil {
			return err
		}
		if err := f.Chmod(privateFileMode); err != nil {
			_ = f.Close()
			return err
		}
		ds.journalFile = f
	}
	if _, err := ds.journalFile.Write(data); err != nil {
		_ = ds.closeJournalLocked()
		return err
	}
	return nil
}

// closeJournalLocked releases the active append handle before rotation or
// shutdown. Caller must hold ds.mu.
func (ds *DedupeStore) closeJournalLocked() error {
	if ds.journalFile == nil {
		return nil
	}
	f := ds.journalFile
	ds.journalFile = nil
	return f.Close()
}

// loadFromFiles loads the compacted snapshot and commits appended after it.
func (ds *DedupeStore) loadFromFiles() {
	if ds.storagePath == "" {
		return
	}
	data, err := os.ReadFile(ds.storagePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("dedupe: failed to read file error=%v", err)
		}
	} else if err := ds.loadSnapshot(data); err != nil {
		log.Warnf("dedupe: failed to parse file error=%v", err)
	}

	rotated, err := filepath.Glob(ds.journalPath + ".compact-*")
	if err != nil {
		log.Warnf("dedupe: failed to find rotated journals error=%v", err)
	}
	for _, path := range append(rotated, ds.journalPath) {
		ds.loadJournal(path)
	}
	log.Infof("dedupe: loaded entries from file count=%v", len(ds.entries))
}

func (ds *DedupeStore) loadJournal(path string) {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("dedupe: failed to read journal path=%v error=%v", path, err)
		}
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry dedupeEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			log.Warnf("dedupe: failed to parse journal entry path=%v error=%v", path, err)
			continue
		}
		ds.loadEntry(entry)
	}
	if err := scanner.Err(); err != nil {
		log.Warnf("dedupe: failed to scan journal path=%v error=%v", path, err)
	}
}

func (ds *DedupeStore) loadSnapshot(data []byte) error {
	var entries []dedupeEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	for _, entry := range entries {
		ds.loadEntry(entry)
	}
	return nil
}

func (ds *DedupeStore) loadEntry(entry dedupeEntry) {
	current, exists := ds.entries[entry.Key]
	if entry.Committed && time.Now().Before(entry.ExpiresAt) && (!exists || current.ExpiresAt.Before(entry.ExpiresAt)) {
		ds.entries[entry.Key] = entry
	}
}
