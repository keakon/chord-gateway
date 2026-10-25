package main

import (
	"context"
	"sync"
	"testing"
	"time"

	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func TestFeishuNormalizerCapacityReturnsFailureWithoutBlockingPong(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "test-app"})
	installFeishuReceiver(t, a, func(ctx context.Context, _ im.IngressEnvelope) im.ReceiveResult {
		<-ctx.Done()
		return im.ReceiveResult{Disposition: im.ReceiveUncertain}
	})
	r := a.msgRouter.(*NotificationRouter)
	for range ingressNormalizationCapacity {
		if _, err := r.ingress.normalizer.Submit("occupied-route", func(context.Context) (im.IngressEnvelope, error) { return im.IngressEnvelope{}, nil }); err != nil {
			t.Fatal(err)
		}
	}
	conn, client := feishuSocketPair(t)
	g := a.startGeneration(t.Context(), conn)
	defer g.close()
	started := time.Now()
	if err := a.handleDataFrame(g.ctx, conn, feishuTextFrame(t, "chat", "capacity-event", "capacity-delivery")); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatal("reader blocked on full normalizer", elapsed)
	}
	pong := larkws.Frame{Method: int32(larkws.FrameTypeControl), Headers: larkws.Headers{{Key: larkws.HeaderType, Value: string(larkws.MessageTypePong)}}, Payload: []byte(`{"PingInterval":9}`)}
	raw, err := pong.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.handleWSFrame(g.ctx, conn, raw); err != nil {
		t.Fatal(err)
	}
	if a.currentPingInterval() != 9*time.Second {
		t.Fatal("capacity blocked pong")
	}
	got, code := readFeishuResponse(t, client)
	if code != 500 || larkws.Headers(got.Headers).GetString(larkws.HeaderMessageID) != "capacity-delivery" {
		t.Fatal(code, string(got.Payload))
	}
}

func TestFeishuResponseCapacityRetainsGenerationWithSingleFrame500(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "test-app"})
	conn, client := feishuSocketPair(t)
	g := a.startGeneration(t.Context(), conn)
	defer g.close()
	entered := make(chan struct{}, feishuResponseWorkers)
	for range feishuResponseWorkers {
		ctx, cancel := context.WithCancel(g.ctx)
		g.responses <- feishuResponseJob{ctx: ctx, cancel: cancel, resolve: func(context.Context) (any, error) { entered <- struct{}{}; <-g.ctx.Done(); return nil, g.ctx.Err() }}
	}
	for range feishuResponseWorkers {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("response worker did not start")
		}
	}
	for range feishuResponseCapacity {
		ctx, cancel := context.WithCancel(context.Background())
		g.responses <- feishuResponseJob{ctx: ctx, cancel: cancel}
	}
	frame := larkws.Frame{Method: int32(larkws.FrameTypeData), Headers: larkws.Headers{{Key: larkws.HeaderType, Value: string(larkws.MessageTypeEvent)}, {Key: larkws.HeaderMessageID, Value: "overflow-delivery"}}}
	started := time.Now()
	// A burst above the bounded queue drops this frame with a 500 instead of
	// tearing down the generation: the reader never blocks and workers drain.
	if err := a.submitDataFrame(g.ctx, conn, frame, []byte(`{"header":{"event_type":"not.subscribed"}}`)); err != nil {
		t.Fatal("overflow must retain the generation", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("overflow blocked reader")
	}
	if g.ctx.Err() != nil {
		t.Fatal("overflow killed the generation")
	}
	got, code := readFeishuResponse(t, client)
	if code != 500 || larkws.Headers(got.Headers).GetString(larkws.HeaderMessageID) != "overflow-delivery" {
		t.Fatal(code, string(got.Payload))
	}
}

