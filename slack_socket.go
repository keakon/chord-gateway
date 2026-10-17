package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/keakon/golog/log"
)

const (
	slackACKCapacity = 128
	// Slack sends a "warning" or "refresh_requested" disconnect when it wants
	// the client to reconnect promptly; a full backoff would waste that window.
	slackImmediateReconnectDelay = 250 * time.Millisecond
	// Slack drops an envelope that is not acknowledged within three seconds of
	// receiving it, so the ACK write is clamped to that deadline.
	slackEnvelopeACKDeadline = 3 * time.Second
	// Only a slash command that accepts a response payload waits this long for
	// an admission hint before sending a bare ACK.
	slackAcceptanceBudget = 2 * time.Second
)

type slackSocketFrame struct {
	Type                   string          `json:"type"`
	EnvelopeID             string          `json:"envelope_id"`
	AcceptsResponsePayload bool            `json:"accepts_response_payload"`
	Payload                json.RawMessage `json:"payload"`
	Reason                 string          `json:"reason"`
	ConnectionInfo         struct {
		AppID string `json:"app_id"`
	} `json:"connection_info"`
}
type slackACK struct {
	Generation uint64
	Deadline   time.Time
	Data       []byte
	Done       chan error
}
type slackReceipt struct {
	Frame      slackSocketFrame
	ReceivedAt time.Time
	Future     <-chan ReceiveResult
}

func validSlackSocketURL(address string) bool {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "wss" || u.User != nil || u.Fragment != "" || u.Port() != "" && u.Port() != "443" {
		return false
	}
	// Only official Socket Mode hosts. The short-lived ticket stays in memory.
	host := strings.ToLower(u.Hostname())
	return host == "wss.slack.com" || host == "wss-primary.slack.com" || host == "wss-backup.slack.com"
}
func dialSlackSocketWithProxy(ctx context.Context, address string, proxyFunc func(*http.Request) (*url.URL, error)) (*websocket.Conn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, WriteBufferSize: 32 << 10}
	dialer.Proxy = websocketProxyFunc(proxyFunc)
	conn, response, err := dialer.DialContext(ctx, address, http.Header{"Origin": []string{"https://api.slack.com"}})
	if response != nil && err != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, slackConnectionError{"socket_dial_failed", false}
	}
	return conn, nil
}

