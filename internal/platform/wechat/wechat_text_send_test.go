package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// wechatSendStub records sendmessage bodies and authorization headers and
// replies with handler-controlled outcomes. The shared sender calls it
// synchronously per segment, so the hooks see one-based request numbers.
// onRequest runs before the reply, block suspends the request until it is
// closed, and respond picks the outcome; nil respond accepts the segment.
type wechatSendStub struct {
	mu        sync.Mutex
	bodies    []ILinkSendMessageRequest
	auths     []string
	deadlines []time.Time
	onRequest func(n int, body ILinkSendMessageRequest)
	block     <-chan struct{}
	respond   func(n int, body ILinkSendMessageRequest) (int, string)
}

func (s *wechatSendStub) RoundTrip(req *http.Request) (*http.Response, error) {
	var body ILinkSendMessageRequest
	if req.Body != nil {
		defer req.Body.Close()
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
	}
	deadline, _ := req.Context().Deadline()
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	s.auths = append(s.auths, req.Header.Get("Authorization"))
	s.deadlines = append(s.deadlines, deadline)
	n := len(s.bodies)
	onRequest, block, respond := s.onRequest, s.block, s.respond
	s.mu.Unlock()
	if onRequest != nil {
		onRequest(n, body)
	}
	if block != nil {
		// Wait like the real transport does: released by the test or cancelled
		// through the request context.
		select {
		case <-block:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	if respond == nil {
		return ilinkResponse(http.StatusOK, `{"ret":0}`), nil
	}
	status, payload := respond(n, body)
	return ilinkResponse(status, payload), nil
}

func (s *wechatSendStub) requests() ([]ILinkSendMessageRequest, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ILinkSendMessageRequest(nil), s.bodies...), append([]string(nil), s.auths...)
}

func (s *wechatSendStub) requestDeadlines() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.deadlines...)
}

func ilinkResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func newSendTestAdapter(t *testing.T) (*WechatAdapter, *wechatSendStub) {
	t.Helper()
	a := newTestAdapter(t)
	stub := &wechatSendStub{}
	a.SetTransportForTest(stub)
	// apiPostSnapshot refuses an empty endpoint before reaching the stubbed
	// transport, so give it a neutral placeholder.
	a.SetBaseURLForTest("https://wechat-ilink.example.test")
	a.cachePairedContext("bot", "chat", "ctx-1", true)
	return a, stub
}

func TestWechatTextSendUsesOneTotalBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		control      bool
		parentBudget time.Duration
		want         time.Duration
	}{
		{name: "normal budget", parentBudget: time.Hour, want: im.IMTextSendTimeout},
		{name: "control budget", control: true, parentBudget: time.Hour, want: im.IMControlSendTimeout},
		{name: "shorter caller normal", parentBudget: time.Second, want: im.IMTextSendTimeout},
		{name: "shorter caller control", control: true, parentBudget: time.Second, want: im.IMControlSendTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, stub := newSendTestAdapter(t)
			// Observe the output budget, not the HTTP client's shorter
			// per-request timeout.
			a.httpClient.Timeout = 0
			ctx, cancel := context.WithTimeout(t.Context(), tc.parentBudget)
			defer cancel()
			if tc.control {
				ctx = context.WithValue(ctx, im.OutboundReplyContextKey{}, im.ReplyPermit{})
			}
			parentDeadline, _ := ctx.Deadline()
			start := time.Now()
			if err := a.SendTextContext(ctx, "chat", strings.Repeat("a", 3*ILinkMaxMessageLen)); err != nil {
				t.Fatal(err)
			}
			end := time.Now()
			deadlines := stub.requestDeadlines()
			if len(deadlines) != 3 {
				t.Fatalf("request deadlines = %v, want three segments", deadlines)
			}
			earliest, latest := start.Add(tc.want), end.Add(tc.want)
			if parentDeadline.Before(earliest) {
				earliest = parentDeadline
			}
			if parentDeadline.Before(latest) {
				latest = parentDeadline
			}
			if deadlines[0].Before(earliest) || deadlines[0].After(latest) {
				t.Fatalf("deadline = %s, want between %s and %s", deadlines[0], earliest, latest)
			}
			for i, deadline := range deadlines[1:] {
				if !deadline.Equal(deadlines[0]) {
					t.Fatalf("segment %d renewed deadline: %s != %s", i+2, deadline, deadlines[0])
				}
			}
		})
	}
}

