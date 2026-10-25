package discord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/disgoorg/disgo/discord"

	"github.com/keakon/chord-gateway/internal/im"
)

const DiscordChannelCacheCapacity = 4096

var ErrDiscordStaleGeneration = errors.New("discord connection generation unavailable")

type discordIngressSubmit func(context.Context, string, im.IngressEnvelope, func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error)

type discordChannelMetadata struct {
	kind discord.ChannelType
}

// discordInbound shares the common bounded normalizer. Neither the Gateway
// listener nor its heartbeat waits for a REST lookup or a in-memory admission.
// Generation guards the receive boundary. Once captured into the queue, valid
// input belongs to the account lifecycle and survives a transient reconnect.
type discordInbound struct {
	ctx                      context.Context
	accountID, applicationID string
	submit                   discordIngressSubmit
	resolve                  func(context.Context, string) (discord.Channel, error)
	current                  func(uint64) bool
	live                     func(uint64) bool
	observe                  func(event, reason string)
	mu                       sync.Mutex
	channels                 map[string]discordChannelMetadata
}

func NewDiscordInbound(ctx context.Context, accountID, applicationID string, submit discordIngressSubmit, resolve func(context.Context, string) (discord.Channel, error), current, live func(uint64) bool, observe func(event, reason string)) *discordInbound {
	return &discordInbound{
		ctx: ctx, accountID: accountID, applicationID: applicationID,
		submit: submit, resolve: resolve, current: current, live: live, observe: observe,
		channels: make(map[string]discordChannelMetadata),
	}
}

func (d *discordInbound) available(generation uint64) bool {
	return d.ctx.Err() == nil && d.current != nil && d.current(generation)
}

// connectionLive accepts classification from a generation that still owns the
// transport, even while the resume barrier holds back input.
func (d *discordInbound) connectionLive(generation uint64) bool {
	return d.ctx.Err() == nil && d.live != nil && d.live(generation)
}

// CacheChannel only stores classification, never authorization or send access.
func (d *discordInbound) CacheChannel(generation uint64, channel discord.Channel) {
	if channel == nil || channel.ID() == 0 || !d.connectionLive(generation) {
		return
	}
	d.cacheChannel(channel.ID().String(), discordChannelMetadata{channel.Type()})
}

func (d *discordInbound) ClearChannels() {
	d.mu.Lock()
	clear(d.channels)
	d.mu.Unlock()
}

func (d *discordInbound) cacheChannel(id string, metadata discordChannelMetadata) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.channels[id]; !exists && len(d.channels) >= DiscordChannelCacheCapacity {
		// Classification can always be fetched again. Keep memory bounded without
		// retaining potentially sensitive full channel objects from SDK events.
		// Go map iteration order is random, so the entry dropped here is
		// arbitrary rather than the oldest.
		for entry := range d.channels {
			delete(d.channels, entry)
			break
		}
	}
	d.channels[id] = metadata
}

func (d *discordInbound) DeleteChannel(generation uint64, id string) {
	if !d.connectionLive(generation) {
		return
	}
	d.mu.Lock()
	delete(d.channels, id)
	d.mu.Unlock()
}

func (d *discordInbound) channelMetadata(ctx context.Context, generation uint64, id string) (discordChannelMetadata, error) {
	d.mu.Lock()
	metadata, exists := d.channels[id]
	d.mu.Unlock()
	if exists {
		return metadata, nil
	}
	if d.resolve == nil {
		return discordChannelMetadata{}, errors.New("discord channel lookup unavailable")
	}
	channel, err := d.resolve(ctx, id)
	if err != nil {
		// REST errors may carry the URL or response body. Keep the event boundary
		// error limited to a safe category; the REST wrapper owns classification.
		return discordChannelMetadata{}, errors.New("discord channel lookup failed")
	}
	if err := ctx.Err(); err != nil {
		return discordChannelMetadata{}, err
	}
	if err := d.ctx.Err(); err != nil {
		return discordChannelMetadata{}, err
	}
	if channel == nil || channel.ID().String() != id {
		return discordChannelMetadata{}, errors.New("discord channel identity mismatch")
	}
	metadata = discordChannelMetadata{channel.Type()}
	// A captured message survives transient reconnects, but its old lookup must
	// not overwrite classification events from a newer connection.
	if d.available(generation) {
		d.cacheChannel(id, metadata)
	}
	return metadata, nil
}

// HandleMessage dispatches one socket event synchronously, in Gateway dispatch
// order. The Gateway captures at arrival and delivers through the resume-aware
// closure instead; this path keeps that order observable for tests. The future
// is buffered by the common ingress service, and Discord MESSAGE_CREATE has no
// per-message ACK.
func (d *discordInbound) HandleMessage(generation uint64, message discord.Message) error {
	if !d.available(generation) {
		return ErrDiscordStaleGeneration
	}
	deliver, err := d.CaptureMessage(generation, message, nil)
	if err != nil || deliver == nil {
		return err
	}
	return deliver()
}

