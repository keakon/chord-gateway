package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func ingressCounter(snapshot IMIngressMetricsSnapshot, event string) uint64 {
	var count uint64
	for _, counter := range snapshot.Counters {
		if counter.Event == event {
			count += counter.Count
		}
	}
	return count
}
func TestIngressMetricsObserveCommittedTransitionsAndAdmission(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "metrics")
	got := mustAcceptIngress(t, s, r)
	duplicate := s.AcceptRecord(context.Background(), r, nil)
	if duplicate.Disposition != ReceiveDuplicate {
		t.Fatal(duplicate)
	}
	if err := s.UpdateRecord(context.Background(), got.RecordID, []ingressState{"received"}, func(r *IngressRecord) error { r.State = "consumed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRecord(context.Background(), got.RecordID, []ingressState{"consumed"}, func(r *IngressRecord) error { r.State = "consumed"; return nil }); err != nil {
		t.Fatal(err)
	}
	snapshot := s.MetricsSnapshot()
	for _, event := range []string{"received", "duplicate", "consumed"} {
		if ingressCounter(snapshot, event) != 1 {
			t.Fatalf("wrong %s counter: %+v", event, snapshot)
		}
	}
	if snapshot.Pending != 0 || snapshot.Retained != 1 {
		t.Fatalf("missing writer metrics: %+v", snapshot)
	}
	// Snapshots are owned by callers.
	snapshot.Counters[0].Count = 1000
	if ingressCounter(s.MetricsSnapshot(), snapshot.Counters[0].Event) == 1000 {
		t.Fatal("caller modified live counters")
	}
}
func TestIngressMetricsLabelsAndLogsNeverIncludeIdentityOrErrorText(t *testing.T) {
	s := testIngressStore(t)
	for range 10 {
		s.ObserveLostBeforeAccept("secret-account", "secret-chat: raw body")
	}
	s.ObserveStateTransition("slack", "received", "quarantined", "secret-user")
	raw, err := json.Marshal(s.MetricsSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "raw body") {
		t.Fatal("sensitive metric label", string(raw))
	}
	if len(s.MetricsSnapshot().Counters) != 2 {
		t.Fatal("unbounded metric labels")
	}
	var lines []string
	var previousOutbound queueMetricsSnapshot
	logQueueMetrics(&NotificationRouter{ingressStore: s}, nil, func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }, &previousOutbound)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "event=lost_before_accept") || !strings.Contains(joined, "retained=0") || strings.Contains(joined, "secret") {
		t.Fatal("bad metrics log", joined)
	}
}
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
	if len(s.metrics.targets) != 128 {
		t.Fatal("target diagnostics unbounded")
	}
	if got := s.TextSendTargetSnapshot("129"); got.LastFailureCategory != "other" {
		t.Fatalf("unfiltered diagnostic: %+v", got)
	}
}
func TestIngressStoreControlSharesRouteCapacity(t *testing.T) {
	s := testIngressStore(t)
	r := testIngressTask(t, s, "control")
	r.Kind = "control"
	for i := range 64 {
		r.BusinessID = fmt.Sprint(i)
		mustAcceptIngress(t, s, r)
	}
	r.BusinessID = "overflow"
	if result := s.AcceptRecord(context.Background(), r, nil); result.Disposition != ReceiveRetryable || result.Reason != "capacity" {
		t.Fatalf("control bypassed route quota: %+v", result)
	}
}

// ingressReasonLiteral matches the reason fields the ingress core fills in on
// ReceiveResult, AdmissionResult and IngressRecord.
var ingressReasonLiteral = regexp.MustCompile(`Reason:\s*"([a-z0-9_]+)"`)

// producedIngressReasons reads the reasons out of the ingress sources instead of
// restating them here. A hand-maintained copy drifts, which is how the
// normalization and transport reasons were lost to "other" while this guard
// still passed.
func producedIngressReasons(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	reasons := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !strings.HasPrefix(name, "im_") && name != "wechat_ingress.go" && name != "inbound_admission.go" {
			continue
		}
		if name == "im_ingress_metrics.go" {
			// The allowlist itself: scanning it would assert nothing.
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range ingressReasonLiteral.FindAllSubmatch(source, -1) {
			reasons[string(match[1])] = name
		}
	}
	if len(reasons) < 10 {
		t.Fatalf("the reason scan found only %d literals, so it is broken", len(reasons))
	}
	return reasons
}

// TestIngressMetricVocabularyRoundTripsProducedLabels keeps the bounded metric
// vocabulary in step with what the ingress code actually emits. A produced
// reason or terminal event that is missing from the allowlist is dropped to
// "other" or discarded entirely, hiding the outcome operators need to see.
func TestIngressMetricVocabularyRoundTripsProducedLabels(t *testing.T) {
	for reason, file := range producedIngressReasons(t) {
		if got := imMetricReason(reason); got != reason {
			t.Errorf("%s emits reason %q, which the snapshot collapses to %q", file, reason, got)
		}
	}
	// Transport failure categories are metric labels too, so the bounded set a
	// classifier may report and the metrics vocabulary must agree.
	for _, category := range imTextFailureCategories {
		if got := imMetricReason(category); got != category {
			t.Errorf("failure category %q collapses to %q in send diagnostics", category, got)
		}
	}
	// Dead labels from the removed identity-scoped paths must not reserve space.
	for _, reason := range []string{"session_missing", "process_restarted", "pairing_reset", "local_discard", "stdin_failed"} {
		if imMetricReason(reason) == reason {
			t.Fatalf("dead reason still reserving a label: %q", reason)
		}
	}
	// Terminal states and saturation events are the outcomes operators act on;
	// recording one must keep it.
	s := testIngressStore(t)
	events := []string{"dispatching", "consumed", "completed", "submitted", "expired", "quarantined", "blocked", "slot_exhausted"}
	for _, event := range events {
		s.metrics.record("slack", event, "")
	}
	snapshot := s.MetricsSnapshot()
	for _, event := range events {
		if ingressCounter(snapshot, event) == 0 {
			t.Fatalf("terminal/state event dropped: %q", event)
		}
	}
}