// A caller deadline expiring during HTTP I/O must retain the unknown outcome,
// unlike a cancellation before the segment was sent.
func TestWechatTextSendCallerDeadlineBoundsWholeOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		control bool
	}{
		{name: "normal budget"},
		{name: "control budget", control: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, stub := newSendTestAdapter(t)
			release := make(chan struct{})
			defer close(release)
			stub.block = release
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if tc.control {
				ctx = context.WithValue(ctx, im.OutboundReplyContextKey{}, im.ReplyPermit{})
			}
			start := time.Now()
			err := a.SendTextContext(ctx, "chat", strings.Repeat("a", 3*ILinkMaxMessageLen))
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("caller deadline ignored, waited %s", elapsed)
			}
			var sendErr *im.IMTextSendError
			if !errors.As(err, &sendErr) || sendErr.AcceptedSegments != 0 {
				t.Fatalf("error = %v, want 0 accepted segments", err)
			}
			// The segment was in flight when the deadline hit: the outcome stays
			// unknown instead of being downgraded to a definite non-delivery.
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
				t.Fatalf("error = %v, want deadline exceeded with unknown outcome", err)
			}
		})
	}
}

// W5-A03: a call cancelled while queued for the target slot sends nothing and
// releases its slot reference.
func TestWechatTextSendQueuedCancellationSendsNothing(t *testing.T) {
	a, stub := newSendTestAdapter(t)
	holder := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	stub.block = release
	stub.onRequest = func(n int, _ ILinkSendMessageRequest) {
		if n == 1 {
			close(holder)
		}
	}
	done := make(chan error, 1)
	go func() { done <- a.SendTextContext(context.Background(), "chat", "holder output") }()
	<-holder

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := a.SendTextContext(ctx, "chat", "queued output"); err == nil {
		t.Fatal("queued call survived cancellation")
	}
	// The holder must still own the slot: the queued call left no reference
	// behind and never reached the transport.
	sender, err := a.wechatTextSender()
	if err != nil {
		t.Fatalf("wechatTextSender error = %v", err)
	}
	if refs := sender.TargetRefsForTest("chat"); refs != 1 {
		t.Fatalf("target refs = %d, want only the holder", refs)
	}
	bodies, _ := stub.requests()
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want only the holder's", len(bodies))
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatalf("holder send error = %v", err)
	}
	if refs := sender.TargetRefsForTest("chat"); refs != 0 {
		t.Fatalf("target refs = %d after completion, want 0", refs)
	}
}

// W5-A03: Disconnect ends an in-flight output; the aborted segment keeps its
// unknown outcome and the slot is released.
func TestWechatTextSendDisconnectEndsInFlightOutput(t *testing.T) {
	a, stub := newSendTestAdapter(t)
	release := make(chan struct{})
	defer close(release)
	stub.block = release
	done := make(chan error, 1)
	go func() {
		done <- a.SendTextContext(context.Background(), "chat", strings.Repeat("a", 3*ILinkMaxMessageLen))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for bodies, _ := stub.requests(); len(bodies) == 0 && time.Now().Before(deadline); bodies, _ = stub.requests() {
		time.Sleep(time.Millisecond)
	}
	a.Disconnect()
	select {
	case err := <-done:
		if !errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
			t.Fatalf("error = %v, want unknown delivery outcome", err)
		}
		var sendErr *im.IMTextSendError
		if !errors.As(err, &sendErr) || sendErr.AcceptedSegments != 0 {
			t.Fatalf("error = %v, want 0 accepted segments", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not end the in-flight output")
	}
	sender, err := a.wechatTextSender()
	if err != nil {
		t.Fatalf("wechatTextSender error = %v", err)
	}
	if refs := sender.TargetRefsForTest("chat"); refs != 0 {
		t.Fatalf("target refs = %d after disconnect, want 0", refs)
	}
}

// W5-A03: a supervisor stop between segments refuses the next segment as a
// definite non-delivery without touching the transport.
func TestWechatTextSendStopRefusesNextSegment(t *testing.T) {
	a, stub := newSendTestAdapter(t)
	stub.onRequest = func(n int, _ ILinkSendMessageRequest) {
		if n == 1 {
			a.SetStoppedForTest(true)
		}
	}
	err := a.SendTextContext(context.Background(), "chat", strings.Repeat("a", 3*ILinkMaxMessageLen))
	var sendErr *im.IMTextSendError
	if !errors.As(err, &sendErr) || sendErr.AcceptedSegments != 1 || sendErr.Category != "connection_unavailable" {
		t.Fatalf("error = %v, want stop refusal after 1 accepted segment", err)
	}
	if errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
		t.Fatalf("definite stop refusal reported as unknown outcome: %v", err)
	}
	bodies, _ := stub.requests()
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want the stopped call to halt after one segment", len(bodies))
	}
}

