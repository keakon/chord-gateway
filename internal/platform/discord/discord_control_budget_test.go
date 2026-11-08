package discord

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// TestDiscordSendTextContextControlBudget pins the reply-permit deadline
// derivation: a flow reply must inherit the 15s control-send budget instead
// of the five-minute proactive budget.
func TestDiscordSendTextContextControlBudget(t *testing.T) {
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"id":"999","channel_id":"500"}`)
			}))
			defer server.Close()
			adapter := discordRESTAdapterForTest(t, server)
			// The server-side request context carries no client deadline, so
			// capture it on the client side of the transport.
			base := adapter.rest.HTTPClient().Transport
			adapter.rest.HTTPClient().Transport = discordRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				deadline, ok := req.Context().Deadline()
				hasDeadline, remaining = ok, time.Until(deadline)
				return base.RoundTrip(req)
			})
			ctx := t.Context()
			if tc.permit {
				ctx = context.WithValue(ctx, im.OutboundReplyContextKey{}, im.ReplyPermit{SenderID: "user-1", ExpiresAt: time.Now().Add(time.Hour)})
			}
			if err := adapter.SendTextContext(ctx, "500", "sample reply"); err != nil {
				t.Fatalf("send failed: %v", err)
			}
			if !hasDeadline {
				t.Fatal("create message request carries no deadline")
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
