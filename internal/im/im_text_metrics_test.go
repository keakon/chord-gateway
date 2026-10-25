package im

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIMTextSendMetricsAndBoundedTargetDiagnostics(t *testing.T) {
	calls := 0
	s := testIMTextSender(t, 5, func(context.Context, string, string) error {
		calls++
		if calls == 2 {
			return &IMRateLimitError{RetryAfter: time.Millisecond}
		}
		if calls == 3 {
			return errors.New("secret transport error")
		}
		return nil
	})
	if err := s.Send(context.Background(), nil, nil, "secret-chat", "aaaaabbbbb", false); err == nil {
		t.Fatal("failure ignored")
	}
	snapshot := s.TextSendMetrics()
	if snapshot.RateLimitWaits != 1 || snapshot.RateLimitWaitTotal <= 0 || snapshot.PartialFailures != 1 || snapshot.Failures != 1 {
		t.Fatalf("missing send metrics: %+v", snapshot)
	}
	target := s.TextSendTargetSnapshot("secret-chat")
	if target.LastAcceptedSegments != 1 || target.LastFailureCategory != "delivery_failed" || target.LastFailureAt.IsZero() {
		t.Fatalf("missing target diagnostics: %+v", target)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(raw), "secret") {
		t.Fatalf("private metrics: %s %v", raw, err)
	}
	for i := range 130 {
		s.observeSendFailure(fmt.Sprint(i), 1, "private arbitrary error")
	}
	if s.DiagnosticTargetCountForTest() != 128 {
		t.Fatal("target diagnostics unbounded")
	}
	if got := s.TextSendTargetSnapshot("129"); got.LastFailureCategory != "other" {
		t.Fatalf("unfiltered diagnostic: %+v", got)
	}
}

// Transport failure categories are metric labels too, so the bounded set a
// classifier may report and the metrics vocabulary must agree.
func TestFailureCategoriesAreMetricReasons(t *testing.T) {
	for _, category := range imTextFailureCategories {
		if got := MetricReason(category); got != category {
			t.Errorf("failure category %q collapses to %q in send diagnostics", category, got)
		}
	}
}
