package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

var errIngressMissing = errors.New("ingress record or route not found")
var errIngressBlocked = errors.New("ingress route has undelivered or uncertain input")
var errIngressClosed = errors.New("ingress is closed")
var errIngressCapacity = errors.New("ingress capacity reached")
var errIngressTransportChanged = errors.New("transport identity changed before admission")

// ingressAcceptBudget bounds how long a dispatch waits for an acceptance before
// reporting an uncertain outcome. It is variable so the acceptance-budget test
// does not wait out the full window; production keeps two seconds.
var ingressAcceptBudget = 2 * time.Second

const ingressHistoryLimit = 100000
const ingressHistoryTTL = 24 * time.Hour

// IngressStore is process-local coordination, not a recoverable message broker.
// One short critical section publishes an acceptance and its polling progress.
// No network, filesystem I/O, or Chord wait may run while mu is held.
// Only bounded pending records are scanned by the scheduler; terminal history
// expires on a bounded TTL as new acceptances are pruned, and is never replayed
// after a restart.
type IngressStore struct {
	memoryBytes        int
	mu                 sync.Mutex
	closing            atomic.Bool
	metrics            ingressMetrics
	changed            chan struct{}
	now                func() time.Time
	records            map[string]IngressRecord
	pending            map[string]struct{}
	pendingByRoute     map[ChatIdentity]map[string]struct{}
	routes             map[ChatIdentity]IngressRoute
	byRoute            map[ChatIdentity]map[string]struct{}
	business, semantic map[string]string
	cursors            map[string]IngressCursor
	history            []ingressHistoryEntry
	historyHead        int
	counts             map[string]ingressCounts
	total              ingressCounts
}
type ingressHistoryEntry struct {
	id      string
	expires time.Time
}

type ingressCounts struct{ Pending, Bytes, Tombstones int }

