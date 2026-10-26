package gateway

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// refetchChunk builds the image_artifact payload Chord sends for a re-request:
// empty call and agent ids, and no preceding image_result.
func refetchChunk(t *testing.T, m receivedImageMetadata, offset int, last bool, data []byte) json.RawMessage {
	t.Helper()
	payload := map[string]any{
		"reference":  m.Reference,
		"request_id": "refetch-request",
		"size_bytes": m.Size,
		"mime_type":  m.MIME,
		"width":      m.Width,
		"height":     m.Height,
		"offset":     offset,
		"last":       last,
		"data":       base64.StdEncoding.EncodeToString(data),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestImageRefetchRecoversWithoutSummary proves the re-request path works. Chord
// answers /image <reference> with chunks alone, so the normal "chunk requires a
// preceding image_result" rule would reject every one of them. Metadata comes
// from the first chunk's own headers, verified against the digest baked into the
// reference.
func TestImageRefetchRecoversWithoutSummary(t *testing.T) {
	data := testImageBytes(t)
	m := testImageMetadata(data)
	initial := im.ImageSpoolBytes.Load()

	var receiver imageReceiver
	defer receiver.reset()

	if !receiver.expectRefetch("session-1", m.Reference, "refetch-request") {
		t.Fatal("expectRefetch refused a fresh reference")
	}
	if receiver.expectRefetch("session-1", m.Reference, "refetch-duplicate") {
		t.Fatal("a second outstanding resend must be refused")
	}

	got, err := receiver.chunk(refetchChunk(t, m, 0, false, data[:10]), "session-1")
	if err != nil || got != nil {
		t.Fatalf("first refetch chunk: %v", err)
	}
	image, err := receiver.chunk(refetchChunk(t, m, 10, true, data[10:]), "session-1")
	if err != nil || image == nil {
		t.Fatalf("final refetch chunk: %v", err)
	}
	defer image.Close()
	if image.SHA256 != m.SHA256 || image.Size != m.Size {
		t.Fatalf("image = %+v, want the original identity", image)
	}
	if image.SessionID != "session-1" {
		t.Fatalf("SessionID = %q, want session-1", image.SessionID)
	}
	if _, err := im.ReadReceivedImage(image); err != nil {
		t.Fatalf("resumed image unusable: %v", err)
	}
	image.Close()
	receiver.reset()
	if im.ImageSpoolBytes.Load() != initial {
		t.Fatal("refetch did not release its spool accounting")
	}
}

func TestImageRefetchCoexistsWithOrdinaryTransfer(t *testing.T) {
	data := testImageBytes(t)
	meta := testImageMetadata(data)
	var receiver imageReceiver
	defer receiver.reset()
	if err := receiver.result(testImageResult(t, "ordinary-call", []receivedImageMetadata{meta}), "session-1"); err != nil {
		t.Fatal(err)
	}
	if !receiver.expectRefetch("session-1", meta.Reference, "refetch-request") {
		t.Fatal("ordinary transfer prevented reserving a resend")
	}
	if image, err := receiver.chunk(refetchChunk(t, meta, 0, true, data), "session-1"); err != nil || image == nil {
		t.Fatalf("resend could not bootstrap: image=%v err=%v", image, err)
	} else {
		image.Close()
	}
	if image, err := receiver.chunk(testImageChunk(t, "ordinary-call", meta, 0, true, data), "session-1"); err != nil || image == nil {
		t.Fatalf("resend disturbed ordinary transfer: image=%v err=%v", image, err)
	} else {
		image.Close()
	}
}

func TestImageRefetchReservationCleanup(t *testing.T) {
	data := testImageBytes(t)
	meta := testImageMetadata(data)
	for _, cleanup := range []string{"send failure", "idle", "error", "session switch", "timeout"} {
		t.Run(cleanup, func(t *testing.T) {
			p := &ChordProcess{state: ControlState{SessionID: "session-1"}}
			defer p.images.reset()
			if !p.images.expectRefetch("session-1", meta.Reference, "refetch-request") {
				t.Fatal("reservation failed")
			}
			switch cleanup {
			case "send failure":
				p.images.cancelRefetch("refetch-request")
			case "idle":
				p.processEnvelope(&HeadlessEnvelope{Type: "idle", Payload: json.RawMessage(`{}`)})
			case "error":
				p.processEnvelope(&HeadlessEnvelope{Type: "error", Payload: json.RawMessage(`{"request_id":"refetch-request","message":"artifact unavailable"}`)})
			case "session switch":
				p.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Payload: json.RawMessage(`{"session_id":"session-2"}`)})
			case "timeout":
				p.images.mu.Lock()
				p.images.expected[meta.Reference].timer.Reset(time.Millisecond)
				p.images.mu.Unlock()
				deadline := time.Now().Add(time.Second)
				for {
					p.images.mu.Lock()
					count := len(p.images.expected)
					p.images.mu.Unlock()
					if count == 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("reservation timeout did not clean up")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if !p.images.expectRefetch(p.State().SessionID, meta.Reference, "retry-request") {
				t.Fatal("cleanup prevented a retry")
			}
		})
	}
}

func TestUncorrelatedErrorDoesNotCancelImageRefetches(t *testing.T) {
	var p ChordProcess
	defer p.images.reset()
	for _, ref := range []string{"artifact:images/first.png", "artifact:images/second.png"} {
		if !p.images.expectRefetch("session-1", ref, ref) {
			t.Fatalf("reservation failed for %q", ref)
		}
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "error", Payload: json.RawMessage(`{"message":"unrelated failure"}`)})
	if got := len(p.images.expected); got != 2 {
		t.Fatalf("uncorrelated error left %d reservations, want 2", got)
	}

	p.processEnvelope(&HeadlessEnvelope{Type: "error", Payload: json.RawMessage(`{"request_id":"artifact:images/first.png","message":"artifact unavailable"}`)})
	if got := len(p.images.expected); got != 1 {
		t.Fatalf("correlated error left %d reservations, want 1", got)
	}
	if !p.images.expectRefetch("session-1", "artifact:images/first.png", "retry") {
		t.Fatal("correlated error did not release its reservation")
	}
	p.images.mu.Lock()
	_, secondPending := p.images.expected["artifact:images/second.png"]
	p.images.mu.Unlock()
	if !secondPending {
		t.Fatal("unrelated reservation was unexpectedly removed")
	}
	if p.images.expectRefetch("session-1", "artifact:images/second.png", "duplicate") {
		t.Fatal("duplicate reservation was unexpectedly accepted")
	}
}

// TestImageRefetchRejectsForgedReference keeps the relaxed bootstrap honest: the
// digest still has to match what literally named it.
func TestImageRefetchRejectsForgedReference(t *testing.T) {
	data := testImageBytes(t)
	m := testImageMetadata(data)
	forged := m
	forged.SHA256 = strings.Repeat("0", hex.EncodedLen(32))
	forged.Reference = "artifact:images/sha256-" + forged.SHA256 + ".png"

	var receiver imageReceiver
	defer receiver.reset()
	// Sense check: the reference form itself must be acceptable to the bootstrap.
	if _, err := imageDigestFromReference(m.Reference, m.MIME); err != nil {
		t.Fatalf("valid reference rejected: %v", err)
	}
	// A reference whose digest is not entangled with the bytes is refused.
	if _, err := imageDigestFromReference("artifact:images/latest.png", m.MIME); err == nil {
		t.Fatal("non-artifact reference accepted")
	}
	if _, err := imageDigestFromReference(strings.TrimSuffix(m.Reference, ".png"), m.MIME); err == nil {
		t.Fatal("extension-less reference accepted")
	}
	// Extension and MIME must agree, otherwise a jpeg could arrive as .png.
	if _, err := imageDigestFromReference(m.Reference, "image/jpeg"); err == nil {
		t.Fatal("mismatched extension and MIME accepted")
	}
	if !receiver.expectRefetch("session-1", forged.Reference, "refetch-request") {
		t.Fatal("valid artifact reference was not reserved")
	}
	if image, err := receiver.chunk(refetchChunk(t, forged, 0, true, data), "session-1"); err == nil || image != nil {
		t.Fatalf("forged digest accepted: image=%v err=%v", image, err)
	}
	if len(receiver.expected) != 0 {
		t.Fatal("digest failure retained reservation")
	}
}

// TestImageRefetchRejectsUnrequested keeps a re-request out of another session's
// hands, and keeps unrequested chunks out entirely.
func TestImageRefetchRejectsUnrequested(t *testing.T) {
	data := testImageBytes(t)
	m := testImageMetadata(data)

	var receiver imageReceiver
	defer receiver.reset()

	if _, err := receiver.chunk(refetchChunk(t, m, 0, true, data), "session-1"); err == nil {
		t.Fatal("unsolicited chunk without a summary was accepted")
	}

	receiver.reset()
	receiver.expectRefetch("session-1", m.Reference, "refetch-request")
	if _, err := receiver.chunk(refetchChunk(t, m, 0, true, data), "session-2"); err == nil {
		t.Fatal("resend crossed into another session")
	}
	// A resumed delivery must start at zero or the digest check is meaningless.
	receiver.expectRefetch("session-1", m.Reference, "refetch-request")
	if _, err := receiver.chunk(refetchChunk(t, m, 5, true, data), "session-1"); err == nil {
		t.Fatal("resend starting mid-file accepted")
	}
}

// TestImageFinishNoticeNamesReferences makes the dead end actionable: a discarded
// transfer has to tell the user which reference to resend.
func TestImageFinishNoticeNamesReferences(t *testing.T) {
	data := testImageBytes(t)
	m := testImageMetadata(data)

	var receiver imageReceiver
	if err := receiver.result(testImageResult(t, "call-1", []receivedImageMetadata{m}), "session-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.chunk(testImageChunk(t, "call-1", m, 0, false, data[:10]), "session-1"); err != nil {
		t.Fatal(err)
	}
	err := receiver.finish()
	if err == nil {
		t.Fatal("expected a discard report")
	}
	if !strings.Contains(err.Error(), "/image <reference>") || !strings.Contains(err.Error(), m.Reference) {
		t.Fatalf("notice = %q, want a resend hint carrying the reference", err)
	}
	if n := receiver.finish(); n != nil {
		t.Fatalf("second finish = %v, want nil", n)
	}
	receiver.reset()
}
