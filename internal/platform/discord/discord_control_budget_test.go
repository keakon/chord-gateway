package discord

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestDiscordSendTextContextControlBudget(t *testing.T) {
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"id":"999","channel_id":"500"}`)
			}))
			defer server.Close()
			adapter := discordRESTAdapterForTest(t, server)
			// The server-side request context carries no client deadline, so
			// capture it on the client side of the transport.
			base := adapter.rest.HTTPClient().Transport
			adapter.rest.HTTPClient().Transport = discordRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				deadline, _ := req.Context().Deadline()
				deadlines = append(deadlines, deadline)
				return base.RoundTrip(req)
			})
			ctx, cancel := context.WithTimeout(t.Context(), tc.parentBudget)
			defer cancel()
			if tc.permit {
				ctx = context.WithValue(ctx, im.OutboundReplyContextKey{}, im.ReplyPermit{SenderID: "user-1", ExpiresAt: time.Now().Add(time.Hour)})
			}
			parentDeadline, _ := ctx.Deadline()
			start := time.Now()
			if err := adapter.SendTextContext(ctx, "500", strings.Repeat("a", 3*1900)); err != nil {
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