// runSlackSocket joins every worker before a subsequent generation can dial.
// The reader never waits for an API query, storage transaction or ACK writer.
func (a *SlackAdapter) runSlackSocket(parent context.Context, identity slackIdentity) error {
	openCtx, stopOpen := context.WithTimeout(parent, 15*time.Second)
	address, err := a.openSocket(openCtx)
	stopOpen()
	if err != nil {
		return err
	}
	conn, err := a.dialSocket(parent, address)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	a.mu.Lock()
	a.generation++
	generation := a.generation
	a.conn = conn
	a.ready = false
	a.connectionStable = false
	a.readySince = time.Time{}
	a.mu.Unlock()
	conn.SetReadLimit(1 << 20)
	hello := make(chan struct{}, 1)
	failures := make(chan error, 1)
	acks := make(chan slackACK, slackACKCapacity)
	receipts := make(chan slackReceipt, slackACKCapacity)
	var workers sync.WaitGroup
	var lastPing atomic.Int64
	lastPing.Store(time.Now().UnixNano())
	fail := func(err error) {
		a.setUnavailable()
		select {
		case failures <- err:
		default:
		}
		cancel()
		_ = conn.Close()
	}
	defer func() {
		a.setUnavailable()
		cancel()
		_ = conn.Close()
		workers.Wait()
		a.mu.Lock()
		if a.conn == conn {
			a.conn = nil
		}
		a.mu.Unlock()
	}()
	conn.SetPingHandler(func(payload string) error {
		lastPing.Store(time.Now().UnixNano())
		if err := conn.WriteControl(websocket.PongMessage, []byte(payload), time.Now().Add(time.Second)); err != nil {
			fail(slackConnectionError{"pong_failed", false})
			return err
		}
		return nil
	})
	workers.Go(func() { <-ctx.Done(); _ = conn.Close() })
	workers.Go(func() { a.slackACKWriter(ctx, conn, generation, acks, fail) })
	workers.Go(func() {
		interval := max(time.Millisecond, min(time.Second, a.pingTimeout/4))
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastPing.Load())) >= a.pingTimeout {
					fail(slackConnectionError{"ping_timeout", false})
					return
				}
			}
		}
	})
	for range 8 {
		workers.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case receipt := <-receipts:
					a.slackReceiptACK(ctx, generation, receipt, acks)
				}
			}
		})
	}
	inbound := a.inboundFor(identity)
	var capacityLogged atomic.Bool
	workers.Go(func() {
		seenHello := false
		for {
			_, raw, readErr := conn.ReadMessage()
			if readErr != nil {
				if ctx.Err() == nil {
					fail(slackConnectionError{"socket_read_failed", false})
				}
				return
			}
			receivedAt := time.Now()
			// Any inbound frame proves liveness; Slack does not guarantee a
			// WebSocket ping interval, so hello/envelopes refresh the deadline.
			lastPing.Store(receivedAt.UnixNano())
			var frame slackSocketFrame
			if json.Unmarshal(raw, &frame) != nil {
				fail(slackConnectionError{"malformed_socket_frame", false})
				return
			}
			switch frame.Type {
			case "hello":
				if seenHello || frame.ConnectionInfo.AppID != identity.AppID {
					fail(slackConnectionError{"hello_identity_mismatch", true})
					return
				}
				seenHello = true
				hello <- struct{}{}
			case "disconnect":
				fail(slackConnectionError{"socket_" + slackDisconnectCategory(frame.Reason), frame.Reason == "link_disabled"})
				return
			default:
				if frame.EnvelopeID == "" {
					continue
				}
				a.mu.Lock()
				ready := a.ready && a.generation == generation
				a.mu.Unlock()
				if !ready {
					// Slack redelivers unacked envelopes, so dropping pre-ready
					// frames is safe; count them so a stuck ready gate is visible.
					if a.router != nil {
						a.router.ObserveIngress("slack", "lost_before_accept", "platform_unavailable")
					}
					continue
				}
				if frame.Type != "events_api" && frame.Type != "slash_commands" {
					a.queueSlackACK(ctx, generation, frame, receivedAt, "", acks)
					continue
				}
				if len(receipts) == cap(receipts) {
					// Same contract as Telegram's stuck update: never block the
					// reader on a full queue; Slack retries the unacked envelope.
					if a.router != nil {
						a.router.ObserveIngress("slack", "lost_before_accept", "capacity")
					}
					if capacityLogged.CompareAndSwap(false, true) {
						log.Warnf("slack: intake capacity reached; leaving envelopes unacknowledged for Slack to redeliver")
					}
					continue
				}
				future, cleanup, submitErr := inbound.submit(ctx, frame.Type, frame.Payload, frame.EnvelopeID, receivedAt)
				if submitErr != nil {
					if cleanup != nil {
						cleanup()
					}
					switch {
					case errors.Is(submitErr, errSlackIgnored):
						a.queueSlackACK(ctx, generation, frame, receivedAt, "", acks)
					case errors.Is(submitErr, errSlackCaptureBusy):
						// The reader must not block on a held snapshot lock; Slack
						// redelivers the unacknowledged envelope.
						if a.router != nil {
							a.router.ObserveIngress("slack", "lost_before_accept", "capture_busy")
						}
					}
					continue
				}
				// events_api must be ACKed immediately (within 3s) before slow
				// conversation lookups finish; only slash commands with a response
				// payload wait briefly for an acceptance hint.
				if frame.Type == "events_api" || !frame.AcceptsResponsePayload {
					a.queueSlackACK(ctx, generation, frame, receivedAt, "", acks)
					workers.Go(func() {
						defer cleanup()
						for range future {
						}
					})
					continue
				}
				// Join the authoritative admission even when an ACK wait expires.
				// Cancellation cannot establish whether in-flight acceptance completed.
				relay := make(chan ReceiveResult, 1)
				workers.Go(func() {
					defer cleanup()
					result, ok := <-future
					if ok {
						relay <- result
					}
					close(relay)
				})
				pending := slackReceipt{frame, receivedAt, relay}
				select {
				case receipts <- pending:
				default:
					cleanup()
					// Same observability as the reader-side capacity discard:
					// an unacknowledged envelope is a lost intake.
					if a.router != nil {
						a.router.ObserveIngress("slack", "lost_before_accept", "capacity")
					}
				}
			}
		}
	})
	timer := time.NewTimer(a.helloTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return slackSocketResult(parent, failures)
	case <-timer.C:
		return slackConnectionError{"hello_timeout", false}
	case <-hello:
	}
	// Recheck after hello while the reader continues serving pings. A changed
	// bot identity must never publish readiness for old account leases.
	verified, err := a.verifyIdentity(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return slackSocketResult(parent, failures)
		}
		return err
	}
	if !verified.sameInstallation(identity) {
		if a.router != nil {
			_ = a.router.PlatformReady("slack", verified.AccountID, PlatformPresentation{Platform: "slack", BotUsername: verified.BotName})
		}
		return slackConnectionError{"identity_changed", true}
	}
	if err = a.router.PrepareIngressAccount(ctx, "slack", identity.AccountID); err != nil {
		if ctx.Err() != nil {
			return slackSocketResult(parent, failures)
		}
		return slackConnectionError{"ingress_preparation_failed", !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errIngressCapacity)}
	}
	if ctx.Err() != nil {
		return slackSocketResult(parent, failures)
	}
	if err = a.router.PlatformReady("slack", identity.AccountID, PlatformPresentation{Platform: "slack", BotUsername: verified.BotName}); err != nil {
		return slackConnectionError{"pairing_unavailable", true}
	}
	a.mu.Lock()
	if ctx.Err() == nil && !a.stopping {
		a.ready = true
		a.readySince = time.Now()
	}
	a.mu.Unlock()
	if ctx.Err() != nil {
		a.setUnavailable()
	}
	a.wakeIngressReadiness()
	<-ctx.Done()
	return slackSocketResult(parent, failures)
}

