package slack

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/keakon/chord-gateway/internal/im"
)

var ErrCaptureBusy = errors.New("slack control snapshot busy")

const (
	slackConversationTTL = 5 * time.Minute
	// Bound the per-installation conversation cache independently of the
	// ingress queue so unsolicited channels cannot grow it without limit.
	ConversationCacheMax = 256
)

type CachedConversation struct {
	channel slackapi.Channel
	expires time.Time
}

type Inbound struct {
	client   *slackapi.Client
	identity Identity
	router   im.IMSocketHost
	mu       sync.Mutex
	cache    map[string]CachedConversation
}

func NewInbound(client *slackapi.Client, identity Identity, router im.IMSocketHost) *Inbound {
	return &Inbound{client: client, identity: identity, router: router, cache: make(map[string]CachedConversation)}
}

type InboundMessage struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	User        string `json:"user"`
	UserTeam    string `json:"user_team"`
	Team        string `json:"team"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	EventTS     string `json:"event_ts"`
	ThreadTS    string `json:"thread_ts"`
}

type EventsPayload struct {
	Type           string         `json:"type"`
	TeamID         string         `json:"team_id"`
	AppID          string         `json:"api_app_id"`
	EventID        string         `json:"event_id"`
	Event          InboundMessage `json:"event"`
	Authorizations []struct {
		IsEnterpriseInstall bool `json:"is_enterprise_install"`
	} `json:"authorizations"`
}

type SlashPayload struct {
	TeamID              string          `json:"team_id"`
	AppID               string          `json:"api_app_id"`
	ChannelID           string          `json:"channel_id"`
	UserID              string          `json:"user_id"`
	Command             string          `json:"command"`
	Text                string          `json:"text"`
	TriggerID           string          `json:"trigger_id"`
	IsEnterpriseInstall json.RawMessage `json:"is_enterprise_install,omitempty"`
}

// decode runs on the reader: validate installation scope, retain exact ts,
// and construct the operation before any slow conversation lookup.
func (s *Inbound) decode(kind string, payload json.RawMessage, envelopeID string, receivedAt time.Time) (im.IngressEnvelope, string, bool, error) {
	if envelopeID == "" {
		return im.IngressEnvelope{}, "", false, ErrIgnored
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	message := im.IncomingMessage{IMType: "slack", AccountID: s.identity.AccountID, AppID: s.identity.AppID}
	envelope := im.IngressEnvelope{Message: message, DeliveryID: envelopeID, ReceivedAt: receivedAt}
	var channel, thread, text string
	force := false
	switch kind {
	case "events_api":
		var p EventsPayload
		if json.Unmarshal(payload, &p) != nil || p.Type != "event_callback" || p.TeamID != s.identity.TeamID || p.AppID != s.identity.AppID || p.EventID == "" {
			return im.IngressEnvelope{}, "", false, ErrIgnored
		}
		for _, auth := range p.Authorizations {
			if auth.IsEnterpriseInstall {
				return im.IngressEnvelope{}, "", false, ErrIgnored
			}
		}
		e := p.Event
		if (e.Type != "message" && e.Type != "app_mention") || e.Subtype != "" || e.BotID != "" || e.User == "" || e.User == s.identity.BotUserID || (e.Team != "" && e.Team != s.identity.TeamID) || (e.UserTeam != "" && e.UserTeam != s.identity.TeamID) {
			return im.IngressEnvelope{}, "", false, ErrIgnored
		}
		if e.Type == "message" && (e.ChannelType != "im" || !strings.HasPrefix(e.Channel, "D")) {
			return im.IngressEnvelope{}, "", false, ErrIgnored
		}
		channel, thread, text = e.Channel, e.ThreadTS, e.Text
		if (e.Type == "app_mention" || thread != "" || !strings.HasPrefix(channel, "D")) && !strings.Contains(text, "<@"+s.identity.BotUserID+">") {
			return im.IngressEnvelope{}, "", false, ErrIgnored
		}
		// Check invocation syntax before decoding: escaped literal mentions
		// must never become authorization evidence. Decode only Slack's entities,
		// exactly once; slash text has escaping disabled and stays untouched.
		text = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(StripMentionPrefix(text, s.identity.BotUserID))
		if !ValidTimestamp(e.TS) {
			return im.IngressEnvelope{}, "", false, ErrIgnored
		}
		sourceTS := e.EventTS
		if sourceTS == "" {
			sourceTS = e.TS
		}
		var err error
		envelope.SourceTime, err = SourceTime(sourceTS)
		if err != nil {
			return im.IngressEnvelope{}, "", false, err
		}
		envelope.SemanticID, err = im.EncodeScopedID("slack-message-v1:", p.TeamID, channel, e.TS)
		if err != nil {
			return im.IngressEnvelope{}, "", false, ErrIgnored
		}
		envelope.Message.SenderID, envelope.Message.MessageID = e.User, "event:"+p.EventID
	case "slash_commands":
		var p SlashPayload
		if json.Unmarshal(payload, &p) != nil || p.TeamID != s.identity.TeamID || p.AppID != s.identity.AppID || p.UserID == "" || p.UserID == s.identity.BotUserID || p.Command != "/chord" || p.TriggerID == "" || !WorkspaceInstallation(p.IsEnterpriseInstall) {
			return im.IngressEnvelope{}, "", false, ErrIgnored
		}
		channel, text = p.ChannelID, SlashText(p.Text)
		envelope.Message.SenderID, envelope.Message.MessageID = p.UserID, "slash:"+p.TriggerID
		envelope.SourceTime = receivedAt
	default:
		return im.IngressEnvelope{}, "", false, ErrIgnored
	}
	var err error
	envelope.Message.ChatID, err = Target(channel, thread)
	if err != nil || strings.TrimSpace(text) == "" {
		return im.IngressEnvelope{}, "", false, ErrIgnored
	}
	envelope.Message.Text = text
	// Kind stays missing until conversations.info proves the conversation.
	force = isBindCommand(text)
	return envelope, channel, force, nil
}

func WorkspaceInstallation(value json.RawMessage) bool {
	// Socket slash fields may contain JSON booleans or form-derived strings.
	return len(value) == 0 || string(value) == "false" || string(value) == `"false"`
}

func SlashText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "/help"
	}
	return "/" + text
}

func StripMentionPrefix(text, botUserID string) string {
	text = strings.TrimSpace(text)
	mention := "<@" + botUserID + ">"
	if strings.HasPrefix(text, mention) {
		return strings.TrimSpace(strings.TrimPrefix(text, mention))
	}
	return text
}

// submit never performs network I/O on the socket reader. Shared normalizer
// keys use the root channel so API lookups preserve receive order even across
// threads. Its common queue and worker limits apply across all platforms.
func (s *Inbound) submit(ctx context.Context, kind string, payload json.RawMessage, envelopeID string, receivedAt time.Time) (<-chan im.ReceiveResult, context.CancelFunc, error) {
	envelope, channel, force, err := s.decode(kind, payload, envelopeID, receivedAt)
	if err != nil {
		return nil, nil, err
	}
	if s.router == nil {
		return nil, nil, im.ErrIngressClosed
	}
	capturedKind, target, ok := s.router.TryCaptureIngress(envelope.Message)
	if !ok {
		return nil, nil, ErrCaptureBusy
	}
	envelope.Capture = &im.IngressCapture{Kind: capturedKind, Target: target}
	// Admission may finish after the ACK window; bound lookup and queue time
	// independently, and cancel all work when its socket generation closes.
	ctx, cancel := context.WithDeadline(ctx, envelope.ReceivedAt.Add(30*time.Second))
	future, err := s.router.SubmitIngressNormalization(ctx, channel, envelope, func(ctx context.Context, envelope im.IngressEnvelope) (im.IngressEnvelope, error) {
		conversation, err := s.conversation(ctx, channel, force)
		if err != nil {
			return im.IngressEnvelope{}, err
		}
		kind := ConversationKind(conversation, channel, envelope.Message.SenderID)
		if kind != "unsupported" && strings.HasPrefix(envelope.Message.ChatID, "target-v1:") {
			kind = "group"
		}
		envelope.Message.ChatKind = kind
		return envelope, nil
	})
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return future, cancel, nil
}

func (s *Inbound) conversation(ctx context.Context, channel string, force bool) (slackapi.Channel, error) {
	s.mu.Lock()
	cached, exists := s.cache[channel]
	s.mu.Unlock()
	if !force && exists && time.Now().Before(cached.expires) {
		return cached.channel, nil
	}
	conversation, err := s.client.GetConversationInfoContext(ctx, &slackapi.GetConversationInfoInput{ChannelID: channel})
	if err != nil {
		return slackapi.Channel{}, APIError("conversations.info", err)
	}
	if conversation == nil || conversation.ID != channel {
		return slackapi.Channel{}, ErrIgnored
	}
	s.mu.Lock()
	// A workspace can have many unsolicited channels. Evict expired entries
	// first, then the entry closest to expiry, so a long-lived workspace keeps
	// the conversations it actually uses.
	now := time.Now()
	if len(s.cache) >= ConversationCacheMax {
		for id, cached := range s.cache {
			if now.After(cached.expires) {
				delete(s.cache, id)
			}
		}
		for len(s.cache) >= ConversationCacheMax {
			oldestID, oldest := "", time.Time{}
			for id, cached := range s.cache {
				if oldestID == "" || cached.expires.Before(oldest) {
					oldestID, oldest = id, cached.expires
				}
			}
			delete(s.cache, oldestID)
		}
	}
	s.cache[channel] = CachedConversation{channel: *conversation, expires: now.Add(slackConversationTTL)}
	s.mu.Unlock()
	return *conversation, nil
}

func ConversationKind(channel slackapi.Channel, channelID, sender string) string {
	if channel.ID != channelID || channel.IsShared || channel.IsExtShared || channel.IsOrgShared || channel.IsGlobalShared || channel.IsPendingExtShared || channel.IsArchived || channel.IsMpIM {
		return "unsupported"
	}
	if channel.IsIM {
		if !strings.HasPrefix(channelID, "D") || channel.User == "" || channel.User != sender || channel.IsUserDeleted {
			return "unsupported"
		}
		return "private"
	}
	if !strings.HasPrefix(channelID, "D") && (channel.IsChannel || channel.IsGroup || channel.IsPrivate) {
		return "group"
	}
	return "unsupported"
}

// isBindCommand reports whether text invokes /bind, mirroring the gateway's
// shared command model for that one token (case-insensitive exact leading
// token after trimming). The router owns full command parsing; the inbound
// only needs this bit to force a fresh conversation lookup for binding.
func isBindCommand(text string) bool {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return false
	}
	return strings.ToLower(strings.SplitN(text, " ", 2)[0]) == "/bind"
}
