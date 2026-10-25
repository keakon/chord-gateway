package main

import (
	"sort"
	"sync"

	"github.com/keakon/chord-gateway/internal/im"
)

// IMIngressMetric uses bounded label vocabularies. Caller-provided reasons, transport errors,
// account IDs and chat IDs can never create a metric label or reach metric logs.
type IMIngressMetric struct {
	Platform, Event, Reason string
	Count                   uint64
}
type IMIngressMetricsSnapshot struct {
	Counters                 []IMIngressMetric
	Pending, Bytes, Retained int
}
type ingressMetricKey struct{ platform, event, reason string }
type ingressMetrics struct {
	mu       sync.Mutex
	counters map[ingressMetricKey]uint64
}

// imMetricEventAllowed reports whether event is a bounded ingress metric event.
// record() drops anything else, so caller-provided values can never create an
// unbounded label.
func imMetricEventAllowed(event string) bool {
	switch event {
	case "acceptance_unconfirmed", "callback_dropped", "received", "duplicate", "rejected", "retryable", "uncertain", "lost_before_accept", "consumed", "dispatching", "unknown", "quarantined", "expired", "submitted", "completed", "blocked", "slot_exhausted", "reply_dropped", "receipt_update_failed":
		return true
	}
	return false
}
func (m *ingressMetrics) record(platform, event, reason string) {
	if !imMetricEventAllowed(event) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counters == nil {
		m.counters = make(map[ingressMetricKey]uint64)
	}
	m.counters[ingressMetricKey{im.MetricPlatform(platform), event, im.MetricReason(reason)}]++
}
func (s *IngressStore) MetricsSnapshot() IMIngressMetricsSnapshot {
	s.mu.Lock()
	out := IMIngressMetricsSnapshot{Pending: s.total.Pending, Bytes: s.memoryBytes, Retained: len(s.records)}
	s.mu.Unlock()
	m := &s.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, count := range m.counters {
		out.Counters = append(out.Counters, IMIngressMetric{key.platform, key.event, key.reason, count})
	}
	sort.Slice(out.Counters, func(i, j int) bool {
		a, b := out.Counters[i], out.Counters[j]
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if a.Event != b.Event {
			return a.Event < b.Event
		}
		return a.Reason < b.Reason
	})
	return out
}

// ObserveLostBeforeAccept is for transports without reliable redelivery. Call
// only when the adapter knows no input was accepted; never for uncertain.
func (s *IngressStore) ObserveLostBeforeAccept(platform, reason string) {
	s.metrics.record(platform, "lost_before_accept", reason)
}

// ObserveStateTransition runs after a successful state update.
// Route barriers can use it for transitions outside UpdateRecord.
func (s *IngressStore) ObserveStateTransition(platform, previous, state, reason string) {
	if previous != state {
		s.metrics.record(platform, state, reason)
	}
}
func (s *IngressStore) observeAcceptance(platform string, result im.ReceiveResult) {
	event := string(result.Disposition)
	if result.Disposition == im.ReceiveAccepted {
		event = "received"
	}
	s.metrics.record(platform, event, result.Reason)
}

func logIMMetrics(router *NotificationRouter, adapter im.IMAdapter, logf func(string, ...any)) {
	if router != nil && router.ingressStore != nil {
		snapshot := router.ingressStore.MetricsSnapshot()
		logf("performance ingress pending=%v bytes=%v retained=%v", snapshot.Pending, snapshot.Bytes, snapshot.Retained)
		for _, counter := range snapshot.Counters {
			logf("performance ingress platform=%v event=%v reason=%v count=%v", counter.Platform, counter.Event, counter.Reason, counter.Count)
		}
	}
	if router != nil && router.ingress != nil {
		if snapshot, ok := router.ingress.NormalizationSnapshot(); ok {
			logf("performance ingress_normalizer pending=%v keys=%v closed=%v", snapshot.Pending, snapshot.Keys, snapshot.Closed)
		}
	}
	if provider, ok := adapter.(im.IMTextMetricsProvider); ok {
		snapshot := provider.TextSendMetrics()
		logf("performance text_send platform=%v rate_waits=%v rate_wait_total=%v rate_wait_max=%v failures=%v partial_failures=%v", im.MetricPlatform(snapshot.Platform), snapshot.RateLimitWaits, snapshot.RateLimitWaitTotal, snapshot.RateLimitWaitMax, snapshot.Failures, snapshot.PartialFailures)
	}
	if multi, ok := adapter.(*MultiAdapter); ok {
		for _, child := range multi.adapters {
			logIMMetrics(nil, child, logf)
		}
	}
}