// reportContentless records a mention the gateway deliberately does not accept.
// The metric has fixed labels and counts every drop; only the log line is rate
// limited and capped, because this is a frequent interaction shape.
func (d *discordInbound) reportContentless() {
	if d.observe != nil {
		d.observe("callback_dropped", "unsupported_content")
	}
	if discordContentlessReported() {
		discordLogCategory("message_dropped", "contentless")
	}
}

// CaptureMessage freezes an authenticated socket event before Resume replay is
// complete. The SDK checks its producing generation; delivery later uses the
// same common queue and never recaptures a newer control target.
func (d *discordInbound) CaptureMessage(generation uint64, message discord.Message, capture func(im.IngressEnvelope) im.IngressEnvelope) (func() error, error) {
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}
	if message.Author.Bot || message.Author.System || message.WebhookID != nil || (message.Type != discord.MessageTypeDefault && message.Type != discord.MessageTypeReply) {
		return nil, nil
	}
	if d.accountID == "" || message.ID == 0 || message.ChannelID == 0 || message.Author.ID == 0 || message.CreatedAt.IsZero() {
		return nil, errors.New("discord message identity invalid")
	}
	guild := message.GuildID != nil && *message.GuildID != 0
	text, forceText := message.Content, false
	if guild {
		if !discordMentionsAccount(message.Mentions, d.accountID) {
			return nil, nil
		}
		var prefix bool
		text, prefix = discordStripInvocationPrefix(text, d.accountID)
		forceText = !prefix
	}
	plain := strings.TrimSpace(text)
	if plain == "" || (guild && (plain == "<@"+d.accountID+">" || plain == "<@!"+d.accountID+">")) {
		// The Message Content intent is deliberately not requested, so a guild
		// reply without ping-on-reply or a mention arrives with no text at all.
		// Reporting it keeps that silence distinguishable from a lost event.
		d.reportContentless()
		return nil, nil
	}
	if d.submit == nil {
		return nil, errors.New("discord ingress unavailable")
	}
	envelope := im.IngressEnvelope{
		Message: im.IncomingMessage{
			IMType: "discord", AccountID: d.accountID, AppID: d.applicationID,
			ChatID: message.ChannelID.String(), SenderID: message.Author.ID.String(),
			SenderName: message.Author.Username, MessageID: "message:" + message.ID.String(),
			Text: text, ForceText: forceText,
		},
		SemanticID: "message:" + message.ID.String(),
		SourceTime: message.CreatedAt, ReceivedAt: time.Now(),
	}
	if capture != nil {
		envelope = capture(envelope)
	}
	return func() error {
		_, err := d.submit(d.ctx, envelope.Message.ChatID, envelope, func(ctx context.Context, captured im.IngressEnvelope) (im.IngressEnvelope, error) {
			if err := ctx.Err(); err != nil {
				return im.IngressEnvelope{}, err
			}
			if err := d.ctx.Err(); err != nil {
				return im.IngressEnvelope{}, err
			}
			metadata, err := d.channelMetadata(ctx, generation, captured.Message.ChatID)
			if err != nil {
				return im.IngressEnvelope{}, err
			}
			if err := ctx.Err(); err != nil {
				return im.IngressEnvelope{}, err
			}
			if err := d.ctx.Err(); err != nil {
				return im.IngressEnvelope{}, err
			}
			switch metadata.kind {
			case discord.ChannelTypeDM:
				if !guild {
					captured.Message.ChatKind = "private"
				}
			case discord.ChannelTypeGuildText, discord.ChannelTypeGuildPublicThread, discord.ChannelTypeGuildPrivateThread:
				if guild {
					captured.Message.ChatKind = "group"
				}
			}
			if captured.Message.ChatKind == "" {
				// Unsupported channel types and contradictory transport evidence must
				// leave permanent rejected tombstones, especially a GroupDM /bind.
				captured.Message.ChatKind = "unsupported"
			}
			return captured, nil
		})
		return err
	}, nil
}

func discordMentionsAccount(mentions []discord.User, accountID string) bool {
	for _, user := range mentions {
		if user.ID.String() == accountID {
			return true
		}
	}
	return false
}

func discordStripInvocationPrefix(text, accountID string) (string, bool) {
	for _, prefix := range []string{"<@" + accountID + ">", "<@!" + accountID + ">"} {
		if strings.HasPrefix(text, prefix) {
			return strings.TrimLeftFunc(strings.TrimPrefix(text, prefix), unicode.IsSpace), true
		}
	}
	return text, false
}
