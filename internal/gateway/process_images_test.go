package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

func testImageBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func testImageMetadata(data []byte) receivedImageMetadata {
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	return receivedImageMetadata{Reference: "artifact:images/sha256-" + hash + ".png", SHA256: hash, Size: int64(len(data)), MIME: "image/png", Width: 2, Height: 2}
}
func testImageResult(t *testing.T, call string, meta []receivedImageMetadata) json.RawMessage {
	t.Helper()
	summary, _ := json.Marshal(map[string]any{"images": meta})
	raw, _ := json.Marshal(map[string]any{"call_id": call, "status": "success", "result": string(summary)})
	return raw
}
func testImageChunk(t *testing.T, call string, m receivedImageMetadata, offset int, last bool, data []byte) json.RawMessage {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"call_id": call, "reference": m.Reference, "mime_type": m.MIME, "width": m.Width, "height": m.Height, "size_bytes": m.Size, "offset": offset, "last": last, "data": base64.StdEncoding.EncodeToString(data)})
	return raw
}

func TestImageReceiverIntegrityAndDuplicate(t *testing.T) {
	data := testImageBytes(t)
	m := testImageMetadata(data)
	var receiver imageReceiver
	defer receiver.reset()
	initial := im.ImageSpoolBytes.Load()
	if err := receiver.result(testImageResult(t, "call-1", []receivedImageMetadata{m}), "session-1"); err != nil {
		t.Fatal(err)
	}
	first, err := receiver.chunk(testImageChunk(t, "call-1", m, 0, false, data[:10]), "session-1")
	if err != nil || first != nil {
		t.Fatal(err)
	}
	image, err := receiver.chunk(testImageChunk(t, "call-1", m, 10, true, data[10:]), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if image.SessionID != "session-1" || image.SHA256 != m.SHA256 {
		t.Fatal("source binding lost")
	}
	if _, err := im.ReadReceivedImage(image); err != nil {
		t.Fatal(err)
	}
	if err := receiver.result(testImageResult(t, "call-1", []receivedImageMetadata{m}), "session-1"); err != nil {
		t.Fatal(err)
	}
	if again, err := receiver.chunk(testImageChunk(t, "call-1", m, 0, true, data), "session-1"); err != nil || again != nil {
		t.Fatal("duplicate image delivered")
	}
	image.Close()
	image.Close()
	if _, err := os.Stat(image.Path); !os.IsNotExist(err) {
		t.Fatal("temporary original retained")
	}
	if im.ImageSpoolBytes.Load() != initial {
		t.Fatal("image budget leaked")
	}
}

func TestImageReceiverRejectsCorruptionAndCleansPartial(t *testing.T) {
	for _, kind := range []string{"offset", "hash", "size", "mime", "missing_last", "truncated", "base64"} {
		t.Run(kind, func(t *testing.T) {
			data := testImageBytes(t)
			m := testImageMetadata(data)
			var receiver imageReceiver
			defer receiver.reset()
			initial := im.ImageSpoolBytes.Load()
			if err := receiver.result(testImageResult(t, "call", []receivedImageMetadata{m}), "session"); err != nil {
				t.Fatal(err)
			}
			offset := 0
			last := true
			chunk := append([]byte(nil), data...)
			switch kind {
			case "offset":
				offset = 1
			case "hash":
				chunk[0] ^= 1
			case "size":
				m.Size++
			case "mime":
				m.MIME = "image/jpeg"
			case "missing_last":
				last = false
			case "truncated":
				chunk = chunk[:10]
			}
			raw := testImageChunk(t, "call", m, offset, last, chunk)
			if kind == "base64" {
				var value map[string]any
				_ = json.Unmarshal(raw, &value)
				value["data"] = "!"
				raw, _ = json.Marshal(value)
			}
			got, err := receiver.chunk(raw, "session")
			if err == nil || got != nil {
				t.Fatal("invalid image accepted")
			}
			if im.ImageSpoolBytes.Load() != initial {
				t.Fatal("failed receive leaked budget")
			}
		})
	}
	var receiver imageReceiver
	data := testImageBytes(t)
	m := testImageMetadata(data)
	initial := im.ImageSpoolBytes.Load()
	_ = receiver.result(testImageResult(t, "call", []receivedImageMetadata{m}), "session")
	_, _ = receiver.chunk(testImageChunk(t, "call", m, 0, false, data[:10]), "session")
	if im.ImageSpoolBytes.Load() != initial+m.Size {
		t.Fatal("partial not reserved")
	}
	receiver.reset()
	if im.ImageSpoolBytes.Load() != initial {
		t.Fatal("partial not released")
	}
	if _, err := receiver.chunk(testImageChunk(t, "call", m, 10, true, data[10:]), "session"); err == nil {
		t.Fatal("old session receive continued")
	}
}

func TestImageReceiverMetadataAndBudgetLimits(t *testing.T) {
	data := testImageBytes(t)
	base := testImageMetadata(data)
	for _, kind := range []string{"count", "size", "hash", "pixels", "reference", "session"} {
		t.Run(kind, func(t *testing.T) {
			m := base
			metas := []receivedImageMetadata{m}
			session := "session"
			switch kind {
			case "count":
				metas = make([]receivedImageMetadata, 6)
			case "size":
				metas[0].Size = im.MaxGatewayImageBytes + 1
			case "hash":
				metas[0].SHA256 = "invalid"
			case "pixels":
				metas[0].Width = 36_000_001
			case "reference":
				metas[0].Reference = "artifact:../private.png"
			case "session":
				session = ""
			}
			var receiver imageReceiver
			defer receiver.reset()
			if receiver.result(testImageResult(t, "call", metas), session) == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
	var receiver imageReceiver
	defer receiver.reset()
	_ = receiver.result(testImageResult(t, "call", []receivedImageMetadata{base}), "session")
	old := im.ImageSpoolBytes.Swap(maxGatewayImageSpool)
	defer im.ImageSpoolBytes.Store(old)
	if _, err := receiver.chunk(testImageChunk(t, "call", base, 0, true, data), "session"); err == nil {
		t.Fatal("spool limit ignored")
	}
}

func imagePayloadAgent(t *testing.T, raw json.RawMessage, agent string) json.RawMessage {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	payload["agent_id"] = agent
	result, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestImageReceiverScopesIdentityAndFailure(t *testing.T) {
	for _, sameImage := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_image=%t", sameImage), func(t *testing.T) {
			data := testImageBytes(t)
			other := append(append([]byte(nil), data...), byte(0))
			if sameImage {
				other = data
			}
			a, b := testImageMetadata(data), testImageMetadata(other)
			var receiver imageReceiver
			defer receiver.reset()
			initial := im.ImageSpoolBytes.Load()
			for agent, meta := range map[string]receivedImageMetadata{"agent-a": a, "agent-b": b} {
				if err := receiver.result(imagePayloadAgent(t, testImageResult(t, "call-1", []receivedImageMetadata{meta}), agent), "session-1"); err != nil {
					t.Fatal(err)
				}
			}
			if len(receiver.entries) != 2 {
				t.Fatal("Agent image identities collapsed")
			}
			for agent, meta := range map[string]receivedImageMetadata{"agent-a": a, "agent-b": b} {
				if _, err := receiver.chunk(imagePayloadAgent(t, testImageChunk(t, "call-1", meta, 0, false, data[:10]), agent), "session-1"); err != nil {
					t.Fatal(err)
				}
			}
			// A wrong Agent or session cannot mutate the real assembly.
			raw := imagePayloadAgent(t, testImageChunk(t, "call-1", b, 10, true, other[10:]), "agent-c")
			if _, err := receiver.chunk(raw, "session-1"); err == nil {
				t.Fatal("wrong Agent accepted")
			}
			raw = imagePayloadAgent(t, testImageChunk(t, "call-1", b, 10, true, other[10:]), "agent-b")
			if _, err := receiver.chunk(raw, "session-2"); err == nil {
				t.Fatal("wrong session accepted")
			}
			if err := receiver.result(json.RawMessage(`{"call_id":"call-1","agent_id":"agent-a","status":"error"}`), "session-1"); err != nil {
				t.Fatal(err)
			}
			if len(receiver.entries) != 1 || im.ImageSpoolBytes.Load() != initial+b.Size {
				t.Fatal("failure removed sibling image")
			}
			img, err := receiver.chunk(raw, "session-1")
			if err != nil || img == nil {
				t.Fatalf("sibling receive: %v", err)
			}
			defer img.Close()
			if img.AgentID != "agent-b" || img.SessionID != "session-1" {
				t.Fatal("source identity lost")
			}
		})
	}
}

func TestImageReceiverDedupIncludesAgentAndSession(t *testing.T) {
	data := testImageBytes(t)
	meta := testImageMetadata(data)
	var receiver imageReceiver
	defer receiver.reset()
	for _, id := range []struct{ session, agent string }{{"session-1", "agent-a"}, {"session-1", "agent-b"}, {"session-2", "agent-a"}} {
		result := imagePayloadAgent(t, testImageResult(t, "call-1", []receivedImageMetadata{meta}), id.agent)
		raw := imagePayloadAgent(t, testImageChunk(t, "call-1", meta, 0, true, data), id.agent)
		if err := receiver.result(result, id.session); err != nil {
			t.Fatal(err)
		}
		img, err := receiver.chunk(raw, id.session)
		if err != nil || img == nil {
			t.Fatalf("identity %v suppressed: %v", id, err)
		}
		img.Close()
		if err := receiver.result(result, id.session); err != nil {
			t.Fatal(err)
		}
		if duplicate, err := receiver.chunk(raw, id.session); err != nil || duplicate != nil {
			t.Fatal("duplicate delivered")
		}
	}
}

func TestImageTransfersFinishOnAggregateIdle(t *testing.T) {
	data := testImageBytes(t)
	meta := testImageMetadata(data)
	initial := im.ImageSpoolBytes.Load()
	var events []string
	p := &ChordProcess{state: ControlState{SessionID: "session-1", Busy: true}, onEvent: func(_ string, event string, _ ControlState) { events = append(events, event) }}
	defer p.images.reset()
	// Include a summary without chunks, a partial file, and a completed file
	// already owned by outbound. Idle must release only the first two.
	for _, call := range []string{"missing", "partial", "complete"} {
		p.processEnvelope(&HeadlessEnvelope{Type: imageResultEvent, Payload: testImageResult(t, call, []receivedImageMetadata{meta})})
	}
	if _, err := p.images.chunk(testImageChunk(t, "partial", meta, 0, false, data[:10]), "session-1"); err != nil {
		t.Fatal(err)
	}
	path := p.images.entries[imageReceiveKey{"session-1", "", "partial", meta.Reference}].file.Name()
	complete, err := p.images.chunk(testImageChunk(t, "complete", meta, 0, true, data), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer complete.Close()
	p.processEnvelope(&HeadlessEnvelope{Type: "idle", Payload: json.RawMessage(`{}`)})
	if len(p.images.entries) != 0 || im.ImageSpoolBytes.Load() != initial+meta.Size {
		t.Fatal("idle leaked incomplete reservations or took outbound ownership")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("partial file retained")
	}
	if _, err := im.ReadReceivedImage(complete); err != nil {
		t.Fatal("idle deleted outbound image", err)
	}
	if p.State().Busy || !slices.Equal(events, []string{"transport_error", "idle"}) {
		t.Fatalf("idle notifications: %v", events)
	}
	if p.State().LastTransportError == nil || !strings.Contains(p.State().LastTransportError.Message, "2 incomplete") {
		t.Fatal("discard not reported")
	}
	p.processEnvelope(&HeadlessEnvelope{Type: "idle", Payload: json.RawMessage(`{}`)})
	if !slices.Equal(events, []string{"transport_error", "idle", "idle"}) {
		t.Fatal("repeated discard warning")
	}
	if duplicate, err := p.images.chunk(testImageChunk(t, "complete", meta, 0, true, data), "session-1"); err != nil || duplicate != nil {
		t.Fatal("idle lost dedup state")
	}
}

func TestImageTransfersResetOnSessionSwitch(t *testing.T) {
	data := testImageBytes(t)
	meta := testImageMetadata(data)
	initial := im.ImageSpoolBytes.Load()
	p := &ChordProcess{state: ControlState{SessionID: "session-1"}}
	defer p.images.reset()
	p.processEnvelope(&HeadlessEnvelope{Type: imageResultEvent, Payload: testImageResult(t, "call-1", []receivedImageMetadata{meta})})
	if _, err := p.images.chunk(testImageChunk(t, "call-1", meta, 0, false, data[:10]), "session-1"); err != nil {
		t.Fatal(err)
	}
	path := p.images.entries[imageReceiveKey{"session-1", "", "call-1", meta.Reference}].file.Name()
	p.processEnvelope(&HeadlessEnvelope{Type: "session_switched", Payload: json.RawMessage(`{"session_id":"session-2"}`)})
	if len(p.images.entries) != 0 || im.ImageSpoolBytes.Load() != initial {
		t.Fatal("session switch retained incomplete transfers")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("session switch retained the partial file")
	}
	if image, err := p.images.chunk(testImageChunk(t, "call-1", meta, 10, true, data[10:]), "session-2"); err == nil || image != nil {
		t.Fatal("late chunk from the abandoned session was accepted")
	}
}

func TestImageReceiveFailureReportsOnceAndPreservesSibling(t *testing.T) {
	for _, kind := range []string{"offset", "spool", "invalid_summary", "orphan"} {
		t.Run(kind, func(t *testing.T) {
			data := testImageBytes(t)
			meta := testImageMetadata(data)
			initial := im.ImageSpoolBytes.Load()
			warnings := 0
			mgr := &ChordManager{}
			var delivered []*im.ReceivedImage
			mgr.SetOnImage(func(_ string, img *im.ReceivedImage) { delivered = append(delivered, img) })
			p := &ChordProcess{mgr: mgr, state: ControlState{SessionID: "session"}, onEvent: func(_ string, event string, _ ControlState) {
				if event == "transport_error" {
					warnings++
				}
			}}
			defer p.images.reset()
			if kind != "orphan" {
				resultMeta := meta
				if kind == "invalid_summary" {
					resultMeta.Width = im.MaxGatewayImageDimension + 1
				}
				p.processEnvelope(&HeadlessEnvelope{Type: imageResultEvent, Payload: testImageResult(t, "failed", []receivedImageMetadata{resultMeta})})
			}
			if kind == "spool" {
				im.ImageSpoolBytes.Store(maxGatewayImageSpool)
				defer im.ImageSpoolBytes.Store(initial)
			}
			for offset := range 8 {
				chunkOffset := offset + 1
				if kind == "spool" && offset == 0 {
					chunkOffset = 0
				}
				p.processEnvelope(&HeadlessEnvelope{Type: imageArtifactEvent, Payload: testImageChunk(t, "failed", meta, chunkOffset, offset == 7, data[:1])})
			}
			if kind == "spool" {
				if im.ImageSpoolBytes.Load() != maxGatewayImageSpool {
					t.Fatal("failed receive changed existing reservations")
				}
				im.ImageSpoolBytes.Store(initial)
			}
			if warnings != 1 {
				t.Fatalf("failure warnings = %d, want 1", warnings)
			}
			// A repeated result cannot reopen a failed transfer.
			p.processEnvelope(&HeadlessEnvelope{Type: imageResultEvent, Payload: testImageResult(t, "failed", []receivedImageMetadata{meta})})
			p.processEnvelope(&HeadlessEnvelope{Type: imageArtifactEvent, Payload: testImageChunk(t, "failed", meta, 0, true, data)})
			p.processEnvelope(&HeadlessEnvelope{Type: imageResultEvent, Payload: testImageResult(t, "sibling", []receivedImageMetadata{meta})})
			p.processEnvelope(&HeadlessEnvelope{Type: imageArtifactEvent, Payload: testImageChunk(t, "sibling", meta, 0, true, data)})
			if warnings != 1 || len(delivered) != 1 || delivered[0].CallID != "sibling" {
				t.Fatalf("warnings=%d delivered=%v", warnings, delivered)
			}
			delivered[0].Close()
			if im.ImageSpoolBytes.Load() != initial {
				t.Fatal("failed transfer leaked spool capacity")
			}
		})
	}
}

func TestImageReceiverTerminalCacheIsBounded(t *testing.T) {
	var receiver imageReceiver
	for n := range 1200 {
		receiver.rememberTerminal(imageReceiveKey{"session", "agent", fmt.Sprint(n), "reference"})
	}
	if len(receiver.seen) != 1024 || len(receiver.seenOrder) != 1024 || receiver.seen[imageReceiveKey{"session", "agent", "0", "reference"}] {
		t.Fatal("terminal cache is not bounded")
	}
	receiver.reset()
	if len(receiver.seen) != 0 || len(receiver.seenOrder) != 0 {
		t.Fatal("session reset retained terminal identities")
	}
}
