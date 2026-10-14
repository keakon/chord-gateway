package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

var (
	errSlackFatal   = errors.New("slack authentication or installation identity is invalid")
	errSlackIgnored = errors.New("unsupported or malformed Slack input")
)

type slackIdentity struct {
	TeamID, AppID, BotUserID, BotID, AccountID string
	BotName                                    string // Presentation only; renaming a bot does not change its installation.
}

func (s slackIdentity) sameInstallation(other slackIdentity) bool {
	// BotName is presentation only; renaming a bot does not change installation.
	return s.TeamID == other.TeamID && s.AppID == other.AppID &&
		s.BotUserID == other.BotUserID && s.BotID == other.BotID && s.AccountID == other.AccountID
}

// verifySlackIdentity proves that the bot token and configured receiver app
// belong to the same installation. Token prefixes are not identity evidence.
func verifySlackIdentity(ctx context.Context, client *slack.Client, teamID, appID string) (slackIdentity, error) {
	if client == nil || teamID == "" || appID == "" {
		return slackIdentity{}, fmt.Errorf("slack identity configuration: %w", errSlackFatal)
	}
	auth, err := client.AuthTestContext(ctx)
	if err != nil {
		return slackIdentity{}, slackAPIError("auth.test", err)
	}
	if auth == nil || auth.TeamID != teamID || auth.UserID == "" || auth.BotID == "" {
		return slackIdentity{}, fmt.Errorf("slack bot installation identity mismatch: %w", errSlackFatal)
	}
	bot, err := client.GetBotInfoContext(ctx, slack.GetBotInfoParameters{Bot: auth.BotID, TeamID: teamID})
	if err != nil {
		return slackIdentity{}, slackAPIError("bots.info", err)
	}
	if bot == nil || bot.ID != auth.BotID || bot.AppID != appID || bot.UserID != auth.UserID || bot.Deleted {
		return slackIdentity{}, fmt.Errorf("slack bot app identity mismatch: %w", errSlackFatal)
	}
	account, err := EncodeScopedID("account-v1:", teamID, auth.UserID)
	if err != nil {
		return slackIdentity{}, fmt.Errorf("slack account identity: %w", errSlackFatal)
	}
	return slackIdentity{TeamID: teamID, AppID: appID, BotUserID: auth.UserID, BotID: auth.BotID, AccountID: account, BotName: strings.Join(strings.Fields(bot.Name), " ")}, nil
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
		return errSlackFatal
	}
	return nil
}

// slackAPIError only returns allowlisted categories. SDK transport errors can
// contain sensitive URLs, so they must not flow into gateway logs.
func slackAPIError(method string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("slack %s: %w", method, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("slack %s: %w", method, context.DeadlineExceeded)
	}
	var apiErr slack.SlackErrorResponse
	if errors.As(err, &apiErr) {
		switch apiErr.Err {
		case "invalid_auth", "account_inactive", "token_revoked", "not_authed", "missing_scope":
			return slackAPIErrorDetail{method, apiErr.Err, true}
		case "channel_not_found", "not_in_channel", "is_archived":
			return slackAPIErrorDetail{method, apiErr.Err, false}
		}
	}
	var limited *slack.RateLimitedError
	if errors.As(err, &limited) {
		return fmt.Errorf("slack %s: %w", method, limited)
	}
	return slackAPIErrorDetail{method, "api_unavailable", false}
}

func validSlackChannelID(id string) bool {
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

func validSlackTimestamp(ts string) bool {
	_, err := slackSourceTime(ts)
	return err == nil
}

// Slack timestamps remain strings for identity and thread addressing. Only
// source-time metadata is converted, using integer arithmetic.
func slackSourceTime(ts string) (time.Time, error) {
	seconds, fraction, ok := strings.Cut(ts, ".")
	if !ok || seconds == "" || fraction == "" || len(seconds) > 12 || len(fraction) > 9 {
		return time.Time{}, errSlackIgnored
	}
	for _, c := range seconds + fraction {
		if c < '0' || c > '9' {
			return time.Time{}, errSlackIgnored
		}
	}
	sec, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, errSlackIgnored
	}
	nsec, err := strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
	if err != nil {
		return time.Time{}, errSlackIgnored
	}
	return time.Unix(sec, nsec).UTC(), nil
}

func slackTarget(channel, threadTS string) (string, error) {
	if !validSlackChannelID(channel) {
		return "", errSlackIgnored
	}
	if threadTS == "" {
		return channel, nil
	}
	if !validSlackTimestamp(threadTS) {
		return "", errSlackIgnored
	}
	return EncodeScopedID("target-v1:", "slack-thread", channel, threadTS)
}

func decodeSlackTarget(target string) (channel, threadTS string, err error) {
	if validSlackChannelID(target) {
		return target, "", nil
	}
	parts, err := DecodeScopedID(target, "target-v1:", 3)
	if err != nil || parts[0] != "slack-thread" || !validSlackChannelID(parts[1]) || !validSlackTimestamp(parts[2]) {
		return "", "", errSlackIgnored
	}
	return parts[1], parts[2], nil
}
