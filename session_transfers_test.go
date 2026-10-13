package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func takeoverTestRecord(state string) sessionTransfer {
	return sessionTransfer{Status: TransferStatus{BindingInstanceID: "binding-1", TransferID: "transfer-1", SessionID: "session-1", SourceRuntimeID: "runtime-1", SourceSessionEpoch: 1, Workspace: "/workspace", TargetKind: "tui", TargetInstanceID: "target-1", State: state}, Direction: "outgoing", BindingInstanceID: "binding-1"}
}

func TestSessionTransferGuardSurvivesRestart(t *testing.T) {
	for _, state := range []string{"draining", "saved", "released", "status_unknown", "restore_failed"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			first := newSessionTransferStore(dir)
			record := takeoverTestRecord(state)
			if err := first.update("key", &record, ""); err != nil {
				t.Fatal(err)
			}
			second := newSessionTransferStore(dir)
			if err := second.Load(); err != nil {
				t.Fatal(err)
			}
			if !second.Blocked("key") || second.Blocked("other") {
				t.Fatal("restart lost binding isolation or transfer guard")
			}
			got, ok, err := second.Get("key")
			if err != nil || !ok || got.Status.State != state {
				t.Fatalf("loaded record = %#v, %v", got, err)
			}
			info, err := os.Stat(filepath.Join(dir, "session-transfers.json"))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("file mode = %v, %v", info, err)
			}
		})
	}
}

func TestSessionTransferFailedPersistenceKeepsGuard(t *testing.T) {
	store := newSessionTransferStore(t.TempDir())
	record := takeoverTestRecord("draining")
	if err := store.update("key", &record, ""); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("disk unavailable")
	store.writer = func(string, []byte) error { return injected }
	if err := store.update("key", nil, record.Status.TransferID); !errors.Is(err, injected) {
		t.Fatalf("clear error = %v", err)
	}
	record.Status.State = "released"
	if err := store.update("key", &record, record.Status.TransferID); !errors.Is(err, injected) {
		t.Fatalf("advance error = %v", err)
	}
	got, _, _ := store.Get("key")
	if !store.Blocked("key") || got.Status.State != "draining" {
		t.Fatalf("failed writer published %#v", got)
	}
	fresh := newSessionTransferStore(filepath.Dir(store.path))
	if err := fresh.Load(); err != nil {
		t.Fatal(err)
	}
	if !fresh.Blocked("key") {
		t.Fatal("failed clear opened binding after restart")
	}
}

