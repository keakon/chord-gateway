package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

func observerTestProcess() *ChordProcess {
	return &ChordProcess{runtimeID: "runtime", sessionEpoch: 2, state: ControlState{SessionID: "session"}, stdin: &testWriteCloser{}}
}

type testWriteCloser struct{}

func (*testWriteCloser) Write(b []byte) (int, error) { return len(b), nil }
func (*testWriteCloser) Close() error                { return nil }

func applyObserverReceipt(p *ChordProcess, status string, seq uint64) {
	payload, _ := json.Marshal(InputResultPayload{RequestID: "im-record", Status: status})
	p.mu.Lock()
	p.applyInputResult(&HeadlessEnvelope{Payload: payload, Seq: seq}, time.Now())
	p.mu.Unlock()
}

func waitObserver(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("observer did not settle")
}

func TestInputObserverStaleSequenceAndCommitProtection(t *testing.T) {
	p := observerTestProcess()
	p.state.LastEnvelopeSeq = 100
	entered, release := make(chan ProcessInputReceipt, 1), make(chan struct{})
	defer close(release)
	if err := p.ObserveInputReceipt("im-record", func(r ProcessInputReceipt) error { entered <- r; <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.SendUserMessageWithRequestID("hello", "im-record"); err != nil {
		t.Fatal(err)
	}
	applyObserverReceipt(p, inputQueued, 1)
	select {
	case r := <-entered:
		if r.Status != inputQueued || r.RuntimeID != "runtime" {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("stale UI sequence lost receipt")
	}
	// Acquiring the lock while commit is blocked proves a receipt callback cannot block p.mu.
	p.mu.Lock()
	if len(p.pendingInputs) != 1 {
		t.Fatal("pending removed before commit")
	}
	p.mu.Unlock()
}

func TestInputObserverFailureRetryAndLateReceipt(t *testing.T) {
	p := observerTestProcess()
	fail := true
	results := make(chan string, 4)
	if err := p.ObserveInputReceipt("im-record", func(r ProcessInputReceipt) error {
		results <- r.Status
		if fail {
			return errors.New("receipt application unavailable")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.SendUserMessageWithRequestID("hello", "im-record"); err != nil {
		t.Fatal(err)
	}
	applyObserverReceipt(p, inputUnknown, 1)
	waitObserver(t, func() bool { p.mu.RLock(); defer p.mu.RUnlock(); return p.inputReceiptBlocked && !p.inputReceiptWorker })
	if err := p.SendUserMessage("more"); !errors.Is(err, ErrInputReceiptBackpressure) {
		t.Fatal(err)
	}
	// The worker stopped before this synchronization point.
	p.mu.Lock()
	fail = false
	p.mu.Unlock()
	applyObserverReceipt(p, inputUnknown, 2)
	waitObserver(t, func() bool { p.mu.RLock(); defer p.mu.RUnlock(); return !p.inputReceiptWorker })
	p.mu.RLock()
	retained := len(p.inputObservers) == 1 && len(p.pendingInputs) == 1
	p.mu.RUnlock()
	if !retained {
		t.Fatal("unknown lost late receipt protection")
	}
	applyObserverReceipt(p, inputStarted, 3)
	waitObserver(t, func() bool {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return len(p.inputObservers) == 0 && len(p.pendingInputs) == 0 && p.state.UnconfirmedInputs == 0
	})
}

func TestInputObserverIdentityAndExit(t *testing.T) {
	p := observerTestProcess()
	results := make(chan ProcessInputReceipt, 2)
	if err := p.ObserveInputReceipt("im-record", func(r ProcessInputReceipt) error { results <- r; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.SendUserMessageWithRequestID("hello", "im-record"); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.runtimeID = "replacement"
	p.mu.Unlock()
	applyObserverReceipt(p, inputStarted, 1)
	select {
	case <-results:
		t.Fatal("old observer accepted new runtime")
	default:
	}
	p.mu.Lock()
	p.abandonPendingInputs()
	p.mu.Unlock()
	select {
	case r := <-results:
		if r.Status != inputUnknown || r.RuntimeID != "runtime" {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("exit receipt missing")
	}
}

func TestInputObserverBoundAndShortWrite(t *testing.T) {
	p := observerTestProcess()
	p.stdin = shortObserverWriter{}
	results := make(chan ProcessInputReceipt, 1)
	if err := p.ObserveInputReceipt("im-record", func(r ProcessInputReceipt) error { results <- r; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.SendUserMessageWithRequestID("hello", "im-record"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	select {
	case r := <-results:
		if r.Status != inputUnknown {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("short write not unknown")
	}
}

type shortObserverWriter struct{}

func (shortObserverWriter) Write(b []byte) (int, error) { return 1, nil }
func (shortObserverWriter) Close() error                { return nil }

func TestInputObserverCapacity(t *testing.T) {
	p := observerTestProcess()
	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, o := range p.inputObservers {
			o.timer.Stop()
		}
	}()
	for i := 0; i < maxPendingInputs; i++ {
		if err := p.ObserveInputReceipt(fmt.Sprint(i), func(ProcessInputReceipt) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.ObserveInputReceipt("overflow", func(ProcessInputReceipt) error { return nil }); !errors.Is(err, ErrInputReceiptBackpressure) {
		t.Fatal(err)
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.inputObservers) != maxPendingInputs {
		t.Fatal("observer bound exceeded")
	}
}

func TestInputObserverDoneRaceRejectsBeforeWrite(t *testing.T) {
	p := observerTestProcess()
	writer := &captureWriteCloser{}
	p.stdin = writer
	if err := p.ObserveInputReceipt("im-record", func(ProcessInputReceipt) error { return nil }); err != nil {
		t.Fatal(err)
	}
	defer p.inputObservers["im-record"].timer.Stop()
	p.state.PendingConfirm = &ConfirmPayload{ToolName: "Done"}
	if err := p.SendUserMessageWithRequestID("hello", "im-record"); !errors.Is(err, errIngressDonePending) {
		t.Fatal(err)
	}
	if writer.String() != "" {
		t.Fatal("input written through new Done confirmation")
	}
}

func TestInputObserverShutdownJoinsCommit(t *testing.T) {
	m := &ChordManager{}
	p := observerTestProcess()
	p.mgr = m
	entered, release := make(chan struct{}), make(chan struct{})
	if err := p.ObserveInputReceipt("im-record", func(r ProcessInputReceipt) error { close(entered); <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() { m.WaitInputReceiptsShutdown(); close(finished) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not record unknown")
	}
	select {
	case <-finished:
		t.Fatal("shutdown did not join receipt callbacks")
	default:
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
	if err := p.ObserveInputReceipt("new", func(ProcessInputReceipt) error { return nil }); err == nil {
		t.Fatal("shutdown accepted observer")
	}
}

// A commit that started before the seal runs on the unbounded normal path,
// outside the per-commit watchdog: the shutdown join itself must be bounded.
func TestInputObserverShutdownBoundsInFlightCommit(t *testing.T) {
	oldFlush := inputReceiptFlushTimeout
	inputReceiptFlushTimeout = 50 * time.Millisecond
	t.Cleanup(func() { inputReceiptFlushTimeout = oldFlush })

	m := &ChordManager{}
	p := observerTestProcess()
	p.key = "workspace|wechat|chat"
	p.mgr = m
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, o := range p.inputObservers {
			o.timer.Stop()
		}
	})
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	if err := p.ObserveInputReceipt("im-record", func(ProcessInputReceipt) error {
		entered <- struct{}{}
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Start the worker while shutdown is still false, so the commit runs on
	// the unbounded normal path.
	applyObserverReceipt(p, inputStarted, 1)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start before shutdown")
	}
	finished := make(chan struct{})
	go func() { m.WaitInputReceiptsShutdown(); close(finished) }()
	mutating, stopMutating, mutated := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(mutated)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		close(mutating)
		for {
			select {
			case <-stopMutating:
				return
			case <-ticker.C:
				p.mu.Lock()
				p.state.SessionID = "updated-session"
				p.mu.Unlock()
			}
		}
	}()
	<-mutating
	defer func() { close(stopMutating); <-mutated }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown joined an in-flight commit without a bound")
	}
	// Releasing lets the sealed worker settle in the background.
	releaseOnce.Do(func() { close(release) })
	waitObserver(t, func() bool { p.mu.RLock(); defer p.mu.RUnlock(); return !p.inputReceiptWorker })
}

// A commit callback that never returns must not hang gateway shutdown: the
// shutdown flush bounds each commit and treats the timeout as a failure.
func TestInputObserverShutdownBoundsStuckCommit(t *testing.T) {
	oldTimeout := inputReceiptCommitTimeout
	inputReceiptCommitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { inputReceiptCommitTimeout = oldTimeout })

	m := &ChordManager{}
	p := observerTestProcess()
	p.mgr = m
	entered, release := make(chan struct{}), make(chan struct{})
	if err := p.ObserveInputReceipt("im-record", func(ProcessInputReceipt) error {
		close(entered)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() { m.WaitInputReceiptsShutdown(); close(finished) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not record unknown")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not bound the stuck commit")
	}
	close(release)
}
