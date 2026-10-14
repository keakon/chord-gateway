package gateway

import (
	"fmt"
	"time"

	"github.com/disgoorg/json/v2"
	"github.com/gorilla/websocket"
)

// ReadLimit accommodates large READY messages while bounding allocations.
// This is a client resource limit, not Discord's limit for outgoing payloads.
const ReadLimit = 16 << 20

type transport interface {
	ReceiveMessage() (*Message, error)
	WriteMessage(Message) error
	Close() error
}
type jsonTransport struct{ conn *websocket.Conn }

func (t *jsonTransport) ReceiveMessage() (*Message, error) {
	kind, data, err := t.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if kind != websocket.TextMessage {
		return nil, fmt.Errorf("unsupported gateway message type %d", kind)
	}
	return decodeGatewayMessage(data)
}

func decodeGatewayMessage(data []byte) (*Message, error) {
	var message Message
	if err := json.Unmarshal(data, &message); err != nil {
		// Upstream errors include raw event bodies. Never propagate those.
		return nil, fmt.Errorf("invalid gateway message")
	}
	return &message, nil
}
func (t *jsonTransport) WriteMessage(message Message) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if err = t.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return t.conn.WriteMessage(websocket.TextMessage, data)
}
func (t *jsonTransport) Close() error { return t.conn.Close() }