// W5-A02: one adapter keeps a single sender across calls, and sends to
// different targets run concurrently through it.
func TestWechatTextSendSingleSenderAndCrossTargetConcurrency(t *testing.T) {
	a, stub := newSendTestAdapter(t)
	sender, err := a.wechatTextSender()
	if err != nil {
		t.Fatalf("wechatTextSender error = %v", err)
	}
	a.cachePairedContext("bot", "chat-2", "ctx-2", true)

	var enteredMu sync.Mutex
	entered := 0
	ready := make(chan struct{})
	stub.block = ready
	stub.onRequest = func(_ int, _ ILinkSendMessageRequest) {
		enteredMu.Lock()
		entered++
		if entered == 2 {
			close(ready)
		}
		enteredMu.Unlock()
	}
	done := make(chan error, 2)
	go func() { done <- a.SendTextContext(context.Background(), "chat", "first") }()
	go func() { done <- a.SendTextContext(context.Background(), "chat-2", "second") }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("cross-target send error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("different targets were serialized")
		}
	}
	if got, err := a.wechatTextSender(); err != nil || got != sender {
		t.Fatalf("sender instance changed across calls: %v", err)
	}
}

// W5-A04: token rotation between segments stops the old call on its frozen
// snapshot; a later call may independently use the new credentials.
func TestWechatTextSendRotationStopsOldCallSnapshot(t *testing.T) {
	t.Run("same account token rotation", func(t *testing.T) {
		a, stub := newSendTestAdapter(t)
		stub.onRequest = func(n int, _ ILinkSendMessageRequest) {
			if n == 1 {
				a.StoreTokenForTest(&TokenData{Token: "rotated", AccountID: "bot"})
			}
		}
		err := a.SendTextContext(context.Background(), "chat", strings.Repeat("a", 3*ILinkMaxMessageLen))
		var sendErr *im.IMTextSendError
		if !errors.As(err, &sendErr) || sendErr.AcceptedSegments != 1 || sendErr.Category != "identity_mismatch" {
			t.Fatalf("error = %v, want identity refusal after 1 accepted segment", err)
		}
		bodies, auths := stub.requests()
		if len(bodies) != 1 {
			t.Fatalf("requests = %d, want old call to halt after one segment", len(bodies))
		}
		if auths[0] != "Bearer sample-token" {
			t.Fatalf("first segment authorization = %q", auths[0])
		}
		// A still-valid new call independently uses the rotated credential.
		if err := a.SendTextContext(context.Background(), "chat", "fresh"); err != nil {
			t.Fatalf("new call error = %v", err)
		}
		bodies, auths = stub.requests()
		if len(bodies) != 2 || auths[1] != "Bearer rotated" || bodies[1].Msg.ContextToken != "ctx-1" {
			t.Fatalf("new call did not use its own snapshot: auth=%q body=%#v", auths[1], bodies[1])
		}
	})

	t.Run("account replacement never redirects old output", func(t *testing.T) {
		a, stub := newSendTestAdapter(t)
		stub.onRequest = func(n int, _ ILinkSendMessageRequest) {
			if n == 1 {
				a.StoreTokenForTest(&TokenData{Token: "replacement", AccountID: "bot-b"})
			}
		}
		err := a.SendTextContext(context.Background(), "chat", strings.Repeat("a", 3*ILinkMaxMessageLen))
		var sendErr *im.IMTextSendError
		if !errors.As(err, &sendErr) || sendErr.AcceptedSegments != 1 || sendErr.Category != "identity_mismatch" {
			t.Fatalf("error = %v, want identity refusal after 1 accepted segment", err)
		}
		a.cachePairedContext("bot-b", "chat", "ctx-b", true)
		if err := a.SendTextContext(context.Background(), "chat", "fresh"); err != nil {
			t.Fatalf("new account call error = %v", err)
		}
		bodies, auths := stub.requests()
		if len(bodies) != 2 {
			t.Fatalf("requests = %d, want one per call", len(bodies))
		}
		for i, body := range bodies {
			if body.Msg.ToUserID != "chat" {
				t.Fatalf("request %d switched target: %#v", i, body)
			}
		}
		if auths[1] != "Bearer replacement" || bodies[1].Msg.ContextToken != "ctx-b" {
			t.Fatalf("new call did not use the replacement snapshot: auth=%q body=%#v", auths[1], bodies[1])
		}
	})
}

