package feishu

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

type feishuRoundTripFunc func(*http.Request) (*http.Response, error)

func (f feishuRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func feishuTextBudgetAdapter(t *testing.T, transport feishuRoundTripFunc) *FeishuAdapter {
	t.Helper()
	lifecycle, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	a := &FeishuAdapter{}
	a.WireForTest(config.IMAdapterConfig{Feishu: &config.FeishuConfig{AppID: "sample-app"}}, &http.Client{Transport: transport}, nil)
	a.SetLifecycleForTest(lifecycle, cancel)
	a.SetTokenForTest("sample-token", time.Now().Add(time.Hour))
	return a
}

// TestFeishuSendTextContextControlBudget pins the reply-permit deadline
// derivation: a flow reply must inherit the 15s control-send budget instead
// of the five-minute proactive budget.
func TestFeishuSendTextContextControlBudget(t *testing.T) {
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
			transport := feishuRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				deadline, ok := req.Context().Deadline()
				hasDeadline, remaining = ok, time.Until(deadline)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"message_id":"sample-message"}}`)),
				}, nil
			})
			a := feishuTextBudgetAdapter(t, transport)
			ctx := t.Context()
			if tc.permit {
				ctx = context.WithValue(ctx, im.OutboundReplyContextKey{}, im.ReplyPermit{SenderID: "user-1", ExpiresAt: time.Now().Add(time.Hour)})
			}
			if err := a.SendTextContext(ctx, "chat-1", "sample reply"); err != nil {
				t.Fatalf("send failed: %v", err)
			}
			if !hasDeadline {
				t.Fatal("send request carries no deadline")
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