func NewIngressStore() *IngressStore {
	return &IngressStore{changed: make(chan struct{}, 1), now: time.Now,
		records: make(map[string]IngressRecord), pending: make(map[string]struct{}),
		pendingByRoute: make(map[ChatIdentity]map[string]struct{}),
		routes:         make(map[ChatIdentity]IngressRoute), byRoute: make(map[ChatIdentity]map[string]struct{}),
		business: make(map[string]string), semantic: make(map[string]string),
		cursors: make(map[string]IngressCursor), counts: make(map[string]ingressCounts)}
}
func newIngressID() string                       { return rand.Text() }
func (s *IngressStore) Changed() <-chan struct{} { return s.changed }
func (s *IngressStore) signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}
func (s *IngressStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing.Store(true)
	clear(s.records)
	clear(s.pending)
	clear(s.pendingByRoute)
	clear(s.routes)
	clear(s.byRoute)
	clear(s.business)
	clear(s.semantic)
	clear(s.cursors)
	clear(s.counts)
	s.history = nil
	s.historyHead = 0
	s.total = ingressCounts{}
	s.memoryBytes = 0
	return nil
}
func (s *IngressStore) lock(ctx context.Context) error {
	if err := lockIngressMutex(ctx, &s.mu); err != nil {
		return err
	}
	if s.closing.Load() {
		s.mu.Unlock()
		return errIngressClosed
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	return nil
}
func ingressKey(prefix string, parts ...string) []byte {
	b, _ := json.Marshal(parts)
	return append([]byte(prefix), b...)
}
func recordSize(r IngressRecord) int { b, _ := json.Marshal(r); return len(b) }
func cloneIngressRecord(r IngressRecord) IngressRecord {
	if r.ControlTarget != nil {
		target := *r.ControlTarget
		r.ControlTarget = &target
	}
	return r
}
func (s *IngressStore) adjustCounts(r IngressRecord, delta int) {
	size := recordSize(r)
	s.memoryBytes += delta * size
	key := compositeKey(r.Platform, r.AccountID)
	c := s.counts[key]
	if r.Terminal() {
		c.Tombstones += delta
		s.total.Tombstones += delta
	} else {
		c.Pending += delta
		s.total.Pending += delta
		c.Bytes += delta * size
		s.total.Bytes += delta * size
	}
	if c == (ingressCounts{}) {
		delete(s.counts, key)
	} else {
		s.counts[key] = c
	}
}
func (s *IngressStore) save(r IngressRecord) {
	old, exists := s.records[r.RecordID]
	if exists {
		s.adjustCounts(old, -1)
		if !old.Terminal() && r.Terminal() {
			pending := s.pendingByRoute[old.Identity()]
			delete(pending, r.RecordID)
			if len(pending) == 0 {
				delete(s.pendingByRoute, old.Identity())
			}
		}
	}
	if r.Terminal() {
		r.Text = ""
		delete(s.pending, r.RecordID)
		if !exists || !old.Terminal() {
			s.history = append(s.history, ingressHistoryEntry{r.RecordID, s.now().Add(ingressHistoryTTL)})
		}
	} else {
		s.pending[r.RecordID] = struct{}{}
		if s.pendingByRoute[r.Identity()] == nil {
			s.pendingByRoute[r.Identity()] = make(map[string]struct{})
		}
		s.pendingByRoute[r.Identity()][r.RecordID] = struct{}{}
	}
	s.adjustCounts(r, 1)
	s.records[r.RecordID] = cloneIngressRecord(r)
	if s.byRoute[r.Identity()] == nil {
		s.byRoute[r.Identity()] = make(map[string]struct{})
	}
	s.byRoute[r.Identity()][r.RecordID] = struct{}{}
}
func (s *IngressStore) pruneHistory(preserve ChatIdentity) {
	// Terminal entries are appended in completion order. Evict only expired
	// entries: reaching the bound must never silently forget live deduplication.
	for s.historyHead < len(s.history) {
		entry := s.history[s.historyHead]
		id := entry.id
		r, ok := s.records[id]
		if ok && s.now().Before(entry.expires) {
			break
		}
		s.historyHead++
		if !ok {
			continue
		}
		s.adjustCounts(r, -1)
		delete(s.records, id)
		business := compositeKey(r.Platform, r.AccountID, r.BusinessID)
		semantic := compositeKey(r.Platform, r.AccountID, r.SemanticID)
		if s.business[business] == id {
			delete(s.business, business)
		}
		if s.semantic[semantic] == id {
			delete(s.semantic, semantic)
		}
		route := s.byRoute[r.Identity()]
		delete(route, id)
		if len(route) == 0 {
			delete(s.byRoute, r.Identity())
			if r.Identity() != preserve {
				delete(s.routes, r.Identity())
			}
		}
	}
	if s.historyHead > 1024 && s.historyHead*2 >= len(s.history) {
		s.history = slices.Clone(s.history[s.historyHead:])
		s.historyHead = 0
	}
}
func (s *IngressStore) Get(id string) (IngressRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return r, errIngressMissing
	}
	return cloneIngressRecord(r), nil
}
func (s *IngressStore) list(id *ChatIdentity, pending, metadata bool) []IngressRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []IngressRecord
	add := func(r IngressRecord) {
		if id != nil && r.Identity() != *id {
			return
		}
		if pending && r.Terminal() {
			return
		}
		r = cloneIngressRecord(r)
		if metadata {
			r.Text = ""
		}
		out = append(out, r)
	}
	if pending {
		keys := s.pending
		if id != nil {
			keys = s.pendingByRoute[*id]
		}
		for key := range keys {
			add(s.records[key])
		}
	} else if id != nil {
		for key := range s.byRoute[*id] {
			add(s.records[key])
		}
	} else {
		for _, r := range s.records {
			add(r)
		}
	}
	// Compare identity fields directly: compositeKey allocates two strings per
	// comparison, and this runs under the store mutex on every scheduler wake.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if c := compareChatIdentity(a.Identity(), b.Identity()); c != 0 {
			return c < 0
		}
		return a.Sequence < b.Sequence
	})
	return out
}

// listAll, listPending, and listRoutePending are test-only conveniences; the
// scheduler uses the Metadata variants to avoid cloning record bodies.
func (s *IngressStore) listAll() ([]IngressRecord, error)     { return s.list(nil, false, false), nil }
func (s *IngressStore) listPending() ([]IngressRecord, error) { return s.list(nil, true, false), nil }
func (s *IngressStore) listRoutePending(id ChatIdentity) ([]IngressRecord, error) {
	return s.list(&id, true, false), nil
}

// listPendingMetadata is test-only: the scheduler walks PendingRoutes and
// clones bodies per route instead of scanning every pending record.
func (s *IngressStore) listPendingMetadata() []IngressRecord {
	return s.list(nil, true, true)
}

