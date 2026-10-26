package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

const (
	imageResultEvent     = "image_result"
	imageArtifactEvent   = "image_artifact"
	maxGatewayImageChunk = 384 << 10
	maxGatewayImageSpool = 256 << 20
	maxGatewayImageCount = 5
	imageRefetchTimeout  = 30 * time.Second
)

type receivedImageMetadata struct {
	Reference string `json:"reference"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size_bytes"`
	MIME      string `json:"mime_type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}
type imageReceiveEntry struct {
	meta   receivedImageMetadata
	file   *os.File
	digest hash.Hash
	offset int64
	// refetch marks an entry created by an explicit re-request rather than an
	// image_result summary. Chord answers a re-request with chunks only, so the
	// metadata has to come from the first chunk's own headers.
	refetch bool
}

// Image identity is scoped to the durable session and the emitting Agent.
type imageReceiveKey struct {
	session, agent, call, reference string
}

type imageRefetch struct {
	key   imageReceiveKey
	timer *time.Timer
}

type imageReceiver struct {
	mu      sync.Mutex
	entries map[imageReceiveKey]*imageReceiveEntry
	// Completed and failed identities share a bounded terminal cache. Failed
	// transfers consume the remaining chunks without reporting every chunk.
	// expected holds references the gateway actively asked Chord to resend.
	// Indexed by reference alone: Chord answers a re-request with an empty call
	// and agent id, so the identity tuple that normal delivery uses is not
	// available to match on.
	expected  map[string]imageRefetch
	seen      map[imageReceiveKey]bool
	seenOrder []imageReceiveKey
}

// expectRefetch records that the gateway asked Chord to resend reference, so the
// arriving chunks can bootstrap their own metadata instead of being rejected for
// having no preceding image_result. Returns false when the reference is already
// fully known, which would mean a duplicate re-request.
func (r *imageReceiver) expectRefetch(session, reference, requestID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, e := range r.entries {
		if key.session == session && key.reference == reference && e.refetch {
			return false
		}
	}
	if r.seen[imageReceiveKey{session: session, reference: reference}] || len(r.expected) >= 32 {
		return false
	}
	if r.expected == nil {
		r.expected = make(map[string]imageRefetch)
	}
	if _, ok := r.expected[reference]; ok {
		return false
	}
	key := imageReceiveKey{session: session, call: requestID, reference: reference}
	timer := time.AfterFunc(imageRefetchTimeout, func() {
		r.cancelRefetch(requestID)
	})
	r.expected[reference] = imageRefetch{key: key, timer: timer}
	return true
}

// cancelRefetch leaves ordinary deliveries alone, even for the same reference.
// An empty request ID is used for an uncorrelated headless error.
func (r *imageReceiver) cancelRefetch(requestID string) {
	if requestID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for reference, pending := range r.expected {
		if pending.key.call != requestID {
			continue
		}
		pending.timer.Stop()
		if entry := r.entries[pending.key]; entry != nil {
			r.discard(entry)
			delete(r.entries, pending.key)
		}
		r.rememberTerminal(pending.key)
		delete(r.expected, reference)
	}
}

// rememberTerminal requires r.mu and stores no image bytes or open files.
func (r *imageReceiver) rememberTerminal(key imageReceiveKey) {
	if r.seen == nil {
		r.seen = make(map[imageReceiveKey]bool)
	}
	if r.seen[key] {
		return
	}
	if len(r.seen) >= 1024 {
		delete(r.seen, r.seenOrder[0])
		r.seenOrder = r.seenOrder[1:]
	}
	r.seen[key] = true
	r.seenOrder = append(r.seenOrder, key)
}

func (r *imageReceiver) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		r.discard(e)
	}
	for _, pending := range r.expected {
		pending.timer.Stop()
	}
	r.entries = nil
	r.expected = nil
	r.seen = nil
	r.seenOrder = nil
}
func (*imageReceiver) discard(e *imageReceiveEntry) {
	if e.file != nil {
		_ = e.file.Close()
		_ = os.Remove(e.file.Name())
		im.ImageSpoolBytes.Add(-e.meta.Size)
		e.file = nil
	}
}

// finish discards only unfinished assemblies; delivered images belong to the
// outbound queue and completed identities remain deduplicated.
func (r *imageReceiver) finish() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := len(r.entries)
	// Collect before the loop clears the map, or the notice loses the very
	// references it exists to hand out.
	refs := make([]string, 0, count)
	for _, e := range r.entries {
		if e.meta.Reference != "" {
			refs = append(refs, e.meta.Reference)
		}
	}
	for key, e := range r.entries {
		r.discard(e)
		delete(r.entries, key)
		r.rememberTerminal(key)
	}
	for reference, pending := range r.expected {
		pending.timer.Stop()
		if !slices.Contains(refs, reference) {
			refs = append(refs, reference)
			count++
		}
		r.rememberTerminal(pending.key)
		delete(r.expected, reference)
	}
	if count != 0 {
		// Name the recoverable references: each one can be re-requested with
		// /image <reference>, which is the only way back to a lost transfer.
		sort.Strings(refs)
		if len(refs) > 3 {
			refs = append(refs[:3:3], fmt.Sprintf("…and %d more", len(refs)-3))
		}
		return fmt.Errorf("%d incomplete image transfer(s) discarded; re-request an original with /image <reference>: %s", count, strings.Join(refs, ", "))
	}
	return nil
}

func (r *imageReceiver) result(raw json.RawMessage, session string) error {
	var payload struct {
		CallID        string `json:"call_id"`
		AgentID       string `json:"agent_id"`
		Status        string `json:"status"`
		Result        string `json:"result"`
		DeliveryError string `json:"delivery_error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode image result: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	callKey := imageReceiveKey{session, payload.AgentID, payload.CallID, ""}
	if r.seen[callKey] {
		return nil
	}
	failCall := func(err error) error {
		for key, e := range r.entries {
			if key.session == session && key.agent == payload.AgentID && key.call == payload.CallID {
				r.discard(e)
				delete(r.entries, key)
			}
		}
		r.rememberTerminal(callKey)
		return err
	}
	if payload.DeliveryError != "" || payload.Status != "success" {
		if payload.DeliveryError != "" {
			return failCall(fmt.Errorf("headless image delivery failed: %s", payload.DeliveryError))
		}
		return failCall(nil)
	}
	var summary struct {
		Images []receivedImageMetadata `json:"images"`
	}
	if err := json.Unmarshal([]byte(payload.Result), &summary); err != nil {
		return failCall(fmt.Errorf("decode image summary: %w", err))
	}
	if session == "" || payload.CallID == "" || len(summary.Images) == 0 || len(summary.Images) > maxGatewayImageCount {
		return failCall(fmt.Errorf("invalid image result identity or count"))
	}
	if r.entries == nil {
		r.entries = make(map[imageReceiveKey]*imageReceiveEntry)
	}
	newKeys := make(map[imageReceiveKey]bool)
	for _, m := range summary.Images {
		digest, err := hex.DecodeString(m.SHA256)
		if err != nil || len(digest) != sha256.Size || m.Size <= 0 || m.Size > im.MaxGatewayImageBytes || m.Width <= 0 || m.Height <= 0 || m.Width > im.MaxGatewayImageDimension || m.Height > im.MaxGatewayImageDimension || int64(m.Width)*int64(m.Height) > 36_000_000 || (m.MIME != "image/png" && m.MIME != "image/jpeg" && m.MIME != "image/webp") {
			return failCall(fmt.Errorf("invalid image metadata"))
		}
		if !strings.HasPrefix(m.Reference, "artifact:images/sha256-"+m.SHA256+".") {
			return failCall(fmt.Errorf("image reference does not match digest"))
		}
		key := imageReceiveKey{session, payload.AgentID, payload.CallID, m.Reference}
		if existing := r.entries[key]; existing != nil && existing.meta != m {
			return failCall(fmt.Errorf("repeated image result changed metadata"))
		}
		if !r.seen[key] && r.entries[key] == nil {
			newKeys[key] = true
		}
	}
	if len(r.entries)+len(newKeys) > 32 {
		return failCall(fmt.Errorf("too many incomplete images"))
	}
	for _, m := range summary.Images {
		key := imageReceiveKey{session, payload.AgentID, payload.CallID, m.Reference}
		if newKeys[key] {
			r.entries[key] = &imageReceiveEntry{meta: m}
		}
	}
	return nil
}