func TestFeishuLateAcceptedOldGenerationDoesNotSendToNewSocket(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "test-app"})
	entered, release, accepted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	installFeishuReceiver(t, a, func(_ context.Context, e im.IngressEnvelope) im.ReceiveResult {
		if e.Message.MessageID == "message:old-business" {
			close(entered)
			<-release
			close(accepted)
		}
		return im.ReceiveResult{Disposition: im.ReceiveAccepted}
	})
	oldConn, _ := feishuSocketPair(t)
	old := a.startGeneration(t.Context(), oldConn)
	if err := a.handleDataFrame(old.ctx, oldConn, feishuTextFrame(t, "old-chat", "old-business", "old-delivery")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("old accept did not start")
	}
	old.close()
	newConn, newClient := feishuSocketPair(t)
	next := a.startGeneration(t.Context(), newConn)
	defer next.close()
	if err := a.handleDataFrame(next.ctx, newConn, feishuTextFrame(t, "new-chat", "new-business", "new-delivery")); err != nil {
		t.Fatal(err)
	}
	frame, code := readFeishuResponse(t, newClient)
	if code != 200 || larkws.Headers(frame.Headers).GetString(larkws.HeaderMessageID) != "new-delivery" {
		t.Fatal("old response contaminated current generation")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("late admission did not complete")
	}
	_ = newClient.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, data, err := newClient.ReadMessage(); err == nil {
		t.Fatalf("late response sent to new socket: %s", data)
	}
	if next.ctx.Err() != nil {
		t.Fatal("late old result cancelled current generation")
	}
}

func TestFeishuGenerationParentCancellationClosesReader(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "test-app"})
	conn, _ := feishuSocketPair(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// This reproduces Disconnect racing with a successful DialContext before
	// the connection has been published to closeConn.
	g := a.startGeneration(ctx, conn)
	defer g.close()
	readDone := make(chan error, 1)
	go func() { _, _, err := conn.ReadMessage(); readDone <- err }()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("cancelled reader returned success")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("cancelled generation retained a blocked socket reader")
	}
}

func TestFeishuIncompleteFragmentsStayWithinMessageBudget(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "test-app"})
	chunk := make([]byte, 600*1024)
	if result := a.combinePayload("large-fragment", 3, 0, chunk); result != nil {
		t.Fatal("incomplete payload returned")
	}
	if result := a.combinePayload("large-fragment", 3, 1, chunk); result != nil {
		t.Fatal("oversize payload returned")
	}
	a.fragmentsMu.Lock()
	_, retained := a.fragments["large-fragment"]
	a.fragmentsMu.Unlock()
	if retained {
		t.Fatal("incomplete fragment exceeded 1 MiB message budget")
	}
}

func TestFeishuOldSocketCannotSubmitBusinessToNewGeneration(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "test-app"})
	received := make(chan struct{}, 1)
	installFeishuReceiver(t, a, func(context.Context, im.IngressEnvelope) im.ReceiveResult {
		received <- struct{}{}
		return im.ReceiveResult{Disposition: im.ReceiveAccepted}
	})
	oldConn, _ := feishuSocketPair(t)
	old := a.startGeneration(t.Context(), oldConn)
	old.close()
	newConn, _ := feishuSocketPair(t)
	next := a.startGeneration(t.Context(), newConn)
	defer next.close()
	err := a.handleDataFrame(context.Background(), oldConn, feishuTextFrame(t, "chat", "stale-business", "stale-delivery"))
	if err == nil {
		t.Fatal("stale socket submitted a business event")
	}
	select {
	case <-received:
		t.Fatal("stale socket reached public acceptance")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestFeishuAcceptanceBudgetReturnsUncertainBeforeLateCommit(t *testing.T) {
	// The elapsed-time assertion is relative to the budget; shrink the budget so
	// the test does not wait out the full production window.
	swapTestValue(t, &im.IngressAcceptBudget, 200*time.Millisecond)
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "test-app"})
	release, accepted := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	installFeishuReceiver(t, a, func(context.Context, im.IngressEnvelope) im.ReceiveResult {
		<-release
		close(accepted)
		return im.ReceiveResult{Disposition: im.ReceiveAccepted}
	})
	conn, client := feishuSocketPair(t)
	g := a.startGeneration(t.Context(), conn)
	defer g.close()
	started := time.Now()
	if err := a.handleDataFrame(g.ctx, conn, feishuTextFrame(t, "chat", "slow-commit", "slow-delivery")); err != nil {
		t.Fatal(err)
	}
	frame, code := readFeishuResponse(t, client)
	if code != 500 || larkws.Headers(frame.Headers).GetString(larkws.HeaderMessageID) != "slow-delivery" {
		t.Fatal("unconfirmed write received success", code)
	}
	if elapsed := time.Since(started); elapsed < im.IngressAcceptBudget/2 || elapsed > im.IngressAcceptBudget+time.Second {
		t.Fatal("acceptance budget not honored", elapsed)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("late result did not settle")
	}
	_ = client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, data, err := client.ReadMessage(); err == nil {
		t.Fatalf("late commit sent another response %s", data)
	}
}
