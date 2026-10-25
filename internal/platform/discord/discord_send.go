package discord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/keakon/chord-gateway/internal/im"
)

type SendOutcomeKey struct{}

// SendOutcome tracks only the current HTTP attempt. A request can have been
// accepted remotely even when its caller stops waiting for a receipt.
type SendOutcome struct {
	mu      sync.Mutex
	attempt *discordSendAttempt
	Unknown atomic.Bool
	Status  atomic.Int32
}

type discordSendAttempt struct{ responseSeen bool }

func (outcome *SendOutcome) beginAttempt() *discordSendAttempt {
	outcome.mu.Lock()
	defer outcome.mu.Unlock()
	attempt := &discordSendAttempt{}
	outcome.attempt = attempt
	outcome.Status.Store(0)
	outcome.Unknown.Store(false)
	return attempt
}

func (outcome *SendOutcome) wroteAttempt(attempt *discordSendAttempt) {
	outcome.mu.Lock()
	defer outcome.mu.Unlock()
	// A server may reject headers before the request writer finishes. Its
	// explicit response wins over a late write callback, as does a newer retry.
	if outcome.attempt != attempt || attempt.responseSeen {
		return
	}
	outcome.Unknown.Store(true)
}

func (outcome *SendOutcome) responseAttempt(attempt *discordSendAttempt, status int) {
	outcome.mu.Lock()
	defer outcome.mu.Unlock()
	if attempt != nil && outcome.attempt != attempt {
		return
	}
	if outcome.attempt != nil {
		outcome.attempt.responseSeen = true
	}
	outcome.Status.Store(int32(status))
	knownRejection := status == http.StatusTooManyRequests || status >= 400 && status < 500 && status != http.StatusRequestTimeout
	outcome.Unknown.Store(!knownRejection)
}

func (outcome *SendOutcome) Accepted() {
	outcome.mu.Lock()
	defer outcome.mu.Unlock()
	if outcome.attempt != nil {
		outcome.attempt.responseSeen = true
	}
	outcome.Unknown.Store(false)
}

func discordResponseOutcome(ctx context.Context, status int) {
	if outcome, ok := ctx.Value(SendOutcomeKey{}).(*SendOutcome); ok {
		outcome.responseAttempt(nil, status)
	}
}

func NewDiscordTextSender(send func(context.Context, string, string) error) (*im.IMTextSender, error) {
	return im.NewIMTextSender(im.IMTextSendOptions{
		Platform: "discord", Budget: 1900, Render: EscapeMarkdown, Measure: im.UTF16Units, SendSegment: send,
		FailureCategory: func(ctx context.Context, category string, _ error) string {
			if outcome, ok := ctx.Value(SendOutcomeKey{}).(*SendOutcome); ok && outcome.Unknown.Load() {
				return "unknown_send"
			}
			return category
		},
	})
}
func EscapeMarkdown(text string) string {
	var escaped strings.Builder
	for _, r := range text {
		if strings.ContainsRune("\\`*_{}[]()#+-.!|>~", r) {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(r)
	}
	return escaped.String()
}

func (a *DiscordAdapter) SendText(chatID, text string) error {
	return a.SendTextContext(context.Background(), chatID, text)
}
func (a *DiscordAdapter) SendTextContext(ctx context.Context, chatID, text string) error {
	if ctx == nil {
		return errors.New("discord text send requires a context")
	}
	a.mu.Lock()
	if a.stopping || a.ctx.Err() != nil {
		a.mu.Unlock()
		return context.Canceled
	}
	a.sends.Add(1)
	a.mu.Unlock()
	defer a.sends.Done()
	outcome := &SendOutcome{}
	ctx = context.WithValue(ctx, SendOutcomeKey{}, outcome)
	err := a.sender.Send(ctx, a.ctx, nil, chatID, text, false)
	var partial *im.IMTextSendError
	if errors.As(err, &partial) && partial.Category == "unknown_send" {
		// The shared sender correctly stops following segments on cancellation,
		// but cancellation cannot prove that an already written POST failed.
		cause := im.ErrDeliveryOutcomeUnknown
		if errors.Is(err, context.Canceled) {
			cause = errors.Join(cause, context.Canceled)
		} else if errors.Is(err, context.DeadlineExceeded) {
			cause = errors.Join(cause, context.DeadlineExceeded)
		}
		return &im.IMTextSendError{AcceptedSegments: partial.AcceptedSegments, Category: "unknown_send", Cause: cause}
	}
	return err
}
func (a *DiscordAdapter) sendSegment(ctx context.Context, chatID, text string) error {
	id, err := snowflake.Parse(chatID)
	if err != nil || id == 0 {
		return errors.New("discord invalid_target")
	}
	outcome, _ := ctx.Value(SendOutcomeKey{}).(*SendOutcome)
	if outcome != nil {
		// Production HTTP installs a fresh attempt per RoundTrip. This fallback
		// still records writes for an injected REST client with a plain transport.
		attempt := outcome.beginAttempt()
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) {
			outcome.wroteAttempt(attempt)
		}})
	}
	message, err := a.rest.CreateMessage(id, discord.MessageCreate{Content: text, Flags: discord.MessageFlagSuppressEmbeds, AllowedMentions: &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Users: []snowflake.ID{}, Roles: []snowflake.ID{}, RepliedUser: false}}, rest.WithCtx(ctx))
	if err != nil {
		var response *rest.Error
		if errors.As(err, &response) && response.Response != nil {
			discordResponseOutcome(ctx, response.Response.StatusCode)
		}
		if ctx.Err() != nil {
			return ctx.Err() // SendTextContext preserves written-but-unconfirmed evidence.
		}
		safe := discordRESTError(err)
		if outcome != nil && errors.Is(safe, im.ErrDeliveryOutcomeUnknown) {
			status := int(outcome.Status.Load())
			if status == http.StatusTooManyRequests || status >= 400 && status < 500 && status != http.StatusRequestTimeout {
				return errors.New("discord rejected_response_processing_failed")
			}
			outcome.Unknown.Store(true)
		}
		return safe
	}
	if message == nil || message.ID == 0 {
		if outcome != nil {
			outcome.Unknown.Store(true)
		}
		return fmt.Errorf("discord unknown_send: %w", im.ErrDeliveryOutcomeUnknown)
	}
	if outcome != nil {
		outcome.Accepted()
	}
	return nil
}