// W5-A05: an unbound reply context captured before queueing expires on its own
// schedule; the queued call refuses instead of borrowing a refreshed bound
// context. Pairing feedback itself stays sendable while the context is valid.
func TestWechatTextSendQueuedExpiryDoesNotBorrowFreshContext(t *testing.T) {
	a, stub := newSendTestAdapter(t)
	a.cachePairedContext("bot", "chat", "unbound-ctx", false)
	entry, _ := a.ReplyContextForTest(compositeKey("bot", "chat"))
	sender, err := a.wechatTextSender()
	if err != nil {
		t.Fatalf("wechatTextSender error = %v", err)
	}
	original := sender.SendSegmentForTest()
	sender.SetSendSegmentForTest(func(ctx context.Context, target, text string) error {
		snapshot, ok := ctx.Value(wechatSendSnapshotKey{}).(*wechatSendSnapshot)
		if !ok || snapshot == nil || snapshot.reply.Token != entry.Token || snapshot.reply.Bound || !snapshot.reply.Expires.Equal(entry.Expires) {
			t.Error("queued call replaced its original reply context")
			return errors.New("unexpected send snapshot")
		}
		// Simulate expiry after queueing without relying on scheduler timing
		// or adding a production clock hook.
		snapshot.reply.Expires = time.Now().Add(-time.Second)
		return original(ctx, target, text)
	})
	release, err := sender.AcquireTarget(t.Context(), "chat")
	if err != nil {
		t.Fatal(err)
	}
	unblock := sync.OnceFunc(release)
	defer unblock()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	queued := make(chan error, 1)
	go func() { queued <- a.SendTextContext(ctx, "chat", "queued output") }()
	for sender.TargetRefsForTest("chat") != 2 {
		select {
		case err := <-queued:
			t.Fatalf("call failed before queueing: %v", err)
		case <-ctx.Done():
			t.Fatal("queued call never reached the target slot")
		case <-time.After(time.Millisecond):
		}
	}
	a.cachePairedContext("bot", "chat", "fresh-ctx", true)
	unblock()
	err = <-queued
	var sendErr *im.IMTextSendError
	if !errors.As(err, &sendErr) || sendErr.AcceptedSegments != 0 || sendErr.Category != "local_error" {
		t.Fatalf("error = %v, want expired-context refusal with nothing sent", err)
	}
	if !strings.Contains(sendErr.Cause.Error(), "reply context expired") {
		t.Fatalf("error = %v, want expiry evidence in the cause", err)
	}
	bodies, _ := stub.requests()
	if len(bodies) != 0 {
		t.Fatalf("requests = %d, want the expired queued call to send nothing", len(bodies))
	}
	if refs := sender.TargetRefsForTest("chat"); refs != 0 {
		t.Fatalf("target refs = %d after completion, want 0", refs)
	}
}

