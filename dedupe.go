// Package main implements a lightweight deduplication store for Feishu messages.
// It uses an in-memory hot cache with file-backed persistence for TTL survival across restarts.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	defaultDedupeTTL    = 24 * time.Hour
	dedupeCleanupPeriod = 5 * time.Minute
	dedupeFileName      = "dedupe.json"
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
	mu          sync.Mutex
	entries     map[string]dedupeEntry // key → entry
	ttl         time.Duration
	storagePath string
	stopCleanup chan struct{}
	closeOnce   sync.Once
}

// NewDedupeStore creates a new DedupeStore with file persistence.
func NewDedupeStore(storageDir string) (*DedupeStore, error) {
	if err := os.MkdirAll(storageDir, 0700); err != nil {
		return nil, fmt.Errorf("create dedupe storage dir: %w", err)
	}

	ds := &DedupeStore{
		entries:     make(map[string]dedupeEntry),
		ttl:         defaultDedupeTTL,
		storagePath: filepath.Join(storageDir, dedupeFileName),
		stopCleanup: make(chan struct{}),
	}

	// Load persisted entries.
	ds.loadFromFile()
	// Clean expired entries on startup.
	ds.cleanExpired()

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

	ds.entries[key] = dedupeEntry{
		Key:       key,
		Committed: true,
		ExpiresAt: time.Now().Add(ds.ttl),
	}
	ds.saveToFileLocked()
}

// Release removes an in-flight reservation without marking as committed.
// Use this when processing fails or a message is rejected (e.g., owner filter, queue full).
func (ds *DedupeStore) Release(key string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	delete(ds.entries, key)
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
		return false
	}
	return true
}

// Close stops the background cleanup goroutine.
func (ds *DedupeStore) Close() {
	ds.closeOnce.Do(func() {
		close(ds.stopCleanup)
	})
}

func (ds *DedupeStore) cleanupLoop() {
	ticker := time.NewTicker(dedupeCleanupPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ds.stopCleanup:
			return
		case <-ticker.C:
			ds.mu.Lock()
			ds.cleanExpired()
			ds.saveToFileLocked()
			ds.mu.Unlock()
		}
	}
}

// cleanExpired removes expired entries. Caller must hold ds.mu.
func (ds *DedupeStore) cleanExpired() {
	now := time.Now()
	for k, e := range ds.entries {
		if now.After(e.ExpiresAt) {
			delete(ds.entries, k)
		}
	}
}

// saveToFileLocked persists committed entries to disk. Caller must hold ds.mu.
func (ds *DedupeStore) saveToFileLocked() {
	if ds.storagePath == "" {
		return
	}
	// Only persist committed entries (in-flight are transient).
	var toSave []dedupeEntry
	for _, e := range ds.entries {
		if e.Committed {
			toSave = append(toSave, e)
		}
	}
	data, err := json.Marshal(toSave)
	if err != nil {
		slog.Error("dedupe: failed to marshal entries", "error", err)
		return
	}
	if err := writeFileAtomically(ds.storagePath, data, 0o600); err != nil {
		slog.Error("dedupe: failed to write file", "error", err)
	}
}

// loadFromFile loads committed entries from disk. Caller must NOT hold ds.mu.
func (ds *DedupeStore) loadFromFile() {
	if ds.storagePath == "" {
		return
	}
	data, err := os.ReadFile(ds.storagePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("dedupe: failed to read file", "error", err)
		}
		return
	}
	var entries []dedupeEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Warn("dedupe: failed to parse file", "error", err)
		return
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	for _, e := range entries {
		if e.Committed && time.Now().Before(e.ExpiresAt) {
			ds.entries[e.Key] = e
		}
	}
	slog.Info("dedupe: loaded entries from file", "count", len(ds.entries))
}