// imageDigestFromReference recovers the expected SHA-256 from the reference Chord
// issues. Reference form is artifact:images/sha256-<hex>.<ext>, and the digest is
// what makes a re-requested image verifiable without a signed summary.
func imageDigestFromReference(reference, mime string) (string, error) {
	prefix := "artifact:images/sha256-"
	rest, ok := strings.CutPrefix(reference, prefix)
	if !ok {
		return "", fmt.Errorf("image reference is not an artifact reference")
	}
	digest, ext, found := cutLast(rest, ".")
	if !found {
		return "", fmt.Errorf("image reference has no extension")
	}
	if len(digest) != hex.EncodedLen(sha256.Size) || !isHex(digest) {
		return "", fmt.Errorf("image reference digest is malformed")
	}
	want := map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true}[mime]
	switch {
	case !want:
		return "", fmt.Errorf("unsupported image type")
	case ext == "png" && mime != "image/png", ext == "jpg" && mime != "image/jpeg", ext == "webp" && mime != "image/webp":
		return "", fmt.Errorf("image extension does not match its type")
	}
	return digest, nil
}

func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validateImageMetadata applies the same limits the normal image_result path
// enforces, so a re-request cannot smuggle in something with no summary.
func validateImageMetadata(m receivedImageMetadata) error {
	if m.Size <= 0 || m.Size > im.MaxGatewayImageBytes || m.Width <= 0 || m.Height <= 0 || m.Width > im.MaxGatewayImageDimension || m.Height > im.MaxGatewayImageDimension || int64(m.Width)*int64(m.Height) > 36_000_000 || (m.MIME != "image/png" && m.MIME != "image/jpeg" && m.MIME != "image/webp") {
		return fmt.Errorf("invalid image metadata")
	}
	if !strings.HasPrefix(m.Reference, "artifact:images/sha256-"+m.SHA256+".") {
		return fmt.Errorf("image reference does not match digest")
	}
	return nil
}

