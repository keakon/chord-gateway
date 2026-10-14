package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/keakon/chord-gateway/config"
)

type feishuReceiverFunc func(context.Context, IngressEnvelope) ReceiveResult

func (f feishuReceiverFunc) Accept(ctx context.Context, e IngressEnvelope) ReceiveResult {
	return f(ctx, e)
}

func feishuSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	ready := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		ready <- conn
	}))
	t.Cleanup(server.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn := <-ready
	t.Cleanup(func() { _ = conn.Close(); _ = client.Close() })
	return conn, client
}
func feishuTextFrame(t *testing.T, chat, business, delivery string) larkws.Frame {
	t.Helper()
	event := makeFeishuMessageEvent("owner", chat, business, "sample task")
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	raw["header"] = map[string]string{"event_type": "im.message.receive_v1", "app_id": "test-app"}
	payload, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return larkws.Frame{Method: int32(larkws.FrameTypeData), Headers: larkws.Headers{{Key: larkws.HeaderType, Value: string(larkws.MessageTypeEvent)}, {Key: larkws.HeaderMessageID, Value: delivery}, {Key: larkws.HeaderSum, Value: "1"}}, Payload: payload}
}
func installFeishuReceiver(t *testing.T, a *FeishuAdapter, f feishuReceiverFunc) {
	t.Helper()
	r := a.msgRouter.(*NotificationRouter)
	n := NewIngressNormalizer(t.Context(), f)
	r.ingress.normalizer = n
	t.Cleanup(n.Close)
}
func readFeishuResponse(t *testing.T, client *websocket.Conn) (larkws.Frame, int) {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(4 * time.Second))
	_, raw, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var frame larkws.Frame
	if err = frame.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Code int `json:"code"`
	}
	if err = json.Unmarshal(frame.Payload, &response); err != nil {
		t.Fatal(err)
	}
	return frame, response.Code
}
func TestFeishuFrameReaderDoesNotWaitForAcceptance(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "test-app"})
	entered, release := make(chan struct{}), make(chan struct{})
	installFeishuReceiver(t, a, func(ctx context.Context, e IngressEnvelope) ReceiveResult {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ReceiveResult{Disposition: ReceiveUncertain}
		}
		if e.DeliveryID != "delivery" || e.Message.MessageID != "message:business" {
			t.Errorf("identity %+v", e)
		}
		return ReceiveResult{Disposition: ReceiveAccepted}
	})
	conn, client := feishuSocketPair(t)
	g := a.startGeneration(t.Context(), conn)
	defer g.close()
	frame := feishuTextFrame(t, "chat", "business", "delivery")
	if err := a.handleDataFrame(g.ctx, conn, frame); err != nil {
		t.Fatal(err)
	}
	<-entered
	// A received pong must be handled even while acceptance is blocked.
	pong := larkws.Frame{Method: int32(larkws.FrameTypeControl), Headers: larkws.Headers{{Key: larkws.HeaderType, Value: string(larkws.MessageTypePong)}}, Payload: []byte(`{"PingInterval":7}`)}
	data, err := pong.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.handleWSFrame(g.ctx, conn, data); err != nil {
		t.Fatal(err)
	}
	if a.currentPingInterval() != 7*time.Second {
		t.Fatal("pong was not handled")
	}
	read := make(chan []byte, 1)
	go func() {
		_, data, err := client.ReadMessage()
		if err == nil {
			read <- data
		}
	}()
	select {
	case <-read:
		t.Fatal("success ACK before acceptance")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case data := <-read:
		var got larkws.Frame
		if err := got.Unmarshal(data); err != nil {
			t.Fatal(err)
		}
		if larkws.Headers(got.Headers).GetString(larkws.HeaderMessageID) != "delivery" {
			t.Fatal("wrong delivery")
		}
		var response struct {
			Code int `json:"code"`
		}
		if err = json.Unmarshal(got.Payload, &response); err != nil || response.Code != 200 {
			t.Fatalf("response %s %v", got.Payload, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing committed ACK")
	}
}
func TestFeishuFramesMapUnconfirmedAcceptanceToFailure(t *testing.T) {
	for _, disposition := range []ReceiveDisposition{ReceiveAccepted, ReceiveDuplicate, ReceiveRejected, ReceiveRetryable, ReceiveUncertain} {
		t.Run(string(disposition), func(t *testing.T) {
			a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "test-app"})
			installFeishuReceiver(t, a, func(context.Context, IngressEnvelope) ReceiveResult { return ReceiveResult{Disposition: disposition} })
			conn, client := feishuSocketPair(t)
			g := a.startGeneration(t.Context(), conn)
			defer g.close()
			if err := a.handleDataFrame(g.ctx, conn, feishuTextFrame(t, "chat", "business", "delivery")); err != nil {
				t.Fatal(err)
			}
			_, code := readFeishuResponse(t, client)
			want := 200
			if disposition == ReceiveRetryable || disposition == ReceiveUncertain {
				want = 500
			}
			if code != want {
				t.Fatalf("code %d, want %d", code, want)
			}
		})
	}
}
func TestFeishuStaleGenerationCannotWriteNewSocket(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "test-app"})
	oldConn, _ := feishuSocketPair(t)
	oldGeneration := a.startGeneration(t.Context(), oldConn)
	oldGeneration.close()
	newConn, client := feishuSocketPair(t)
	newGeneration := a.startGeneration(t.Context(), newConn)
	defer newGeneration.close()
	if err := a.writeBinary(oldConn, []byte("stale")); err == nil {
		t.Fatal("old generation permitted")
	}
	frame := feishuTextFrame(t, "chat", "business", "new-delivery")
	if err := a.handleDataFrame(newGeneration.ctx, newConn, frame); err != nil {
		t.Fatal(err)
	}
	got, code := readFeishuResponse(t, client)
	if code != 200 || larkws.Headers(got.Headers).GetString(larkws.HeaderMessageID) != "new-delivery" {
		t.Fatal("new generation response changed")
	}
}
