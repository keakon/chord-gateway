package main

import (
	"context"
	"testing"
	"time"
)

type slowTransferAdapter struct {
	stubIMAdapter
	entered chan struct{}
	release chan struct{}
}

func (a *slowTransferAdapter) SendText(chat, text string) error {
	return a.SendTextContext(context.Background(), chat, text)
}
func (a *slowTransferAdapter) SendTextContext(ctx context.Context, chat, text string) error {
	select {
	case a.entered <- struct{}{}:
	default:
	}
	select {
	case <-a.release:
		return a.stubIMAdapter.SendText(chat, text)
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestTransferDeliveryDoesNotHoldLifecycleOrReader(t *testing.T) {
	m, p, _ := newTakeoverTestProcess(t)
	r := NewNotificationRouter(m)
	a := &slowTransferAdapter{stubIMAdapter: stubIMAdapter{typ: "wechat"}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	r.SetAdapter(a)
	p.onEvent = r.HandleChordEvent
	defer r.Close()
	defer close(a.release)
	done := make(chan struct{})
	go func() {
		p.processEnvelope(takeoverEnvelope(t, "session_transfer_preparing", takeoverTestRecord("preparing").Status))
		close(done)
	}()
	select {
	case <-a.entered:
	case <-time.After(time.Second):
		t.Fatal("delivery never started")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader blocked on delivery")
	}
	locked := make(chan struct{})
	go func() {
		m.lifecycleGate.Lock()
		release := m.acquireKeyLock(p.key)
		release()
		m.lifecycleGate.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("delivery holds lifecycle lock")
	}
}