func (r *imageReceiver) chunk(raw json.RawMessage, session string) (*im.ReceivedImage, error) {
	// Chunks carry no digest of their own; integrity is checked against the
	// summary's SHA-256 once the final chunk arrives.
	var p struct {
		Reference string `json:"reference"`
		Size      int64  `json:"size_bytes"`
		MIME      string `json:"mime_type"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		CallID    string `json:"call_id"`
		RequestID string `json:"request_id"`
		AgentID   string `json:"agent_id"`
		Offset    int64  `json:"offset"`
		Last      bool   `json:"last"`
		Data      string `json:"data"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode image chunk: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := imageReceiveKey{session, p.AgentID, p.CallID, p.Reference}
	if p.RequestID != "" && p.CallID == "" && p.AgentID == "" {
		key.call = p.RequestID
	}
	if r.seen[key] || r.seen[imageReceiveKey{session, p.AgentID, p.CallID, ""}] {
		return nil, nil
	}
	e := r.entries[key]
	if e == nil {
		// A re-requested reference arrives without a preceding image_result:
		// Chord replays the chunks directly. Adopt this chunk's own headers as
		// the metadata, then let the normal checks validate everything after it.
		expected, ok := r.expected[p.Reference]
		if !ok || expected.key != key || p.CallID != "" || p.AgentID != "" {
			r.rememberTerminal(key)
			return nil, fmt.Errorf("image chunk has no matching result")
		}
		bootstrapFailed := func(err error) (*im.ReceivedImage, error) {
			expected.timer.Stop()
			delete(r.expected, p.Reference)
			r.rememberTerminal(key)
			return nil, err
		}
		if p.Offset != 0 {
			return bootstrapFailed(fmt.Errorf("resumed image does not start at offset 0"))
		}
		digest, err := imageDigestFromReference(p.Reference, p.MIME)
		if err != nil {
			return bootstrapFailed(err)
		}
		meta := receivedImageMetadata{Reference: p.Reference, SHA256: digest, Size: p.Size, MIME: p.MIME, Width: p.Width, Height: p.Height}
		if err := validateImageMetadata(meta); err != nil {
			return bootstrapFailed(err)
		}
		if len(r.entries) >= 32 {
			return bootstrapFailed(fmt.Errorf("too many incomplete images"))
		}
		if r.entries == nil {
			r.entries = make(map[imageReceiveKey]*imageReceiveEntry)
		}
		e = &imageReceiveEntry{meta: meta, refetch: true}
		r.entries[key] = e
	}
	releaseRefetch := func() {
		if e.refetch {
			if pending, ok := r.expected[p.Reference]; ok && pending.key == key {
				pending.timer.Stop()
				delete(r.expected, p.Reference)
			}
		}
	}
	fail := func(err error) (*im.ReceivedImage, error) {
		releaseRefetch()
		r.discard(e)
		delete(r.entries, key)
		r.rememberTerminal(key)
		return nil, err
	}
	if p.Offset != e.offset || p.Size != e.meta.Size || p.MIME != e.meta.MIME || p.Width != e.meta.Width || p.Height != e.meta.Height || len(p.Data) > base64.StdEncoding.EncodedLen(maxGatewayImageChunk) {
		return fail(fmt.Errorf("image chunk offset or metadata mismatch"))
	}
	data, err := base64.StdEncoding.Strict().DecodeString(p.Data)
	if err != nil || len(data) == 0 || e.offset+int64(len(data)) > e.meta.Size {
		return fail(fmt.Errorf("invalid image chunk data"))
	}
	if e.file == nil {
		for {
			used := im.ImageSpoolBytes.Load()
			if used+e.meta.Size > maxGatewayImageSpool {
				return fail(fmt.Errorf("image delivery spool is full"))
			}
			if im.ImageSpoolBytes.CompareAndSwap(used, used+e.meta.Size) {
				break
			}
		}
		file, err := os.CreateTemp("", "chord-gateway-image-*")
		if err != nil {
			im.ImageSpoolBytes.Add(-e.meta.Size)
			return fail(fmt.Errorf("create image temporary file: %w", err))
		}
		e.file = file
		e.digest = sha256.New()
	}
	n, err := e.file.Write(data)
	if err != nil || n != len(data) {
		return fail(fmt.Errorf("write image temporary file failed"))
	}
	_, _ = e.digest.Write(data)
	e.offset += int64(len(data))
	if !p.Last {
		if e.offset == e.meta.Size {
			return fail(fmt.Errorf("image final marker is missing"))
		}
		return nil, nil
	}
	if e.offset != e.meta.Size || hex.EncodeToString(e.digest.Sum(nil)) != e.meta.SHA256 {
		return fail(fmt.Errorf("image size or digest mismatch"))
	}
	if err := e.file.Close(); err != nil {
		return fail(fmt.Errorf("close received image: %w", err))
	}
	image := &im.ReceivedImage{Path: e.file.Name(), Reference: e.meta.Reference, CallID: p.CallID, AgentID: p.AgentID, SessionID: session, MIME: e.meta.MIME, SHA256: e.meta.SHA256, Size: e.meta.Size, Width: e.meta.Width, Height: e.meta.Height}
	e.file = nil
	delete(r.entries, key)
	releaseRefetch()
	r.rememberTerminal(key)
	if e.refetch {
		r.rememberTerminal(imageReceiveKey{session: session, reference: p.Reference})
	}
	return image, nil
}

func (p *ChordProcess) processImageEnvelope(env *HeadlessEnvelope) {
	p.mu.RLock()
	session := p.state.SessionID
	p.mu.RUnlock()
	var image *im.ReceivedImage
	var err error
	if env.Type == imageResultEvent {
		err = p.images.result(env.Payload, session)
	} else {
		image, err = p.images.chunk(env.Payload, session)
	}
	if err != nil {
		p.reportTransportError(err, false)
		return
	}
	if image == nil {
		return
	}
	if p.mgr == nil {
		image.Close()
		return
	}
	p.mgr.mu.RLock()
	callback := p.mgr.onImage
	p.mgr.mu.RUnlock()
	if callback == nil {
		image.Close()
		return
	}
	image.BindingInstanceID = p.bindingInstanceID
	callback(p.key, image)
}
