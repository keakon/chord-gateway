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

func TestFeishuSendTextContextControlBudget(t *testing.T) {
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
			transport := feishuRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				defer req.Body.Close()
				deadline, _ := req.Context().Deadline()
				deadlines = append(deadlines, deadline)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"message_id":"sample-message"}}`)),
				}, nil
			})
			a := feishuTextBudgetAdapter(t, transport)
			ctx, cancel := context.WithTimeout(t.Context(), tc.parentBudget)
			defer cancel()
			if tc.permit {
				ctx = context.WithValue(ctx, im.OutboundReplyContextKey{}, im.ReplyPermit{SenderID: "user-1", ExpiresAt: time.Now().Add(time.Hour)})
			}
			parentDeadline, _ := ctx.Deadline()
			start := time.Now()
			if err := a.SendTextContext(ctx, "chat-1", strings.Repeat("a", 3*feishuMaxTextLen)); err != nil {
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
