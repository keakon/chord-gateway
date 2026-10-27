package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/keakon/golog/log"
)

const (
	transferStatePreflight     = "preflight"
	transferStatePrepareFailed = "prepare_failed"
)

var ErrSessionTransferred = errors.New("session transfer blocks this binding; check /takeover status or explicitly use /new or /resume")

// TransferStatus is the bounded public coordinator response. It contains no
// queued text, model output, secrets or tool arguments.
type TransferStatus struct {
	BindingInstanceID  string            `json:"binding_instance_id,omitempty"`
	TransferID         string            `json:"transfer_id"`
	SessionID          string            `json:"session_id"`
	SourceRuntimeID    string            `json:"source_runtime_id"`
	SourceSessionEpoch uint64            `json:"source_session_epoch"`
	Workspace          string            `json:"workspace"`
	TargetKind         string            `json:"target_kind"`
	TargetInstanceID   string            `json:"target_instance_id"`
	State              string            `json:"state"`
	StartedAt          string            `json:"started_at,omitempty"`
	WaitExpired        bool              `json:"wait_expired,omitempty"`
	PendingInputs      int               `json:"pending_inputs,omitempty"`
	CheckpointID       string            `json:"checkpoint_id,omitempty"`
	ErrorCode          string            `json:"error_code,omitempty"`
	Message            string            `json:"message,omitempty"`
	RecoverableAction  string            `json:"recoverable_action,omitempty"`
	Blockers           []TransferBlocker `json:"blockers,omitempty"`
	BlockerCount       int               `json:"blocker_count,omitempty"`
}
type TransferBlocker struct {
	OwnerTaskID string `json:"owner_task_id"`
	Type        string `json:"type"`
	StartedAt   string `json:"started_at"`
	Description string `json:"description"`
}
type sessionTransfer struct {
	Status            TransferStatus `json:"status"`
	Interrupted       bool           `json:"interrupted,omitempty"`
	Direction         string         `json:"direction"` // incoming or outgoing
	BindingInstanceID string         `json:"binding_instance_id"`
}

// One lock serializes disk and memory publication. A failed write never opens
// a blocked binding; a load error blocks all automatic admission.
type sessionTransferStore struct {
	mu             sync.Mutex
	path           string
	entries        map[string]sessionTransfer
	loadErr        error
	writeUncertain bool
	writer         func(string, []byte) error
}

func newSessionTransferStore(dir string) *sessionTransferStore {
	return &sessionTransferStore{path: filepath.Join(dir, "session-transfers.json"), entries: make(map[string]sessionTransfer), writer: writePrivateFileSynced}
}
func (s *sessionTransferStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := readSessionTransferRecords(s.path)
	if err == nil {
		var pending map[string]sessionTransfer
		pending, err = readSessionTransferRecords(s.path + ".pending")
		for key, record := range pending {
			// A current guard wins over an older deletion journal. Missing
			// bindings remain blocked when deletion's durability was uncertain.
			if _, exists := entries[key]; !exists {
				entries[key] = record
			}
		}
	}
	if err != nil {
		s.loadErr = fmt.Errorf("load session transfers: %w", err)
		return s.loadErr
	}
	s.entries = entries
	s.loadErr = nil
	s.writeUncertain = false
	return nil
}

func readSessionTransferRecords(path string) (map[string]sessionTransfer, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(map[string]sessionTransfer), nil
	}
	if err != nil {
		return nil, err
	}
	var records map[string]sessionTransfer
	if err = json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	if records == nil {
		return nil, errors.New("invalid session transfer storage")
	}
	for _, record := range records {
		if record.Status.SessionID == "" || record.Status.TransferID == "" {
			return nil, errors.New("invalid session transfer record")
		}
	}
	return records, nil
}
func (s *sessionTransferStore) Get(key string) (sessionTransfer, bool, error) {
	if s == nil {
		return sessionTransfer{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.entries[key]
	v.Status.Blockers = append([]TransferBlocker(nil), v.Status.Blockers...)
	return v, ok, s.loadErr
}
func (s *sessionTransferStore) Blocked(key string) bool {
	_, ok, err := s.Get(key)
	return ok || err != nil
}
func (s *sessionTransferStore) update(key string, value *sessionTransfer, expectedID string) error {
	return s.commit(key, value, expectedID, true)
}

// replace supersedes the transfer named by expectedID instead of extending it.
// A new transfer on the same binding is a separate operation, so it must not
// inherit the previous transfer's interruption evidence or start time. Only an
// explicit takeover of a finished transfer uses this path.
func (s *sessionTransferStore) replace(key string, value sessionTransfer, expectedID string) error {
	if expectedID == "" {
		return errors.New("replace requires a transfer identity")
	}
	return s.commit(key, &value, expectedID, false)
}

func (s *sessionTransferStore) commit(key string, value *sessionTransfer, expectedID string, inherit bool) error {
	if s == nil {
		return errors.New("session transfer storage unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return s.loadErr
	}
	old, ok := s.entries[key]
	if expectedID != "" && (!ok || old.Status.TransferID != expectedID) {
		return errors.New("stale transfer")
	}
	if expectedID == "" && ok {
		return errors.New("transfer_busy")
	}
	var normalized sessionTransfer
	if value != nil {
		v := *value
		if inherit {
			v.Interrupted = v.Interrupted || old.Interrupted
			if old.Status.StartedAt != "" {
				v.Status.StartedAt = old.Status.StartedAt
			}
		}
		v.Status.WaitExpired = false
		v.Status.Blockers = append([]TransferBlocker(nil), v.Status.Blockers...)
		if ok && !s.writeUncertain && reflect.DeepEqual(old, v) {
			return nil
		}
		normalized = v
	}
	next := maps.Clone(s.entries)
	if value == nil {
		delete(next, key)
	} else {
		next[key] = normalized
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session transfers: %w", err)
	}
	s.writeUncertain = true
	if value == nil {
		// Rename may have replaced the primary file before directory fsync
		// reports failure. Keep the old guards durable until deletion commits.
		previous, marshalErr := json.MarshalIndent(s.entries, "", "  ")
		if marshalErr != nil {
			return fmt.Errorf("marshal transfer deletion journal: %w", marshalErr)
		}
		if err = s.writer(s.path+".pending", previous); err != nil {
			return fmt.Errorf("persist transfer deletion journal: %w", err)
		}
	}
	if err = s.writer(s.path, data); err != nil {
		return fmt.Errorf("persist session transfers: %w", err)
	}
	if value == nil {
		if err = os.Remove(s.path + ".pending"); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove committed transfer deletion journal: %w", err)
		}
		// The primary deletion is already synced. Failure here can only make
		// a conservative old guard reappear after a crash, never lose a guard.
		if err = syncTransferDirectory(filepath.Dir(s.path)); err != nil {
			log.Warnf("transfer deletion committed; journal cleanup sync failed: %v", err)
		}
	}
	s.entries = next
	s.writeUncertain = false
	return nil
}

func (s *sessionTransferStore) failClosed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadErr = fmt.Errorf("transfer guard persistence unavailable: %w", err)
}
