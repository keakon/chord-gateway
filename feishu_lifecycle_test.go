package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// This fixture keeps the actual intake workers and dispatcher. Only the network
// connection is replaced, so migration/account ownership exercise production.
func lifecycleFeishuAdapter(t *testing.T, account string) (*FeishuAdapter, *IngressService, string) {
	t.Helper()
	dir := t.TempDir()
	fc := &config.FeishuConfig{AppID: account, Owner: &config.OwnerIdentity{AccountID: account, UserID: "owner"}, ChatBindings: map[string]string{"chat": "workspace"}}
	cfg := &config.Config{IMs: []config.IMAdapterConfig{{Feishu: fc}}, Workspaces: []config.Workspace{{ID: "workspace", Path: t.TempDir()}}}
	paths := &config.Paths{StateDir: dir}
	configStore := &ConfigStore{}
	configStore.snapshot.Store(cfg)
	mgr := NewChordManager(cfg, paths)
	mgr.store = configStore
	router := NewNotificationRouter(mgr)
	router.admission = NewAdmission(NewPairingService(configStore, false, nil))
	mgr.admission = router.admission
	store := NewIngressStore()
	service := NewIngressService(store, router)
	serviceCtx, cancelService := context.WithCancel(context.Background())
	if err := service.Start(serviceCtx); err != nil {
		t.Fatal(err)
	}
	a, err := NewFeishuAdapter(cfg, cfg.IMs[0], paths, router)
	if err != nil {
		t.Fatal(err)
	}
	a.accessToken, a.tokenExpireAt = "cached-lifecycle-token", time.Now().Add(time.Hour)
	router.SetAdapter(a)
	t.Cleanup(func() {
		a.Disconnect()
		cancelService()
		service.Close()
		router.Close()
		router.admission.Close()
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return a, service, dir
}

func waitFeishuLifecycle[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("Feishu lifecycle operation did not finish")
		var zero T
		return zero
	}
}

func TestFeishuLifecycleConstructorDoesNotCreateLegacyStore(t *testing.T) {
	a, _, dir := lifecycleFeishuAdapter(t, "lifecycle-constructor")
	a.Disconnect()
	for _, name := range []string{"dedupe.json", "dedupe.json.journal"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("constructor started legacy persistence %s: %v", name, err)
		}
	}
}

func TestFeishuLifecycleAccountLockAndRepeatedDisconnectJoin(t *testing.T) {
	account := "lifecycle-lock-owner"
	a, _, firstDir := lifecycleFeishuAdapter(t, account)
	b, _, secondDir := lifecycleFeishuAdapter(t, account)
	if firstDir == secondDir {
		t.Fatal("fixture did not isolate state directories")
	}
	started, cancelled, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	// Release before fixture cleanup on an assertion failure.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	a.runLongConn = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		close(exited)
		return ctx.Err()
	}
	connected := make(chan error, 1)
	go func() { connected <- a.Connect() }()
	waitFeishuLifecycle(t, started)
	var secondCalls atomic.Int32
	b.runLongConn = func(context.Context) error { secondCalls.Add(1); return nil }
	if err := b.Connect(); !errors.Is(err, im.ErrInstanceRunning) || secondCalls.Load() != 0 {
		t.Fatalf("same account accepted with a different state directory: %v calls=%d", err, secondCalls.Load())
	}
	firstDisconnect, secondDisconnect := make(chan struct{}), make(chan struct{})
	go func() { a.Disconnect(); close(firstDisconnect) }()
	waitFeishuLifecycle(t, cancelled)
	go func() { a.Disconnect(); close(secondDisconnect) }()
	select {
	case <-firstDisconnect:
		t.Fatal("Disconnect returned before the connection task exited")
	default:
	}
	if locks, err := im.AcquireIMAccountLock("feishu", account); err == nil {
		locks.Close()
		t.Fatal("account lock released before connection tasks exited")
	}
	close(release)
	waitFeishuLifecycle(t, exited)
	waitFeishuLifecycle(t, firstDisconnect)
	waitFeishuLifecycle(t, secondDisconnect)
	if err := waitFeishuLifecycle(t, connected); err != nil {
		t.Fatal(err)
	}
	locks, err := im.AcquireIMAccountLock("feishu", account)
	if err != nil {
		t.Fatalf("Disconnect did not release account ownership: %v", err)
	}
	locks.Close()
}

func TestFeishuLifecycleReconnectAndDisconnectKeepSharedStoreOpen(t *testing.T) {
	a, service, _ := lifecycleFeishuAdapter(t, "lifecycle-shared-store")
	a.reconnectInterval = time.Millisecond
	var calls atomic.Int32
	reconnected := make(chan struct{})
	a.runLongConn = func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("temporary connection failure")
		}
		close(reconnected)
		<-ctx.Done()
		return ctx.Err()
	}
	connected := make(chan error, 1)
	go func() { connected <- a.Connect() }()
	waitFeishuLifecycle(t, reconnected)
	writeOtherPlatform := func(business string) {
		t.Helper()
		record := testIngressTask(t, service.store, business)
		if result := service.store.AcceptRecord(context.Background(), record, nil); result.Disposition != im.ReceiveAccepted {
			t.Fatalf("other platform cannot commit to shared store: %+v", result)
		}
	}
	writeOtherPlatform("other-after-reconnect")
	a.Disconnect()
	if err := waitFeishuLifecycle(t, connected); err != nil {
		t.Fatal(err)
	}
	if !service.Available() {
		t.Fatal("adapter Disconnect stopped shared ingress workers")
	}
	writeOtherPlatform("other-after-disconnect")
}
