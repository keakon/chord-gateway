package slack

import (
	"context"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func TestSlackSendTextContextControlBudget(t *testing.T) {
	for _, tc := range []struct {
		name               string
		permit             bool
		parentBudget, want time.Duration
	}{
		{name: "control budget", permit: true, parentBudget: time.Hour, want: 15 * time.Second},
		{name: "proactive budget", parentBudget: time.Hour, want: 5 * time.Minute},
		{name: "short caller control", permit: true, parentBudget: 5 * time.Second, want: 15 * time.Second},
		{name: "short caller proactive", parentBudget: 5 * time.Second, want: 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deadlines []time.Time
			a, err := NewSlackAdapter(nil, config.IMAdapterConfig{Slack: &config.SlackConfig{AppToken: "xapp-sample", BotToken: "xoxb-sample", AppID: "AAPP1", TeamID: "TTEAM1"}}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Disconnect)
			a.SetIdentityForTest(Identity{TeamID: "TTEAM1", AppID: "AAPP1", AccountID: "AAPP1"}, true)
			a.sender.limits.channelInterval = 0
			a.sender.limits.accountInterval = 0
			a.sender.client = slackPostFunc(func(ctx context.Context, channel string, _ ...slackapi.MsgOption) (string, string, error) {
				deadline, _ := ctx.Deadline()
				deadlines = append(deadlines, deadline)
				return channel, "1234567890.000001", nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), tc.parentBudget)
			defer cancel()
			if tc.permit {
				ctx = context.WithValue(ctx, im.OutboundReplyContextKey{}, im.ReplyPermit{SenderID: "user-1", ExpiresAt: time.Now().Add(time.Hour)})
			}
			parentDeadline, _ := ctx.Deadline()
			start := time.Now()
			if err := a.SendTextContext(ctx, "CCHAN1", strings.Repeat("a", 3*slackTextBudget)); err != nil {
				t.Fatalf("send failed: %v", err)
			}
			end := time.Now()
			if len(deadlines) != 3 {
				t.Fatalf("request deadlines = %v, want three segments", deadlines)
			}
			earliest, latest := start.Add(tc.want), end.Add(tc.want)
			if parentDeadline.Before(earliest) {
				earliest = parentDeadline
			}
			if parentDeadline.Before(latest) {
				latest = parentDeadline
			}
			if deadlines[0].Before(earliest) || deadlines[0].After(latest) {
				t.Fatalf("deadline = %s, want between %s and %s", deadlines[0], earliest, latest)
			}
			for i, deadline := range deadlines[1:] {
				if !deadline.Equal(deadlines[0]) {
					t.Fatalf("segment %d renewed deadline: %s != %s", i+2, deadline, deadlines[0])
				}
			}
		})
	}
}
