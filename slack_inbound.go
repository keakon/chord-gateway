package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/keakon/chord-gateway/internal/im"
)

var errSlackCaptureBusy = errors.New("slack control snapshot busy")

const (
	slackConversationTTL = 5 * time.Minute
	// Bound the per-installation conversation cache independently of the
	// ingress queue so unsolicited channels cannot grow it without limit.
	slackConversationCacheMax = 256
)

type slackCachedConversation struct {
	channel slack.Channel
	expires time.Time
}

type slackInbound struct {
	client   *slack.Client
	identity slackIdentity
	router   im.IMSocketHost
	mu       sync.Mutex
	cache    map[string]slackCachedConversation
}

func newSlackInbound(client *slack.Client, identity slackIdentity, router im.IMSocketHost) *slackInbound {
	return &slackInbound{client: client, identity: identity, router: router, cache: make(map[string]slackCachedConversation)}
}

type slackInboundMessage struct {
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

type slackEventsPayload struct {
	Type           string              `json:"type"`
	TeamID         string              `json:"team_id"`
	AppID          string              `json:"api_app_id"`
	EventID        string              `json:"event_id"`
	Event          slackInboundMessage `json:"event"`
	Authorizations []struct {
		IsEnterpriseInstall bool `json:"is_enterprise_install"`
	} `json:"authorizations"`
}

type slackSlashPayload struct {
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
func (s *slackInbound) decode(kind string, payload json.RawMessage, envelopeID string, receivedAt time.Time) (im.IngressEnvelope, string, bool, error) {
	if envelopeID == "" {
		return im.IngressEnvelope{}, "", false, errSlackIgnored
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
		var p slackEventsPayload
		if json.Unmarshal(payload, &p) != nil || p.Type != "event_callback" || p.TeamID != s.identity.TeamID || p.AppID != s.identity.AppID || p.EventID == "" {
			return im.IngressEnvelope{}, "", false, errSlackIgnored
		}
		for _, auth := range p.Authorizations {
			if auth.IsEnterpriseInstall {
				return im.IngressEnvelope{}, "", false, errSlackIgnored
			}
		}
		e := p.Event
		if (e.Type != "message" && e.Type != "app_mention") || e.Subtype != "" || e.BotID != "" || e.User == "" || e.User == s.identity.BotUserID || (e.Team != "" && e.Team != s.identity.TeamID) || (e.UserTeam != "" && e.UserTeam != s.identity.TeamID) {
			return im.IngressEnvelope{}, "", false, errSlackIgnored
		}
		if e.Type == "message" && (e.ChannelType != "im" || !strings.HasPrefix(e.Channel, "D")) {
			return im.IngressEnvelope{}, "", false, errSlackIgnored
		}
		channel, thread, text = e.Channel, e.ThreadTS, e.Text
		if (e.Type == "app_mention" || thread != "" || !strings.HasPrefix(channel, "D")) && !strings.Contains(text, "<@"+s.identity.BotUserID+">") {
			return im.IngressEnvelope{}, "", false, errSlackIgnored
		}
		// Check invocation syntax before decoding: escaped literal mentions
		// must never become authorization evidence. Decode only Slack's entities,
		// exactly once; slash text has escaping disabled and stays untouched.
		text = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(stripSlackMentionPrefix(text, s.identity.BotUserID))
		if !validSlackTimestamp(e.TS) {
			return im.IngressEnvelope{}, "", false, errSlackIgnored
		}
		sourceTS := e.EventTS
		if sourceTS == "" {
			sourceTS = e.TS
		}
		var err error
		envelope.SourceTime, err = slackSourceTime(sourceTS)
		if err != nil {
			return im.IngressEnvelope{}, "", false, err
		}
		envelope.SemanticID, err = im.EncodeScopedID("slack-message-v1:", p.TeamID, channel, e.TS)
		if err != nil {
			return im.IngressEnvelope{}, "", false, errSlackIgnored
		}
		envelope.Message.SenderID, envelope.Message.MessageID = e.User, "event:"+p.EventID
	case "slash_commands":
		var p slackSlashPayload
		if json.Unmarshal(payload, &p) != nil || p.TeamID != s.identity.TeamID || p.AppID != s.identity.AppID || p.UserID == "" || p.UserID == s.identity.BotUserID || p.Command != "/chord" || p.TriggerID == "" || !slackWorkspaceInstallation(p.IsEnterpriseInstall) {
			return im.IngressEnvelope{}, "", false, errSlackIgnored
		}
		channel, text = p.ChannelID, slackSlashText(p.Text)
		envelope.Message.SenderID, envelope.Message.MessageID = p.UserID, "slash:"+p.TriggerID
		envelope.SourceTime = receivedAt
	default:
		return im.IngressEnvelope{}, "", false, errSlackIgnored
	}
	var err error
	envelope.Message.ChatID, err = slackTarget(channel, thread)
	if err != nil || strings.TrimSpace(text) == "" {
		return im.IngressEnvelope{}, "", false, errSlackIgnored
	}
	envelope.Message.Text = text
	// Kind stays missing until conversations.info proves the conversation.
	force = parseIMCommand(text).Type == "bind"
	return envelope, channel, force, nil
}

func slackWorkspaceInstallation(value json.RawMessage) bool {
	// Socket slash fields may contain JSON booleans or form-derived strings.
	return len(value) == 0 || string(value) == "false" || string(value) == `"false"`
}

func slackSlashText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "/help"
	}
	return "/" + text
}

func stripSlackMentionPrefix(text, botUserID string) string {
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
func (s *slackInbound) submit(ctx context.Context, kind string, payload json.RawMessage, envelopeID string, receivedAt time.Time) (<-chan im.ReceiveResult, context.CancelFunc, error) {
	envelope, channel, force, err := s.decode(kind, payload, envelopeID, receivedAt)
	if err != nil {
		return nil, nil, err
	}
	if s.router == nil {
		return nil, nil, errIngressClosed
	}
	capturedKind, target, ok := s.router.TryCaptureIngress(envelope.Message)
	if !ok {
		return nil, nil, errSlackCaptureBusy
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
		kind := slackConversationKind(conversation, channel, envelope.Message.SenderID)
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

func (s *slackInbound) conversation(ctx context.Context, channel string, force bool) (slack.Channel, error) {
	s.mu.Lock()
	cached, exists := s.cache[channel]
	s.mu.Unlock()
	if !force && exists && time.Now().Before(cached.expires) {
		return cached.channel, nil
	}
	conversation, err := s.client.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channel})
	if err != nil {
		return slack.Channel{}, slackAPIError("conversations.info", err)
	}
	if conversation == nil || conversation.ID != channel {
		return slack.Channel{}, errSlackIgnored
	}
	s.mu.Lock()
	// A workspace can have many unsolicited channels. Evict expired entries
	// first, then the entry closest to expiry, so a long-lived workspace keeps
	// the conversations it actually uses.
	now := time.Now()
	if len(s.cache) >= slackConversationCacheMax {
		for id, cached := range s.cache {
			if now.After(cached.expires) {
				delete(s.cache, id)
			}
		}
		for len(s.cache) >= slackConversationCacheMax {
			oldestID, oldest := "", time.Time{}
			for id, cached := range s.cache {
				if oldestID == "" || cached.expires.Before(oldest) {
					oldestID, oldest = id, cached.expires
				}
			}
			delete(s.cache, oldestID)
		}
	}
	s.cache[channel] = slackCachedConversation{channel: *conversation, expires: now.Add(slackConversationTTL)}
	s.mu.Unlock()
	return *conversation, nil
}

func slackConversationKind(channel slack.Channel, channelID, sender string) string {
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
