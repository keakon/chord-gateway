package main

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/internal/im"
)

const telegramTextBudget = 3500

// One instance is shared by all destinations of this bot account. Slots are
// claimed only when ready, so a cancelled waiter never reserves future quota.
type telegramSendLimiter struct {
	mu           sync.Mutex
	nextChat     map[int64]time.Time
	recent       []time.Time
	blockedUntil time.Time
}

func (l *telegramSendLimiter) wait(ctx context.Context, chatID int64) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		l.mu.Lock()
		if l.nextChat == nil {
			l.nextChat = make(map[int64]time.Time)
		}
		count := 0
		for _, sent := range l.recent {
			if now.Sub(sent) < time.Second {
				l.recent[count] = sent
				count++
			}
		}
		l.recent = l.recent[:count]
		ready := l.nextChat[chatID]
		if l.blockedUntil.After(ready) {
			ready = l.blockedUntil
		}
		if len(l.recent) >= 20 && l.recent[0].Add(time.Second).After(ready) {
			ready = l.recent[0].Add(time.Second)
		}
		delay := ready.Sub(now)
		if delay <= 0 {
			for chat, next := range l.nextChat {
				if !next.After(now) {
					delete(l.nextChat, chat)
				}
			}
			spacing := time.Second
			if chatID < 0 {
				spacing = 3 * time.Second
			}
			l.nextChat[chatID] = now.Add(spacing)
			l.recent = append(l.recent, now)
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (l *telegramSendLimiter) pause(delay time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := time.Now().Add(delay); until.After(l.blockedUntil) {
		l.blockedUntil = until
	}
}
func newTelegramTextSender(api telegramAPI) (*im.IMTextSender, error) {
	if api == nil {
		return nil, errors.New("telegram text sender requires API")
	}
	limiter := &telegramSendLimiter{}
	return im.NewIMTextSender(im.IMTextSendOptions{
		Platform: "telegram", Budget: telegramTextBudget, Measure: im.UTF16Units,
		SendSegment: func(ctx context.Context, target, text string) error {
			chat, err := strconv.ParseInt(target, 10, 64)
			if err != nil || chat == 0 {
				return &telegramAPIError{Method: "sendMessage", Category: "invalid_target"}
			}
			if err := limiter.wait(ctx, chat); err != nil {
				return err
			}
			_, err = api.SendMessage(ctx, chat, text)
			var apiErr *telegramAPIError
			if errors.As(err, &apiErr) && apiErr.Category == "rate_limited" && apiErr.RetryAfter > 0 {
				limiter.pause(apiErr.RetryAfter)
				return &im.IMRateLimitError{RetryAfter: apiErr.RetryAfter}
			}
			if apiErr != nil {
				log.Warnf("telegram: sendMessage failed category=%v HTTP=%v API=%v", apiErr.Category, apiErr.Status, apiErr.Code)
				if apiErr.Category == "chat_migrated" {
					log.Warnf("telegram: chat migrated; the owner must bind the bot again in the new group")
				}
			}
			return err
		},
	})
}

func (e *telegramAPIError) IMTextSendFailureCategory() string {
	switch e.Category {
	case "credentials", "poller_conflict", "chat_migrated", "forbidden", "unknown_send", "api_rejected", "invalid_target", "invalid_request":
		return e.Category
	}
	return "delivery_failed"
}