// W5-A05: a valid unbound context still serves the pairing feedback path.
func TestWechatTextSendUnboundContextServesPairingFeedback(t *testing.T) {
	a, stub := newSendTestAdapter(t)
	a.cachePairedContext("bot", "chat", "unbound-ctx", false)
	ctx := context.WithValue(context.Background(), im.OutboundReplyContextKey{}, im.ReplyPermit{})
	if err := a.SendTextContext(ctx, "chat", "pairing hint"); err != nil {
		t.Fatalf("pairing feedback error = %v", err)
	}
	bodies, _ := stub.requests()
	if len(bodies) != 1 || bodies[0].Msg.ContextToken != "unbound-ctx" {
		t.Fatalf("bodies = %#v", bodies)
	}
}

// W5-A06: the adapter's shared sender bounds segments at the iLink budget,
// reserves the truncation notice, preserves content prefixes and refuses to
// send empty text.
func TestWechatTextSendSegmentsTruncationAndEmptyText(t *testing.T) {
	t.Run("empty text sends nothing", func(t *testing.T) {
		a, stub := newSendTestAdapter(t)
		if err := a.SendTextContext(context.Background(), "chat", ""); err != nil {
			t.Fatalf("empty text error = %v", err)
		}
		bodies, _ := stub.requests()
		if len(bodies) != 0 {
			t.Fatalf("requests = %d, want none", len(bodies))
		}
	})

	t.Run("segment count cap reserves the notice", func(t *testing.T) {
		a, stub := newSendTestAdapter(t)
		text := strings.Repeat(strings.Repeat("a", 1500)+"\n", 33)
		if err := a.SendTextContext(context.Background(), "chat", text); err != nil {
			t.Fatalf("long text error = %v", err)
		}
		bodies, _ := stub.requests()
		if len(bodies) != im.MaxTextSegments {
			t.Fatalf("requests = %d, want %d", len(bodies), im.MaxTextSegments)
		}
		var content strings.Builder
		for i, body := range bodies {
			segment := body.Msg.ItemList[0].TextItem.Text
			if len([]rune(segment)) > ILinkMaxMessageLen {
				t.Fatalf("segment %d exceeds budget: %d runes", i, len([]rune(segment)))
			}
			if i < im.MaxTextSegments-1 {
				content.WriteString(segment)
				continue
			}
			if !strings.Contains(segment, "Output truncated") {
				t.Fatalf("last segment %q misses the truncation notice", segment)
			}
		}
		// Everything before the notice is an exact prefix of the original text.
		last := bodies[im.MaxTextSegments-1].Msg.ItemList[0].TextItem.Text
		if before, _, ok := strings.Cut(last, "\n"); ok {
			content.WriteString(before)
		}
		if !strings.HasPrefix(text, content.String()) {
			t.Fatal("segmented content is not a prefix of the original text")
		}
	})

	t.Run("emoji segmentation preserves content", func(t *testing.T) {
		a, stub := newSendTestAdapter(t)
		text := strings.Repeat("\U0001F642", 2*ILinkMaxMessageLen+1)
		if err := a.SendTextContext(context.Background(), "chat", text); err != nil {
			t.Fatalf("emoji text error = %v", err)
		}
		bodies, _ := stub.requests()
		if len(bodies) != 3 {
			t.Fatalf("requests = %d, want three emoji segments", len(bodies))
		}
		var joined strings.Builder
		for i, body := range bodies {
			segment := body.Msg.ItemList[0].TextItem.Text
			if len([]rune(segment)) > ILinkMaxMessageLen {
				t.Fatalf("emoji segment %d exceeds budget", i+1)
			}
			joined.WriteString(segment)
		}
		if joined.String() != text {
			t.Fatalf("emoji segmentation altered content: %d segments", len(bodies))
		}
	})
}

