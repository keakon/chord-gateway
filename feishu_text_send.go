package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func (a *FeishuAdapter) feishuTextSender() (*im.IMTextSender, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.textSender == nil {
		sender, err := im.NewIMTextSender(im.IMTextSendOptions{
			Platform: "feishu",
			Budget:   feishuMaxTextLen,
			SendSegment: func(ctx context.Context, chat, text string) error {
				_, err := a.sendMessageContext(ctx, chat, "text", map[string]string{"text": text}, false)
				return err
			},
		})
		if err != nil {
			return nil, err
		}
		a.textSender = sender
	}
	return a.textSender, nil
}

func (a *FeishuAdapter) TextSendMetrics() im.IMTextMetricsSnapshot {
	a.mu.Lock()
	sender := a.textSender
	a.mu.Unlock()
	if sender == nil {
		return im.IMTextMetricsSnapshot{Platform: "feishu"}
	}
	return sender.TextSendMetrics()
}

func (a *FeishuAdapter) TextSendTargetSnapshot(chat string) im.IMTextTargetSnapshot {
	a.mu.Lock()
	sender := a.textSender
	a.mu.Unlock()
	if sender == nil {
		return im.IMTextTargetSnapshot{}
	}
	return sender.TextSendTargetSnapshot(chat)
}

func isFeishuTextRequest(body any) bool {
	values, ok := body.(map[string]string)
	return ok && values["msg_type"] == "text"
}

func feishuTextRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		// Waiting beyond the output deadline cannot succeed, and capping here
		// prevents hostile or malformed headers from overflowing Duration.
		if seconds >= int64(im.IMTextSendTimeout/time.Second) {
			return im.IMTextSendTimeout
		}
		return time.Duration(seconds) * time.Second
	}
	if next, err := http.ParseTime(value); err == nil && next.After(now) {
		return min(next.Sub(now), im.IMTextSendTimeout)
	}
	return time.Second
}