func slackSocketResult(parent context.Context, failures <-chan error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	select {
	case err := <-failures:
		return err
	default:
		return slackConnectionError{"socket_unavailable", false}
	}
}
func slackDisconnectCategory(reason string) string {
	switch reason {
	case "warning", "refresh_requested", "link_disabled":
		return reason
	}
	return "disconnect"
}

func (a *SlackAdapter) slackReceiptACK(ctx context.Context, generation uint64, receipt slackReceipt, acks chan<- slackACK) {
	budget := slackAcceptanceBudget
	waitCtx, cancel := context.WithDeadline(ctx, receipt.ReceivedAt.Add(budget))
	defer cancel()
	result := WaitIngressNormalization(waitCtx, receipt.Future)
	switch result.Disposition {
	case ReceiveAccepted, ReceiveDuplicate, ReceiveRejected:
		done := a.queueSlackACK(ctx, generation, receipt.Frame, receipt.ReceivedAt, slackAcceptanceHint(result), acks)
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
			case <-waitCtx.Done():
			}
		}
	default:
		if a.router != nil {
			a.router.ObserveIngress("slack", "acceptance_unconfirmed", result.Reason)
		}

	}
}

// Transport receipt and command execution are separate facts. Redelivery must
// retain the original outcome, especially controls abandoned during acceptance.
// Only non-accepted outcomes carry an ephemeral hint; a plain acceptance stays
// a bare ACK so every /chord invocation does not push visible chatter.
func slackAcceptanceHint(result ReceiveResult) string {
	if result.Disposition == ReceiveRejected || result.State == "rejected" {
		return ""
	}
	if result.Disposition != ReceiveAccepted && result.Disposition != ReceiveDuplicate {
		return ""
	}
	switch result.State {
	case "expired":
		if result.Reason == "execution_outcome_unknown" {
			return "Outcome unknown. Check the original session and /chord status before retrying."
		}
		return "Expired without execution. Send a new command if still needed."
	case "unknown", "dispatching":
		return "Outcome unconfirmed. Check /chord status before retrying."
	case "quarantined":
		return "Quarantined; execution is paused. Check /chord status."
	default:
		return ""
	}
}

func (a *SlackAdapter) queueSlackACK(ctx context.Context, generation uint64, frame slackSocketFrame, receivedAt time.Time, hint string, queue chan<- slackACK) <-chan error {
	ack := struct {
		EnvelopeID string `json:"envelope_id"`
		Payload    any    `json:"payload,omitempty"`
	}{EnvelopeID: frame.EnvelopeID}
	if frame.Type == "slash_commands" && frame.AcceptsResponsePayload && hint != "" {
		ack.Payload = map[string]string{"response_type": "ephemeral", "text": hint}
	}
	data, err := json.Marshal(ack)
	if err != nil {
		return nil
	}
	if len(data) >= 2048 && ack.Payload != nil {
		// An oversized hint must never leave the envelope unacknowledged.
		// Fall back to a bare ACK and let Slack redeliver the original outcome.
		ack.Payload = nil
		data, err = json.Marshal(ack)
		if err != nil {
			return nil
		}
	}
	if len(data) >= 2048 {
		return nil
	}
	request := slackACK{generation, receivedAt.Add(slackEnvelopeACKDeadline), data, make(chan error, 1)}
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	select {
	case queue <- request:
		return request.Done
	default:
		if a.router != nil {
			a.router.ObserveIngress("slack", "lost_before_accept", "capacity")
		}
		return nil
	}
}
func (a *SlackAdapter) slackACKWriter(ctx context.Context, conn *websocket.Conn, generation uint64, queue <-chan slackACK, fail func(error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-queue:
			a.mu.Lock()
			current := a.generation == generation && a.conn == conn && !a.stopping
			a.mu.Unlock()
			if ctx.Err() != nil || !current || request.Generation != generation {
				request.Done <- context.Canceled
				continue
			}
			deadline := minTime(request.Deadline, time.Now().Add(time.Second))
			if !time.Now().Before(deadline) {
				request.Done <- context.DeadlineExceeded
				continue
			}
			_ = conn.SetWriteDeadline(deadline)
			if err := conn.WriteMessage(websocket.TextMessage, request.Data); err != nil {
				request.Done <- slackConnectionError{"ack_write_failed", false}
				fail(slackConnectionError{"ack_write_failed", false})
				return
			}
			request.Done <- nil
		}
	}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
