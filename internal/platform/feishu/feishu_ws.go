// Long-connection transport: dial, ping/pong, frame handling,
// fragment reassembly and connection bookkeeping.
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/keakon/golog/log"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/keakon/chord-gateway/internal/im"
)

func (a *FeishuAdapter) runLongConnection(ctx context.Context) error {
	connURL, err := a.getLongConnURL(ctx)
	if err != nil {
		return err
	}

	u, err := url.Parse(connURL)
	if err != nil {
		return fmt.Errorf("feishu long connection parse URL: %w", err)
	}
	serviceID, _ := strconv.ParseInt(u.Query().Get(larkws.ServiceID), 10, 32)

	dialer := *websocket.DefaultDialer
	dialer.Proxy = im.WebsocketProxyFunc(a.proxyFunc)
	conn, resp, err := dialer.DialContext(ctx, connURL, nil)
	if err != nil {
		if resp != nil {
			return parseFeishuWSError(resp)
		}
		return fmt.Errorf("feishu long connection dial failed")
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)

	generation := a.startGeneration(ctx, conn)
	defer a.clearConn(conn)
	defer generation.close()
	if generation.ctx.Err() != nil {
		return nil
	}

	log.Infof("feishu adapter: long connection established")
	if a.host != nil {
		fc := a.feishuConfig()
		if fc == nil || !a.matchesActiveAccount(fc.AppID) {
			return fmt.Errorf("feishu: account identity changed during connection")
		}
		if err := a.host.PlatformReadySimple("feishu", fc.AppID); err != nil {
			return err
		}
		defer a.host.PlatformUnavailable("feishu")
	}

	pingErrCh := make(chan error, 1)
	generation.wg.Go(func() { a.pingLoop(generation.ctx, conn, int32(serviceID), pingErrCh) })

	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			select {
			case pingErr := <-pingErrCh:
				if pingErr != nil {
					return pingErr
				}
			default:
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("feishu long connection read: %w", err)
		}
		if mt != websocket.BinaryMessage {
			log.Warnf("feishu: ignoring non-binary websocket message message_type=%v", mt)
			continue
		}
		if err := a.handleWSFrame(generation.ctx, conn, msg); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func (a *FeishuAdapter) getLongConnURL(ctx context.Context) (string, error) {
	fc := a.feishuConfig()
	if fc == nil || !a.matchesActiveAccount(fc.AppID) {
		return "", fmt.Errorf("feishu: account config missing or changed")
	}
	reqBody := map[string]string{
		"AppID":     fc.AppID,
		"AppSecret": fc.AppSecret,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("feishu long connection marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, OpenBaseURL+larkws.GenEndpointUri, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("feishu long connection create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("locale", "zh")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("feishu long connection HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", larkws.NewServerError(resp.StatusCode, "system busy")
	}

	var endpointResp larkws.EndpointResp
	if err := json.NewDecoder(resp.Body).Decode(&endpointResp); err != nil {
		return "", fmt.Errorf("feishu long connection decode endpoint response: %w", err)
	}

	switch endpointResp.Code {
	case larkws.OK:
	case larkws.SystemBusy:
		return "", larkws.NewServerError(endpointResp.Code, "system busy")
	case larkws.InternalError:
		return "", larkws.NewServerError(endpointResp.Code, "endpoint request failed")
	default:
		return "", larkws.NewClientError(endpointResp.Code, "endpoint request rejected")
	}

	if endpointResp.Data == nil || endpointResp.Data.Url == "" {
		return "", larkws.NewServerError(http.StatusInternalServerError, "endpoint is null")
	}
	if endpointResp.Data.ClientConfig != nil {
		a.applyWSClientConfig(endpointResp.Data.ClientConfig)
	}
	return endpointResp.Data.Url, nil
}

func (a *FeishuAdapter) applyWSClientConfig(conf *larkws.ClientConfig) {
	if conf == nil {
		return
	}
	a.intervalMu.Lock()
	defer a.intervalMu.Unlock()
	if conf.PingInterval > 0 {
		a.pingInterval = time.Duration(conf.PingInterval) * time.Second
	}
	if conf.ReconnectInterval > 0 {
		a.reconnectInterval = time.Duration(conf.ReconnectInterval) * time.Second
	}
}

func (a *FeishuAdapter) currentPingInterval() time.Duration {
	a.intervalMu.RLock()
	defer a.intervalMu.RUnlock()
	return a.pingInterval
}

func (a *FeishuAdapter) currentReconnectInterval() time.Duration {
	a.intervalMu.RLock()
	defer a.intervalMu.RUnlock()
	return a.reconnectInterval
}

func parseFeishuWSError(resp *http.Response) error {
	code, _ := strconv.Atoi(resp.Header.Get(larkws.HeaderHandshakeStatus))
	msg := "connection handshake rejected"
	switch code {
	case larkws.AuthFailed:
		authCode, _ := strconv.Atoi(resp.Header.Get(larkws.HeaderHandshakeAuthErrCode))
		if authCode == larkws.ExceedConnLimit {
			return larkws.NewClientError(code, msg)
		}
		return larkws.NewServerError(code, msg)
	case larkws.Forbidden:
		return larkws.NewClientError(code, msg)
	default:
		if code != 0 {
			return larkws.NewServerError(code, msg)
		}
		return larkws.NewServerError(resp.StatusCode, resp.Status)
	}
}

func (a *FeishuAdapter) pingLoop(ctx context.Context, conn *websocket.Conn, serviceID int32, errCh chan<- error) {
	for {
		interval := a.currentPingInterval()
		if interval <= 0 {
			interval = feishuDefaultPing
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		frame := larkws.NewPingFrame(serviceID)
		bs, err := frame.Marshal()
		if err != nil {
			continue
		}
		if err := a.writeBinary(conn, bs); err != nil {
			select {
			case errCh <- fmt.Errorf("feishu long connection ping: %w", err):
			default:
			}
			_ = conn.Close()
			return
		}
	}
}

func (a *FeishuAdapter) handleWSFrame(ctx context.Context, conn *websocket.Conn, msg []byte) error {
	var frame larkws.Frame
	if err := frame.Unmarshal(msg); err != nil {
		return fmt.Errorf("feishu long connection unmarshal frame: %w", err)
	}

	switch larkws.FrameType(frame.Method) {
	case larkws.FrameTypeControl:
		a.handleControlFrame(frame)
		return nil
	case larkws.FrameTypeData:
		return a.handleDataFrame(ctx, conn, frame)
	default:
		return nil
	}
}

func (a *FeishuAdapter) handleControlFrame(frame larkws.Frame) {
	hs := larkws.Headers(frame.Headers)
	switch larkws.MessageType(hs.GetString(larkws.HeaderType)) {
	case larkws.MessageTypePong:
		if len(frame.Payload) == 0 {
			return
		}
		var conf larkws.ClientConfig
		if err := json.Unmarshal(frame.Payload, &conf); err != nil {
			log.Warnf("feishu: unmarshal client config failed error=%v", err)
			return
		}
		a.applyWSClientConfig(&conf)
	}
}

func (a *FeishuAdapter) handleDataFrame(ctx context.Context, conn *websocket.Conn, frame larkws.Frame) error {
	hs := larkws.Headers(frame.Headers)
	sum := hs.GetInt(larkws.HeaderSum)
	seq := hs.GetInt(larkws.HeaderSeq)
	msgID := hs.GetString(larkws.HeaderMessageID)
	_ = hs.GetString(larkws.HeaderTraceID)
	msgType := hs.GetString(larkws.HeaderType)

	payload := frame.Payload
	if sum > 1 {
		payload = a.combinePayload(msgID, sum, seq, payload)
		if payload == nil {
			return nil
		}
	}

	switch larkws.MessageType(msgType) {
	case larkws.MessageTypeEvent, larkws.MessageTypeCard:
		return a.submitDataFrame(ctx, conn, frame, payload)
	default:
		return nil
	}
}

func (a *FeishuAdapter) combinePayload(msgID string, sum, seq int, chunk []byte) []byte {
	if msgID == "" || sum < 1 || sum > 128 || seq < 0 || seq >= sum || len(chunk) > 1<<20 {
		return nil
	}
	now := time.Now()
	a.fragmentsMu.Lock()
	defer a.fragmentsMu.Unlock()

	for key, buf := range a.fragments {
		if now.After(buf.expiresAt) {
			delete(a.fragments, key)
		}
	}

	buf, ok := a.fragments[msgID]
	if !ok || len(buf.parts) != sum {
		if !ok && len(a.fragments) >= 128 {
			return nil
		}
		buf = feishuFragmentBuffer{
			parts:     make([][]byte, sum),
			expiresAt: now.Add(feishuFragmentTTL),
		}
	}
	// Count incomplete buffers too: waiting for a missing part must not let
	// retained chunks exceed the complete-message budget.
	total := len(chunk)
	for index, part := range buf.parts {
		if index != seq {
			total += len(part)
		}
	}
	if total > 1<<20 {
		delete(a.fragments, msgID)
		return nil
	}
	buf.parts[seq] = chunk
	buf.expiresAt = now.Add(feishuFragmentTTL)
	a.fragments[msgID] = buf

	for _, part := range buf.parts {
		if len(part) == 0 {
			return nil
		}
	}

	payload := make([]byte, 0, total)
	for _, part := range buf.parts {
		payload = append(payload, part...)
	}
	delete(a.fragments, msgID)
	return payload
}

func (a *FeishuAdapter) writeBinary(conn *websocket.Conn, data []byte) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.connMu.Lock()
	stale := a.conn != nil && a.conn != conn
	a.connMu.Unlock()
	if stale {
		return fmt.Errorf("feishu: stale connection generation")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(feishuWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.BinaryMessage, data)
}

func (a *FeishuAdapter) clearConn(conn *websocket.Conn) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if conn == nil || a.conn == conn {
		a.conn = nil
		a.generation = nil
	}
}

func (a *FeishuAdapter) closeConn() {
	a.connMu.Lock()
	conn, generation := a.conn, a.generation
	a.connMu.Unlock()
	if generation != nil {
		generation.cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
}
