package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func telegramTestDigest(text string) string {
	d := sha256.Sum256([]byte(text))
	return hex.EncodeToString(d[:])
}

func TestTelegramDuplicateDeliveryTracksFingerprintAndCleanupKeepsOriginalIndex(t *testing.T) {
	s := testIngressStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	r := testIngressTask(t, s, "update:10")
	r.SemanticID, r.InputDigest = "message:chat:1", telegramTestDigest("original")
	original := mustAcceptIngress(t, s, r)
	r.BusinessID = "update:11"
	duplicate := s.AcceptRecord(context.Background(), r, nil)
	if duplicate.Disposition != im.ReceiveDuplicate || duplicate.RecordID == original.RecordID {
		t.Fatalf("new delivery lacks fingerprint: %+v", duplicate)
	}
	r.InputDigest = telegramTestDigest("different")
	conflict := s.AcceptRecord(context.Background(), r, nil)
	if conflict.Disposition != im.ReceiveRejected || conflict.Reason != "update_id_conflict" {
		t.Fatalf("duplicate delivery ID conflict: %+v", conflict)
	}
	// The duplicate/conflict tombstones expire before the original record.
	now = now.Add(24 * time.Hour)
	if err := s.UpdateRecord(context.Background(), original.RecordID, []im.IngressState{"received"}, func(r *IngressRecord) error {
		r.State = "consumed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	s.pruneHistory(im.ChatIdentity{})
	r.BusinessID, r.InputDigest = "update:12", telegramTestDigest("original")
	result := s.AcceptRecord(context.Background(), r, nil)
	if result.Disposition != im.ReceiveDuplicate || result.Reason != "semantic_duplicate" {
		t.Fatalf("cleanup removed original semantic index: %+v", result)
	}
}

func TestTelegramUpdateConflictPreservesOriginalAndCommitsCheckpoint(t *testing.T) {
	for _, change := range []string{"body", "semantic", "chat", "control"} {
		t.Run(change, func(t *testing.T) {
			s := testIngressStore(t)
			r := testIngressTask(t, s, "update:10")
			r.SemanticID = "message:chat:1"
			r.InputDigest = telegramTestDigest(r.Text)
			if change == "control" {
				r.Kind = "control"
				r.Text = ""
				r.InputDigest = telegramTestDigest("/allow first")
			}
			original := mustAcceptIngress(t, s, r)
			switch change {
			case "body":
				r.InputDigest = telegramTestDigest("different original input")
			case "semantic", "control":
				r.SemanticID = "message:chat:2"
			case "chat":
				r.ChatID = "other-chat"
			}
			result := s.AcceptRecord(context.Background(), r, testTelegramIngressCursor(1, 11))
			if result.Disposition != im.ReceiveRejected || result.Reason != "update_id_conflict" || result.RecordID == original.RecordID {
				t.Fatalf("conflict result: %+v", result)
			}
			conflict, err := s.Get(result.RecordID)
			if err != nil || conflict.Text != "" || conflict.State != "rejected" {
				t.Fatalf("conflict persisted body or is not terminal: %+v %v", conflict, err)
			}
			got, err := s.Get(original.RecordID)
			if err != nil || got.Text != original.Text || got.SemanticID != original.SemanticID || got.InputDigest != original.InputDigest || got.State != original.State {
				t.Fatalf("original changed: %+v %v", got, err)
			}
			cursor, err := s.Cursor("telegram", "bot")
			if err != nil || cursor.Generation != 1 || *cursor.Telegram.PendingOffset != 11 {
				t.Fatalf("conflict checkpoint: %+v %v", cursor, err)
			}
			repeat := s.AcceptRecord(context.Background(), r, testTelegramIngressCursor(2, 11))
			if repeat.Disposition != im.ReceiveDuplicate || repeat.Reason != "update_id_conflict" || repeat.RecordID != result.RecordID {
				t.Fatalf("repeat conflict: %+v", repeat)
			}
			rows, err := s.listAll()
			if err != nil || len(rows) != 2 {
				t.Fatalf("repeat grew tombstones: %+v %v", rows, err)
			}
		})
	}
}

func TestTelegramConflictCursorFailureRollsBackIsolation(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "update:10")
	r.InputDigest = telegramTestDigest("original")
	mustAcceptIngress(t, s, r)
	r.InputDigest = telegramTestDigest("conflict")
	result := s.AcceptRecord(context.Background(), r, testTelegramIngressCursor(2, 11))
	if result.Disposition != im.ReceiveRetryable || result.Reason != "cursor_generation_conflict" {
		t.Fatalf("stale checkpoint: %+v", result)
	}
	rows, err := s.listAll()
	if err != nil || len(rows) != 1 {
		t.Fatalf("partial conflict commit: %+v %v", rows, err)
	}
}

func TestTelegramCursorAccountAndDiagnosticClone(t *testing.T) {
	s := testIngressStore(t)
	c := testTelegramIngressCursor(1, 11)
	c.Telegram.BotID = "different-bot"
	if err := s.CommitCursor(context.Background(), "telegram", "bot", 0, *c); err == nil {
		t.Fatal("foreign Bot checkpoint committed")
	}
	id := int64(10)
	c.Telegram.LastObservedUpdateID = &id
	copy := cloneIngressCursor(c)
	*copy.Telegram.LastObservedUpdateID = 20
	if *c.Telegram.LastObservedUpdateID != 10 {
		t.Fatal("cloned diagnostic ID aliases original")
	}
}

func TestTelegramControlDeduplicationDoesNotPersistParameterDigests(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "update:10")
	r.Kind, r.SemanticID = "control", "message:chat:1"
	r.Text = "/bind /workspace pairing-secret"
	r.InputDigest = telegramTestDigest(r.Text)
	original := mustAcceptIngress(t, s, r)
	if original.Text != "" || original.InputDigest != "" {
		t.Fatalf("control persisted parameters or their hashes: %+v", original)
	}
	r.InputDigest = telegramTestDigest("different parameters")
	if repeat := s.AcceptRecord(t.Context(), r, nil); repeat.Disposition != im.ReceiveDuplicate || repeat.RecordID != original.RecordID {
		t.Fatalf("control parameter change must not execute the same delivery twice: %+v", repeat)
	}
	r.BusinessID = "update:11"
	duplicate := s.AcceptRecord(t.Context(), r, nil)
	if duplicate.Disposition != im.ReceiveDuplicate || duplicate.RecordID == original.RecordID {
		t.Fatalf("control redelivery lacks separate identity evidence: %+v", duplicate)
	}
	r.SemanticID = "message:chat:2"
	conflict := s.AcceptRecord(t.Context(), r, nil)
	if conflict.Reason != "update_id_conflict" || conflict.Disposition != im.ReceiveRejected {
		t.Fatalf("control identity conflict not quarantined: %+v", conflict)
	}
	rows, err := s.listAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.InputDigest != "" || row.Text != "" {
			t.Fatalf("sensitive evidence persisted: %+v", row)
		}
	}
}
