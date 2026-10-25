package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func TestFeishuSocketMixedTextAndCardPreservesChatOrder(t *testing.T) {
	a := testFeishuAdapter(t, &config.FeishuConfig{AppID: "test-app", Owner: &config.OwnerIdentity{AccountID: "test-app", UserID: "owner"}, ChatBindings: map[string]string{"chat": "test", "other": "test"}})
	firstEntered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	accepted := make(chan string, 3)
	installFeishuReceiver(t, a, func(ctx context.Context, e im.IngressEnvelope) im.ReceiveResult {
		if e.Message.ChatID == "chat" {
			if e.Message.MessageID == "message:first" {
				close(firstEntered)
				select {
				case <-release:
				case <-ctx.Done():
					return im.ReceiveResult{Disposition: im.ReceiveUncertain}
				}
			}
			accepted <- e.Message.MessageID
		}
		return im.ReceiveResult{Disposition: im.ReceiveAccepted, State: "received"}
	})
	conn, client := feishuSocketPair(t)
	generation := a.StartGenerationForTest(t.Context(), conn)
	defer generation.CloseGenerationForTest()
	readerDone := make(chan error, 1)
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err == nil {
				err = a.HandleWSFrameForTest(generation.GenerationContextForTest(), conn, raw)
			}
			if err != nil {
				readerDone <- err
				return
			}
		}
	}()
	sendFrame := func(frame larkws.Frame) {
		t.Helper()
		raw, err := frame.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := client.WriteMessage(websocket.BinaryMessage, raw); err != nil {
			t.Fatal(err)
		}
	}
	sendFrame(feishuTextFrame(t, "chat", "first", "delivery-first"))
	waitFeishuLifecycle(t, firstEntered)
	card := makeFeishuCardActionEvent("owner", "chat", "request", "allow")
	card.EventV2Base.Header.EventID = "middle"
	card.EventV2Base.Header.AppID = "test-app"
	card.EventV2Base.Header.EventType = "card.action.trigger"
	payload, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(larkws.Frame{Method: int32(larkws.FrameTypeData), Headers: larkws.Headers{{Key: larkws.HeaderType, Value: string(larkws.MessageTypeCard)}, {Key: larkws.HeaderMessageID, Value: "delivery-card"}, {Key: larkws.HeaderSum, Value: "1"}}, Payload: payload})
	sendFrame(feishuTextFrame(t, "chat", "last", "delivery-last"))
	sendFrame(feishuTextFrame(t, "other", "independent", "delivery-other"))

	// The only completed response must belong to the independent chat. This
	// proves the socket reader submitted both kinds without waiting for the
	// first chat's blocked acceptance, and neither kind bypassed its FIFO.
	frame, code := readFeishuResponse(t, client)
	if code != 200 || larkws.Headers(frame.Headers).GetString(larkws.HeaderMessageID) != "delivery-other" {
		t.Fatalf("blocked chat was overtaken or reader stalled: code=%d delivery=%s", code, larkws.Headers(frame.Headers).GetString(larkws.HeaderMessageID))
	}
	select {
	case premature := <-accepted:
		t.Fatalf("same-chat operation overtook blocked first text: %s", premature)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	var order []string
	for range 3 {
		order = append(order, waitFeishuLifecycle(t, accepted))
	}
	if !reflect.DeepEqual(order, []string{"message:first", "card:middle", "message:last"}) {
		t.Fatal("text and card used different acceptance order", order)
	}
	seen := make(map[string]bool)
	for range 3 {
		frame, code := readFeishuResponse(t, client)
		delivery := larkws.Headers(frame.Headers).GetString(larkws.HeaderMessageID)
		if code != 200 || seen[delivery] {
			t.Fatalf("missing or duplicated successful delivery response: %d %s", code, delivery)
		}
		seen[delivery] = true
	}
	for _, delivery := range []string{"delivery-first", "delivery-card", "delivery-last"} {
		if !seen[delivery] {
			t.Fatal("response not bound to original delivery", delivery)
		}
	}
	generation.CloseGenerationForTest()
	select {
	case err := <-readerDone:
		if err == nil {
			t.Fatal(errors.New("reader exited without its closed socket"))
		}
	case <-time.After(time.Second):
		t.Fatal("socket reader survived generation close")
	}
}
