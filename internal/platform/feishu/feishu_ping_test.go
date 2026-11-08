package feishu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// pingLoop emits a binary ping frame on the configured interval and tears the
// connection down through the error channel once a write fails.
func TestFeishuPingLoopSendsFramesAndFailsOnClosedConn(t *testing.T) {
	received := make(chan []byte, 1)
	serverConnCh := make(chan *websocket.Conn, 1)
	upgrader := &websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		serverConnCh <- conn
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				select {
				case received <- msg:
				default:
				}
			}
		}
	}))
	defer server.Close()

	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial("ws://"+server.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	a := &FeishuAdapter{pingInterval: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.pingLoop(ctx, conn, 7, errCh)
	}()

	select {
	case msg := <-received:
		var frame larkws.Frame
		if err := frame.Unmarshal(msg); err != nil {
			t.Fatalf("ping frame unmarshal: %v", err)
		}
		if frame.Method != int32(larkws.FrameTypeControl) || frame.Service != 7 {
			t.Fatalf("ping frame = method %d service %d, want control/7", frame.Method, frame.Service)
		}
		if string(larkws.Headers(frame.Headers).GetString(larkws.HeaderType)) != string(larkws.MessageTypePing) {
			t.Fatalf("ping frame type header = %v, want ping", frame.Headers)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no ping frame arrived")
	}

	// Closing the server side makes the next ping write fail; the loop must
	// surface the error and stop instead of spinning.
	select {
	case srvConn := <-serverConnCh:
		_ = srvConn.Close()
	case <-time.After(time.Second):
		t.Fatal("server handler did not start")
	}
	select {
	case err := <-errCh:
		if !strings.Contains(err.Error(), "ping") {
			t.Fatalf("ping error = %v, want a ping write failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pingLoop kept running on a failed write")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pingLoop did not return after the write failure")
	}
}