// W5-A07/A08: partial failures report only explicitly accepted segments,
// keep unknown outcomes discoverable, never retry a failed or delivered
// segment, and surface the same evidence through metrics and the /status
// target snapshot.
func TestWechatTextSendPartialFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		category string
		unknown  bool
	}{
		{name: "definite rejection", status: http.StatusBadRequest, body: `{"ret":1}`, category: "delivery_failed"},
		{name: "API rejection", status: http.StatusOK, body: `{"ret":1}`, category: "delivery_failed"},
		{name: "request timeout", status: http.StatusRequestTimeout, body: ``, category: "unknown_send", unknown: true},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"ret":0}`, category: "unknown_send", unknown: true},
		{name: "server error", status: http.StatusBadGateway, body: `{"ret":0}`, category: "unknown_send", unknown: true},
		{name: "missing receipt", status: http.StatusOK, body: `{}`, category: "unknown_send", unknown: true},
		{name: "malformed receipt", status: http.StatusOK, body: `invalid`, category: "unknown_send", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, stub := newSendTestAdapter(t)
			stub.respond = func(n int, _ ILinkSendMessageRequest) (int, string) {
				if n == 2 {
					return tc.status, tc.body
				}
				return http.StatusOK, `{"ret":0}`
			}
			first, second, third := strings.Repeat("a", ILinkMaxMessageLen), strings.Repeat("b", ILinkMaxMessageLen), strings.Repeat("c", ILinkMaxMessageLen)
			err := a.SendTextContext(t.Context(), "chat", first+second+third)
			var sendErr *im.IMTextSendError
			if !errors.As(err, &sendErr) || sendErr.AcceptedSegments != 1 || sendErr.Category != tc.category {
				t.Fatalf("error = %v, want category %s after 1 accepted segment", err, tc.category)
			}
			if errors.Is(err, im.ErrDeliveryOutcomeUnknown) != tc.unknown {
				t.Fatalf("error = %v, unknown = %v, want %v", err, errors.Is(err, im.ErrDeliveryOutcomeUnknown), tc.unknown)
			}
			bodies, _ := stub.requests()
			if len(bodies) != 2 {
				t.Fatalf("requests = %d, want no retry and no third segment", len(bodies))
			}
			if bodies[0].Msg.ItemList[0].TextItem.Text != first || bodies[1].Msg.ItemList[0].TextItem.Text != second {
				t.Fatal("sent segments were retried or reordered")
			}
			if text := err.Error(); strings.Contains(text, "sample-token") || strings.Contains(text, "ctx-1") {
				t.Fatalf("error leaks credentials: %v", text)
			}

			snapshot := a.TextSendTargetSnapshot("chat")
			if snapshot.LastFailureCategory != tc.category || snapshot.LastAcceptedSegments != 1 || snapshot.LastFailureAt.IsZero() {
				t.Fatalf("target snapshot = %#v", snapshot)
			}
			metrics := a.TextSendMetrics()
			if metrics.Platform != "wechat" || metrics.Failures != 1 || metrics.PartialFailures != 1 || metrics.RateLimitWaits != 0 {
				t.Fatalf("metrics = %#v", metrics)
			}
		})
	}

	t.Run("first segment failure keeps no target snapshot", func(t *testing.T) {
		a, stub := newSendTestAdapter(t)
		stub.respond = func(int, ILinkSendMessageRequest) (int, string) {
			return http.StatusBadRequest, `{"ret":1}`
		}
		if err := a.SendTextContext(context.Background(), "chat", "text"); err == nil {
			t.Fatal("first segment failure accepted")
		}
		if snapshot := a.TextSendTargetSnapshot("chat"); !snapshot.LastFailureAt.IsZero() {
			t.Fatalf("target snapshot = %#v, want empty for zero accepted segments", snapshot)
		}
		metrics := a.TextSendMetrics()
		if metrics.Failures != 1 || metrics.PartialFailures != 0 {
			t.Fatalf("metrics = %#v", metrics)
		}
	})
}
