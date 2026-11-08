package slack

import (
	"context"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// TestSlackSendTextContextControlBudget pins the reply-permit deadline
// derivation: a flow reply must inherit the 15s control-send budget instead
// of the five-minute proactive budget.
func TestSlackSendTextContextControlBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		permit bool
	}{
		{name: "reply permit gets control budget", permit: true},
		{name: "no permit keeps proactive budget", permit: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hasDeadline bool
			var remaining time.Duration
			a, err := NewSlackAdapter(nil, config.IMAdapterConfig{Slack: &config.SlackConfig{AppToken: "xapp-sample", BotToken: "xoxb-sample", AppID: "AAPP1", TeamID: "TTEAM1"}}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Disconnect)
			a.SetIdentityForTest(Identity{TeamID: "TTEAM1", AppID: "AAPP1", AccountID: "AAPP1"}, true)
			a.sender.client = slackPostFunc(func(ctx context.Context, channel string, _ ...slackapi.MsgOption) (string, string, error) {
				deadline, ok := ctx.Deadline()
				hasDeadline, remaining = ok, time.Until(deadline)
				return channel, "1234567890.000001", nil
			})
			ctx := t.Context()
			if tc.permit {
				ctx = context.WithValue(ctx, im.OutboundReplyContextKey{}, im.ReplyPermit{SenderID: "user-1", ExpiresAt: time.Now().Add(time.Hour)})
			}
			if err := a.SendTextContext(ctx, "CCHAN1", "sample reply"); err != nil {
				t.Fatalf("send failed: %v", err)
			}
			if !hasDeadline {
				t.Fatal("post message request carries no deadline")
			}
			if tc.permit {
				if remaining <= 0 || remaining > im.IMControlSendTimeout+time.Second {
					t.Fatalf("control reply deadline = %s, want within %s", remaining, im.IMControlSendTimeout)
				}
				return
			}
			if remaining <= im.IMControlSendTimeout || remaining > im.IMTextSendTimeout+time.Second {
				t.Fatalf("proactive deadline = %s, want about %s", remaining, im.IMTextSendTimeout)
			}
		})
	}
}