// PendingRoutes returns the routes holding at least one pending record, ordered
// by identity. The scheduler uses it to visit only routes that can dispatch
// instead of rescanning every pending record on each wake.
func (s *IngressStore) PendingRoutes() []ChatIdentity {
	s.mu.Lock()
	out := make([]ChatIdentity, 0, len(s.pendingByRoute))
	for id := range s.pendingByRoute {
		out = append(out, id)
	}
	s.mu.Unlock()
	slices.SortFunc(out, compareChatIdentity)
	return out
}

func compareChatIdentity(a, b ChatIdentity) int {
	if a.Platform != b.Platform {
		return strings.Compare(a.Platform, b.Platform)
	}
	if a.AccountID != b.AccountID {
		return strings.Compare(a.AccountID, b.AccountID)
	}
	return strings.Compare(a.ChatID, b.ChatID)
}
func (s *IngressStore) ListRouteRecords(id ChatIdentity) ([]IngressRecord, error) {
	return s.list(&id, false, false), nil
}
func (s *IngressStore) ListRoutePendingMetadata(id ChatIdentity) []IngressRecord {
	return s.list(&id, true, true)
}
func (s *IngressStore) GetRoute(id ChatIdentity) (IngressRoute, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.routes[id]
	if !ok {
		return r, errIngressMissing
	}
	return r, nil
}
func (s *IngressStore) lookupCommittedBusiness(platform, account, business string) (ReceiveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.business[compositeKey(platform, account, business)]
	if !ok {
		return ReceiveResult{}, errIngressMissing
	}
	r := s.records[id]
	return ReceiveResult{Disposition: ReceiveDuplicate, RecordID: id, State: r.State, Reason: r.Reason}, nil
}
func (s *IngressStore) AcceptRecord(ctx context.Context, r IngressRecord, cursor *IngressCursor) ReceiveResult {
	return s.acceptRecord(ctx, r, cursor, nil)
}
func (s *IngressStore) acceptRecord(ctx context.Context, r IngressRecord, cursor *IngressCursor, valid func() bool) (outcome ReceiveResult) {
	defer func() { s.observeAcceptance(r.Platform, outcome) }()
	reject := func(reason string) ReceiveResult { return ReceiveResult{Disposition: ReceiveRejected, Reason: reason} }
	retry := func(reason string) ReceiveResult { return ReceiveResult{Disposition: ReceiveRetryable, Reason: reason} }
	if r.Platform == "" || r.AccountID == "" || r.ChatID == "" || (r.SenderID == "" && r.State != ingressRejected) || r.BusinessID == "" || !utf8.ValidString(r.Text) || len(r.Text) > 64<<10 {
		return reject("invalid_input")
	}
	for _, v := range []string{r.Platform, r.AccountID, r.ChatID, r.SenderID, r.BusinessID, r.SemanticID, r.DeliveryID} {
		if len(v) > maxScopedIDBytes || !utf8.ValidString(v) {
			return reject("invalid_identity")
		}
	}
	if r.InputDigest != "" {
		if d, err := hex.DecodeString(r.InputDigest); err != nil || len(d) != sha256.Size {
			return reject("invalid_input_digest")
		}
	}
	if r.Kind != "task" && r.Kind != "control" {
		return reject("invalid_kind")
	}
	if err := validateIngressCursor(r.Platform, cursor); err != nil {
		return reject("invalid_cursor")
	}
	// The transport-validity callback reaches adapter and admission locks.
	// Evaluate it before taking the global store mutex so one acceptance cannot
	// serialize every other ingress update behind an unrelated adapter lock.
	if valid != nil && !valid() {
		return retry("transport_identity_changed")
	}
	if err := s.lock(ctx); err != nil {
		return retry("ingress_unavailable")
	}
	defer s.mu.Unlock()
	s.pruneHistory(r.Identity())
	now := s.now()
	r.RecordID = newIngressID()
	r.RequestID = "im-" + r.RecordID
	if r.ReceivedAt.IsZero() || (r.ReceivedAt.After(now) && !r.ReceivedAt.After(now.Add(5*time.Minute))) {
		r.ReceivedAt = now
	} else if r.ReceivedAt.After(now.Add(5 * time.Minute)) {
		return reject("received_time_out_of_range")
	}
	r.UpdatedAt = now
	if r.State == "" {
		r.State = ingressReceived
	}
	if r.State != ingressReceived && r.State != ingressRejected {
		return reject("invalid_state")
	}
	if !r.SourceTime.IsZero() && (r.SourceTime.Before(now.Add(-24*time.Hour)) || r.SourceTime.After(now.Add(5*time.Minute))) {
		r.State = ingressRejected
		r.Reason = "source_time_out_of_range"
	}
	if r.Kind == "control" {
		r.Text = ""
		r.InputDigest = ""
	}
	if r.State == ingressRejected {
		r.Text = ""
	}
	if cursor != nil {
		if err := s.checkCursor(r.Platform, r.AccountID, cursor.Generation-1, *cursor); err != nil {
			return retry("cursor_generation_conflict")
		}
	}
	commitCursor := func() {
		if cursor != nil {
			s.cursors[compositeKey(r.Platform, r.AccountID)] = *cloneIngressCursor(cursor)
		}
	}
	if r.Platform == "telegram" {
		if id := s.business[compositeKey(r.Platform, r.AccountID, r.BusinessID)]; id != "" {
			old := s.records[id]
			if old.ChatID != r.ChatID || old.SemanticID != r.SemanticID || old.InputDigest != r.InputDigest {
				d := sha256.Sum256(ingressKey("", r.BusinessID, r.ChatID, r.SemanticID, r.InputDigest))
				r.BusinessID = "update-conflict:" + hex.EncodeToString(d[:])
				r.State = ingressRejected
				r.Reason = "update_id_conflict"
				r.Text = ""
			}
		}
	}
	disposition := ReceiveAccepted
	for _, index := range []struct {
		values   map[string]string
		key      string
		semantic bool
	}{{s.business, compositeKey(r.Platform, r.AccountID, r.BusinessID), false}, {s.semantic, compositeKey(r.Platform, r.AccountID, r.SemanticID), true}} {
		if index.semantic && (r.SemanticID == "" || r.Reason == "update_id_conflict") {
			continue
		}
		if id := index.values[index.key]; id != "" {
			old := s.records[id]
			if index.semantic && r.Platform == "telegram" {
				r.State = ingressRejected
				r.Reason = "semantic_duplicate"
				r.Text = ""
				disposition = ReceiveDuplicate
				break
			}
			commitCursor()
			return ReceiveResult{Disposition: ReceiveDuplicate, RecordID: old.RecordID, State: old.State, Reason: old.Reason}
		}
	}
	route, exists := s.routes[r.Identity()]
	if !exists {
		route = IngressRoute{Identity: r.Identity(), Revision: newIngressID(), Phase: "unbound"}
	}
	if r.State != ingressRejected && r.Kind == "task" && (route.Phase != "active" || route.Revision != r.RouteRevision || route.AuthorizationDigest != r.AuthorizationDigest) {
		return retry("route_changed")
	}
	reserve := 0
	if r.statusQuery && r.Kind == "control" && r.State == ingressReceived {
		reserve = 1
	}
	pending := len(s.pendingByRoute[r.Identity()])
	account := s.counts[compositeKey(r.Platform, r.AccountID)]
	size := recordSize(r)
	if len(s.records) >= ingressHistoryLimit || s.memoryBytes+size > 64<<20 || account.Pending+account.Tombstones >= ingressHistoryLimit || !r.Terminal() && (pending >= 64+reserve || account.Pending >= 1024+reserve || account.Bytes+size > 16<<20 || s.total.Pending >= 4096+reserve || s.total.Bytes+size > 64<<20) {
		return retry("capacity")
	}
	route.Sequence++
	r.Sequence = route.Sequence
	s.routes[r.Identity()] = route
	s.save(r)
	s.business[compositeKey(r.Platform, r.AccountID, r.BusinessID)] = r.RecordID
	if r.SemanticID != "" && r.Reason != "update_id_conflict" && r.Reason != "semantic_duplicate" {
		s.semantic[compositeKey(r.Platform, r.AccountID, r.SemanticID)] = r.RecordID
	}
	commitCursor()
	s.signal()
	if r.State == ingressRejected && disposition != ReceiveDuplicate {
		disposition = ReceiveRejected
	}
	return ReceiveResult{Disposition: disposition, RecordID: r.RecordID, State: r.State, Reason: r.Reason}
}
func (s *IngressStore) UpdateRecord(ctx context.Context, id string, allowed []ingressState, mutate func(*IngressRecord) error) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return errIngressMissing
	}
	if !slices.Contains(allowed, r.State) {
		return errIngressBlocked
	}
	previous := r.State
	r = cloneIngressRecord(r)
	if err := mutate(&r); err != nil {
		return err
	}
	r.UpdatedAt = s.now()
	s.save(r)
	s.ObserveStateTransition(r.Platform, string(previous), string(r.State), r.Reason)
	s.signal()
	return nil
}
