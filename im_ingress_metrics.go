package main

import (
	"sort"
	"sync"
	"time"
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

func imMetricPlatform(platform string) string {
	switch platform {
	case "telegram", "discord", "wecom", "dingtalk", "slack", "wechat", "feishu":
		return platform
	}
	return "other"
}
func imMetricReason(reason string) string {
	switch reason {
	case "control_target_unavailable", "binding_unavailable", "binding_outcome_unknown", "authorization_changed", "outbound_full", "outbound_closed", "reply_expired":
		return reason
	case "invalid_identity", "identity_probe", "identity_unverified", "identity_probe_requires_private",
		"sender_identity_unsupported", "unsupported_conversation", "unsupported_message",
		"group_mention_required", "message_too_large", "invalid_message_id",
		"update_id_conflict", "semantic_duplicate", "unsupported_update", "unsupported_topic", "unsupported_sender_chat", "unsupported_chat", "unsupported_content", "other_bot_command", "invalid_command_entity", "chat_migrated", "credentials", "forbidden", "poller_conflict", "unknown_send", "platform_rejected", "backpressure", "connection_lost", "connection_unavailable", "auth_failed", "duplicate_connection", "identity_mismatch", "invalid_message", "invalid_callback", "local_error":
		return reason
	case "":
		return "none"
	case "invalid_input", "invalid_kind", "invalid_state", "ingress_unavailable", "source_time_out_of_range", "capacity", "route_changed", "route_changing", "route_busy", "route_unavailable", "route_outcome_unknown", "route_change_unavailable", "stream_capacity", "authorization_denied", "admission_unavailable", "platform_unavailable", "chat_unbound", "pairing_hint_text", "pairing_hint_usage", "pairing_hint_code", "received_expired", "route_identity_changed", "execution_outcome_unknown", "receipt_timeout", "process_exited", "delivery_failed", "cancelled", "invalid_target", "invalid_text", "invalid_rate_limit", "invalid_input_digest", "invalid_cursor", "received_time_out_of_range", "cursor_generation_conflict", "cursor_platform_invalid", "invalid_interaction", "target_busy", "capture_busy", "rejection_not_committed", "observer_registration_failed", "stdin_outcome_unknown", "session_restore_failed", "session_pin_persistence_failed", "session_ready_pin_unconfirmed", "control_parameters_unavailable", "new_done_confirmation", "transfer_guard":
		return reason
	}
	return "other"
}
func (m *ingressMetrics) record(platform, event, reason string) {
	switch event {
	case "acceptance_unconfirmed", "callback_dropped", "received", "duplicate", "rejected", "retryable", "uncertain", "lost_before_accept", "consumed", "dispatching", "unknown", "quarantined", "expired", "submitted", "completed", "blocked", "slot_exhausted", "reply_dropped", "receipt_update_failed":
	default:
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counters == nil {
		m.counters = make(map[ingressMetricKey]uint64)
	}
	m.counters[ingressMetricKey{imMetricPlatform(platform), event, imMetricReason(reason)}]++
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
func (s *IngressStore) observeAcceptance(platform string, result ReceiveResult) {
	event := string(result.Disposition)
	if result.Disposition == ReceiveAccepted {
		event = "received"
	}
	s.metrics.record(platform, event, result.Reason)
}

// IMTextMetricsProvider exposes bounded per-target send diagnostics.
// Adapters expose these optional methods by forwarding their shared sender.
type IMTextMetricsProvider interface {
	TextSendMetrics() IMTextMetricsSnapshot
	TextSendTargetSnapshot(target string) IMTextTargetSnapshot
}
type IMTextMetricsSnapshot struct {
	Platform                                  string
	RateLimitWaits, Failures, PartialFailures uint64
	RateLimitWaitTotal, RateLimitWaitMax      time.Duration
}
type IMTextTargetSnapshot struct {
	LastFailureCategory  string
	LastAcceptedSegments int
	LastFailureAt        time.Time
}
type imTextMetrics struct {
	mu       sync.Mutex
	snapshot IMTextMetricsSnapshot
	targets  map[string]IMTextTargetSnapshot
}

func (s *IMTextSender) TextSendMetrics() IMTextMetricsSnapshot {
	s.metrics.mu.Lock()
	defer s.metrics.mu.Unlock()
	snapshot := s.metrics.snapshot
	snapshot.Platform = imMetricPlatform(s.options.Platform)
	return snapshot
}
func (s *IMTextSender) TextSendTargetSnapshot(target string) IMTextTargetSnapshot {
	s.metrics.mu.Lock()
	defer s.metrics.mu.Unlock()
	return s.metrics.targets[target]
}
func (s *IMTextSender) observeSendFailure(target string, accepted int, category string) {
	m := &s.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot.Failures++
	if accepted == 0 {
		return
	}
	m.snapshot.PartialFailures++
	if m.targets == nil {
		m.targets = make(map[string]IMTextTargetSnapshot)
	}
	if _, exists := m.targets[target]; !exists && len(m.targets) >= 128 {
		oldest := ""
		var before time.Time
		for key, value := range m.targets {
			if oldest == "" || value.LastFailureAt.Before(before) {
				oldest = key
				before = value.LastFailureAt
			}
		}
		delete(m.targets, oldest)
	}
	m.targets[target] = IMTextTargetSnapshot{imMetricReason(category), accepted, time.Now()}
}
func (s *IMTextSender) observeRateWait(wait time.Duration) {
	m := &s.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot.RateLimitWaits++
	m.snapshot.RateLimitWaitTotal += wait
	if wait > m.snapshot.RateLimitWaitMax {
		m.snapshot.RateLimitWaitMax = wait
	}
}
func logIMMetrics(router *NotificationRouter, adapter IMAdapter, logf func(string, ...any)) {
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
	if provider, ok := adapter.(IMTextMetricsProvider); ok {
		snapshot := provider.TextSendMetrics()
		logf("performance text_send platform=%v rate_waits=%v rate_wait_total=%v rate_wait_max=%v failures=%v partial_failures=%v", imMetricPlatform(snapshot.Platform), snapshot.RateLimitWaits, snapshot.RateLimitWaitTotal, snapshot.RateLimitWaitMax, snapshot.Failures, snapshot.PartialFailures)
	}
	if multi, ok := adapter.(*MultiAdapter); ok {
		for _, child := range multi.adapters {
			logIMMetrics(nil, child, logf)
		}
	}
}
