package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/keakon/chord-gateway/internal/im"
)

const (
	feishuResponseWorkers  = 8
	feishuResponseCapacity = 128
	feishuWriteTimeout     = 2 * time.Second
)

// A generation owns only protocol replies. Acceptance and business dispatch
// remain in the gateway's shared, bounded ingress workers.
type feishuConnectionGeneration struct {
	ctx       context.Context
	cancel    context.CancelFunc
	conn      *websocket.Conn
	responses chan feishuResponseJob
	wg        sync.WaitGroup
	closed    chan struct{}
}

type feishuResponseJob struct {
	ctx     context.Context
	cancel  context.CancelFunc
	frame   larkws.Frame
	started time.Time
	resolve func(context.Context) (any, error)
}

func (a *FeishuAdapter) startGeneration(ctx context.Context, conn *websocket.Conn) *feishuConnectionGeneration {
	ctx, cancel := context.WithCancel(ctx)
	g := &feishuConnectionGeneration{ctx: ctx, cancel: cancel, conn: conn, responses: make(chan feishuResponseJob, feishuResponseCapacity), closed: make(chan struct{})}
	context.AfterFunc(g.ctx, func() {
		_ = g.conn.Close()
		close(g.closed)
	})
	a.fragmentsMu.Lock()
	a.fragments = make(map[string]feishuFragmentBuffer)
	a.fragmentsMu.Unlock()
	a.connMu.Lock()
	a.conn, a.generation = conn, g
	a.connMu.Unlock()
	for range feishuResponseWorkers {
		g.wg.Go(func() {
			for {
				select {
				case <-g.ctx.Done():
					return
				case job := <-g.responses:
					if g.ctx.Err() == nil {
						rsp, err := job.resolve(job.ctx)
						if g.ctx.Err() == nil {
							if writeErr := a.writeDataResponse(g.conn, job.frame, job.started, rsp, err); writeErr != nil {
								g.cancel()
								_ = g.conn.Close()
							}
						}
					}
					job.cancel()
				}
			}
		})
	}
	return g
}

func (g *feishuConnectionGeneration) close() {
	g.cancel()
	<-g.closed
	g.wg.Wait()
	for {
		select {
		case job := <-g.responses:
			job.cancel()
		default:
			return
		}
	}
}

// submitDataFrame runs on the reader: decode, freeze, submit, and return. A
// response queue overflow drops this frame with a 500, never blocks its heartbeat
// behind storage, kills the generation, or starts an unbounded goroutine
// waiting for acceptance.
func (a *FeishuAdapter) submitDataFrame(ctx context.Context, conn *websocket.Conn, frame larkws.Frame, payload []byte) error {
	a.connMu.Lock()
	g, current := a.generation, a.conn
	a.connMu.Unlock()
	if current != nil && current != conn {
		return errors.New("feishu: stale connection response")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if g != nil && g.conn == conn && g.ctx.Err() != nil {
		return g.ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, ingressAcceptBudget)
	job := feishuResponseJob{ctx: ctx, cancel: cancel, frame: frame, started: time.Now()}
	var header struct {
		Header struct {
			EventType string `json:"event_type"`
		} `json:"header"`
	}
	err := json.Unmarshal(payload, &header)
	var future <-chan im.ReceiveResult
	var immediate *larkcallback.CardActionTriggerResponse
	var card *larkcallback.CardActionTriggerEvent
	if err == nil {
		switch header.Header.EventType {
		case "im.message.receive_v1":
			var event larkim.P2MessageReceiveV1
			if err = json.Unmarshal(payload, &event); err == nil {
				future, err = a.submitMessageEvent(ctx, &event, larkws.Headers(frame.Headers).GetString(larkws.HeaderMessageID))
			}
		case "card.action.trigger":
			card = &larkcallback.CardActionTriggerEvent{}
			if err = json.Unmarshal(payload, card); err == nil {
				future, immediate, err = a.submitCardActionEvent(ctx, card, larkws.Headers(frame.Headers).GetString(larkws.HeaderMessageID))
			}
		}
	}
	job.resolve = func(ctx context.Context) (any, error) {
		if err != nil || future == nil {
			if immediate != nil {
				return immediate, err
			}
			return nil, err
		}
		result := WaitIngressNormalization(ctx, future)
		if card != nil {
			return a.feishuCardIngressResponse(card, result), feishuMessageIngressError(result)
		}
		return nil, feishuMessageIngressError(result)
	}
	if g != nil && g.conn == conn {
		select {
		case <-g.ctx.Done():
			cancel()
			return g.ctx.Err()
		case g.responses <- job:
			return nil
		default:
			// Keep the generation: a burst above 128 in-flight responses is a
			// single dropped delivery attempt, not proof the socket is unhealthy.
			// Answer 500 immediately without waiting for normalization so the
			// reader never blocks; Feishu redelivers and in-memory dedupe
			// suppresses the duplicate once workers drain.
			defer cancel()
			if r, ok := a.msgRouter.(*NotificationRouter); ok {
				r.ObserveIngress("feishu", "lost_before_accept", "capacity")
			}
			if err := a.writeDataResponse(conn, frame, job.started, nil, errIngressCapacity); err != nil {
				return err
			}
			return nil
		}
	}
	defer cancel()
	if current != nil && current != conn {
		return errors.New("feishu: stale connection response")
	}
	// The same entry point also supports isolated protocol exchanges. It never
	// creates an alternate receiver or bypasses in-memory acceptance.
	rsp, resolveErr := job.resolve(ctx)
	return a.writeDataResponse(conn, frame, job.started, rsp, resolveErr)
}

func (a *FeishuAdapter) writeDataResponse(conn *websocket.Conn, frame larkws.Frame, started time.Time, rsp any, handlerErr error) error {
	hs := larkws.Headers(frame.Headers)
	hs.Add(larkws.HeaderBizRt, strconv.FormatInt(time.Since(started).Milliseconds(), 10))
	resp := larkws.NewResponseByCode(http.StatusOK)
	if handlerErr != nil {
		resp = larkws.NewResponseByCode(http.StatusInternalServerError)
	}
	if rsp != nil {
		data, err := json.Marshal(rsp)
		if err != nil {
			resp = larkws.NewResponseByCode(http.StatusInternalServerError)
		} else {
			resp.Data = data
		}
	}
	var err error
	frame.Payload, err = json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("feishu: marshal response: %w", err)
	}
	frame.Headers = hs
	data, err := frame.Marshal()
	if err != nil {
		return fmt.Errorf("feishu: marshal response frame: %w", err)
	}
	if err := a.writeBinary(conn, data); err != nil {
		return fmt.Errorf("feishu: write response: %w", err)
	}
	return nil
}

func (a *FeishuAdapter) prepareIngressAccount(ctx context.Context, r *NotificationRouter, account string) error {
	return r.PrepareIngressAccount(ctx, "feishu", account)
}