func TestSessionTransferInvalidStorageFailsClosed(t *testing.T) {
	for _, data := range []string{"{", `{"key":{"status":{"session_id":"session-1"}}}`} {
		t.Run(data, func(t *testing.T) {
			store := newSessionTransferStore(t.TempDir())
			if err := os.WriteFile(store.path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if store.Load() == nil || !store.Blocked("any-key") {
				t.Fatal("invalid state enabled automatic admission")
			}
			record := takeoverTestRecord("draining")
			if store.update("key", &record, "") == nil {
				t.Fatal("invalid storage allowed overwrite")
			}
		})
	}
}

func TestSessionTransferIdentityAndImmutableBlockers(t *testing.T) {
	store := newSessionTransferStore(t.TempDir())
	record := takeoverTestRecord("draining")
	record.Status.Blockers = []TransferBlocker{{Description: "active tool"}}
	if err := store.update("key", &record, ""); err != nil {
		t.Fatal(err)
	}
	record.Status.Blockers[0].Description = "changed"
	got, _, _ := store.Get("key")
	if got.Status.Blockers[0].Description != "active tool" {
		t.Fatal("caller changed stored blocker")
	}
	got.Status.Blockers[0].Description = "changed again"
	if store.update("key", nil, "old-transfer") == nil {
		t.Fatal("stale clear accepted")
	}
	if store.update("key", &record, "") == nil {
		t.Fatal("duplicate start accepted")
	}
	got, _, _ = store.Get("key")
	if got.Status.Blockers[0].Description != "active tool" || !store.Blocked("key") {
		t.Fatal("query or stale mutation changed guard")
	}
}

func TestSessionPinClearIfPreservesNewSession(t *testing.T) {
	store := newSessionPinStore(t.TempDir())
	if err := store.Set("key", "session-old"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("key", "session-new"); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearIf("key", "session-old"); err != nil {
		t.Fatal(err)
	}
	reloaded := newSessionPinStore(filepath.Dir(store.path))
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if reloaded.Get("key") != "session-new" {
		t.Fatal("old source cleared newer pin")
	}
	if err := store.ClearIf("key", "session-new"); err != nil {
		t.Fatal(err)
	}
	if store.Get("key") != "" {
		t.Fatal("matching pin not cleared")
	}
}

func TestTransferDeletionAfterRenameFailureKeepsRestartGuard(t *testing.T) {
	dir := t.TempDir()
	store := newSessionTransferStore(dir)
	record := takeoverTestRecord("released")
	if err := store.update("key", &record, ""); err != nil {
		t.Fatal(err)
	}
	store.writer = func(path string, data []byte) error {
		if err := writePrivateFileSynced(path, data); err != nil {
			return err
		}
		if path == store.path {
			// Model a directory sync error after the primary rename replaced
			// its contents. The caller has no successful deletion receipt.
			return errors.New("directory sync failed after replacement")
		}
		return nil
	}
	if err := store.update("key", nil, record.Status.TransferID); err == nil {
		t.Fatal("uncertain deletion was acknowledged")
	}
	data, err := os.ReadFile(store.path)
	if err != nil || string(data) != "{}" {
		t.Fatalf("failure did not replace primary guard file: %q, %v", data, err)
	}
	loaded := newSessionTransferStore(dir)
	if err := loaded.Load(); err != nil || !loaded.Blocked("key") {
		t.Fatalf("restart reopened an unconfirmed deletion: %v", err)
	}
	if err := loaded.update("key", nil, record.Status.TransferID); err != nil {
		t.Fatal(err)
	}
	final := newSessionTransferStore(dir)
	if err := final.Load(); err != nil || final.Blocked("key") {
		t.Fatalf("successful explicit retry retained the old guard: %v", err)
	}
}

func TestTransferDeletionJournalPreservesCurrentBindingAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	store := newSessionTransferStore(dir)
	old := takeoverTestRecord("released")
	journal, _ := json.Marshal(map[string]sessionTransfer{"key": old, "other": old})
	if err := writePrivateFileSynced(store.path+".pending", journal); err != nil {
		t.Fatal(err)
	}
	current := old
	current.Status.TransferID = "transfer-new"
	if err := store.update("key", &current, ""); err != nil {
		t.Fatal(err)
	}
	loaded := newSessionTransferStore(dir)
	if err := loaded.Load(); err != nil {
		t.Fatal(err)
	}
	got, _, _ := loaded.Get("key")
	if got.Status.TransferID != "transfer-new" || !loaded.Blocked("other") {
		t.Fatal("journal replaced a newer guard or lost another blocked binding")
	}
	if err := os.WriteFile(store.path+".pending", []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loaded.Load(); err == nil || !loaded.Blocked("unrelated") {
		t.Fatal("corrupt deletion journal enabled automatic admission")
	}
}

func TestSessionTransferReloadReplacesPublishedRecords(t *testing.T) {
	store := newSessionTransferStore(t.TempDir())
	record := takeoverTestRecord("released")
	if err := store.update("key", &record, ""); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFileSynced(store.path, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := store.Load(); err != nil || store.Blocked("key") {
		t.Fatalf("reload retained deleted records: %v", err)
	}
}

func TestSessionTransferInvalidDeletionJournalFailsClosed(t *testing.T) {
	for _, data := range []string{"null", `{"key":{"status":{"session_id":"session-1"}}}`} {
		t.Run(data, func(t *testing.T) {
			store := newSessionTransferStore(t.TempDir())
			record := takeoverTestRecord("released")
			if err := store.update("key", &record, ""); err != nil {
				t.Fatal(err)
			}
			if err := writePrivateFileSynced(store.path+".pending", []byte(data)); err != nil {
				t.Fatal(err)
			}
			if err := store.Load(); err == nil || !store.Blocked("unrelated") {
				t.Fatal("invalid journal hidden by a primary record enabled admission")
			}
		})
	}
}

func TestTransferUnchangedObservationSkipsWriteUnlessUncertain(t *testing.T) {
	store := newSessionTransferStore(t.TempDir())
	record := takeoverTestRecord("draining")
	writes := 0
	writer := store.writer
	store.writer = func(path string, data []byte) error { writes++; return writer(path, data) }
	if err := store.update("key", &record, ""); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err := store.update("key", &record, record.Status.TransferID); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 1 {
		t.Fatalf("unchanged observations performed %d writes", writes)
	}
	advanced := record
	advanced.Status.State = "saved"
	store.writer = func(path string, data []byte) error {
		if err := writer(path, data); err != nil {
			return err
		}
		return errors.New("sync outcome unknown")
	}
	if err := store.update("key", &advanced, record.Status.TransferID); err == nil {
		t.Fatal("uncertain write accepted")
	}
	store.writer = writer
	if err := store.update("key", &record, record.Status.TransferID); err != nil {
		t.Fatal(err)
	}
	reloaded := newSessionTransferStore(filepath.Dir(store.path))
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	got, _, _ := reloaded.Get("key")
	if got.Status.State != "draining" {
		t.Fatal("unchanged memory skipped uncertain disk repair")
	}
}

func BenchmarkTransferUnchangedObservation(b *testing.B) {
	store := newSessionTransferStore(b.TempDir())
	record := takeoverTestRecord("draining")
	record.Status.Blockers = []TransferBlocker{{OwnerTaskID: "main", Type: "tool", Description: "Active tool"}}
	if err := store.update("key", &record, ""); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if err := store.update("key", &record, record.Status.TransferID); err != nil {
			b.Fatal(err)
		}
	}
}
