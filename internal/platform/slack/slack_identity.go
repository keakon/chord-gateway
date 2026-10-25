package slack

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/keakon/chord-gateway/internal/im"

	slackapi "github.com/slack-go/slack"
)

var (
	ErrFatal   = errors.New("slack authentication or installation identity is invalid")
	ErrIgnored = errors.New("unsupported or malformed Slack input")
)

type Identity struct {
	TeamID, AppID, BotUserID, BotID, AccountID string
	BotName                                    string // Presentation only; renaming a bot does not change its installation.
}

func (s Identity) sameInstallation(other Identity) bool {
	// BotName is presentation only; renaming a bot does not change installation.
	return s.TeamID == other.TeamID && s.AppID == other.AppID &&
		s.BotUserID == other.BotUserID && s.BotID == other.BotID && s.AccountID == other.AccountID
}

// VerifyIdentity proves that the bot token and configured receiver app
// belong to the same installation. Token prefixes are not identity evidence.
func VerifyIdentity(ctx context.Context, client *slackapi.Client, teamID, appID string) (Identity, error) {
	if client == nil || teamID == "" || appID == "" {
		return Identity{}, fmt.Errorf("slack identity configuration: %w", ErrFatal)
	}
	auth, err := client.AuthTestContext(ctx)
	if err != nil {
		return Identity{}, APIError("auth.test", err)
	}
	if auth == nil || auth.TeamID != teamID || auth.UserID == "" || auth.BotID == "" {
		return Identity{}, fmt.Errorf("slack bot installation identity mismatch: %w", ErrFatal)
	}
	bot, err := client.GetBotInfoContext(ctx, slackapi.GetBotInfoParameters{Bot: auth.BotID, TeamID: teamID})
	if err != nil {
		return Identity{}, APIError("bots.info", err)
	}
	if bot == nil || bot.ID != auth.BotID || bot.AppID != appID || bot.UserID != auth.UserID || bot.Deleted {
		return Identity{}, fmt.Errorf("slack bot app identity mismatch: %w", ErrFatal)
	}
	account, err := im.EncodeScopedID("account-v1:", teamID, auth.UserID)
	if err != nil {
		return Identity{}, fmt.Errorf("slack account identity: %w", ErrFatal)
	}
	return Identity{TeamID: teamID, AppID: appID, BotUserID: auth.UserID, BotID: auth.BotID, AccountID: account, BotName: strings.Join(strings.Fields(bot.Name), " ")}, nil
}

type slackAPIErrorDetail struct {
	method, category string
	fatal            bool
}

func (e slackAPIErrorDetail) Error() string    { return "slack " + e.method + ": " + e.category }
func (e slackAPIErrorDetail) Fatal() bool      { return e.fatal }
func (e slackAPIErrorDetail) Category() string { return e.category }
func (e slackAPIErrorDetail) Unwrap() error {
	if e.fatal {
		return ErrFatal
	}
	return nil
}

// APIError only returns allowlisted categories. SDK transport errors can
// contain sensitive URLs, so they must not flow into gateway logs.
func APIError(method string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("slack %s: %w", method, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("slack %s: %w", method, context.DeadlineExceeded)
	}
	var apiErr slackapi.SlackErrorResponse
	if errors.As(err, &apiErr) {
		switch apiErr.Err {
		case "invalid_auth", "account_inactive", "token_revoked", "not_authed", "missing_scope":
			return slackAPIErrorDetail{method, apiErr.Err, true}
		case "channel_not_found", "not_in_channel", "is_archived":
			return slackAPIErrorDetail{method, apiErr.Err, false}
		}
	}
	var limited *slackapi.RateLimitedError
	if errors.As(err, &limited) {
		return fmt.Errorf("slack %s: %w", method, limited)
	}
	return slackAPIErrorDetail{method, "api_unavailable", false}
}

func ValidChannelID(id string) bool {
	if len(id) < 2 || len(id) > 64 || !strings.ContainsRune("CGD", rune(id[0])) {
		return false
	}
	for _, c := range id[1:] {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func ValidTimestamp(ts string) bool {
	_, err := SourceTime(ts)
	return err == nil
}

// SourceTime converts source-time metadata using integer arithmetic. Slack
// timestamps remain strings for identity and thread addressing.
func SourceTime(ts string) (time.Time, error) {
	seconds, fraction, ok := strings.Cut(ts, ".")
	if !ok || seconds == "" || fraction == "" || len(seconds) > 12 || len(fraction) > 9 {
		return time.Time{}, ErrIgnored
	}
	for _, c := range seconds + fraction {
		if c < '0' || c > '9' {
			return time.Time{}, ErrIgnored
		}
	}
	sec, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, ErrIgnored
	}
	nsec, err := strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
	if err != nil {
		return time.Time{}, ErrIgnored
	}
	return time.Unix(sec, nsec).UTC(), nil
}

func Target(channel, threadTS string) (string, error) {
	if !ValidChannelID(channel) {
		return "", ErrIgnored
	}
	if threadTS == "" {
		return channel, nil
	}
	if !ValidTimestamp(threadTS) {
		return "", ErrIgnored
	}
	return im.EncodeScopedID("target-v1:", "slack-thread", channel, threadTS)
}

func DecodeTarget(target string) (channel, threadTS string, err error) {
	if ValidChannelID(target) {
		return target, "", nil
	}
	parts, err := im.DecodeScopedID(target, "target-v1:", 3)
	if err != nil || parts[0] != "slack-thread" || !ValidChannelID(parts[1]) || !ValidTimestamp(parts[2]) {
		return "", "", ErrIgnored
	}
	return parts[1], parts[2], nil
}
