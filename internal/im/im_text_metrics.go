package im

import (
	"context"
	"sync"
	"time"
)

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

// MetricPlatform maps a platform identifier onto the bounded metric label set.
func MetricPlatform(platform string) string {
	switch platform {
	case "telegram", "discord", "wecom", "dingtalk", "slack", "wechat", "feishu":
		return platform
	}
	return "other"
}

// MetricReason maps a caller-provided reason onto the bounded metric label
// vocabulary; anything unlisted collapses into "other" so call-provided values
// can never create an unbounded label.
func MetricReason(reason string) string {
	switch reason {
	case "control_target_unavailable", "binding_unavailable", "binding_outcome_unknown", "authorization_changed", "outbound_full", "outbound_closed", "reply_expired":
		return reason
	case "invalid_identity", "invalid_configuration", "identity_probe", "identity_unverified", "identity_probe_requires_private",
		"sender_identity_unsupported", "unsupported_conversation", "unsupported_message",
		"group_mention_required", "message_too_large", "invalid_message_id",
		"update_id_conflict", "semantic_duplicate", "unsupported_update", "unsupported_topic", "unsupported_sender_chat", "unsupported_chat", "unsupported_content", "other_bot_command", "invalid_command_entity", "chat_migrated", "credentials", "forbidden", "poller_conflict", "unknown_send", "platform_rejected", "backpressure", "connection_lost", "connection_unavailable", "auth_failed", "duplicate_connection", "identity_mismatch", "invalid_message", "invalid_callback", "local_error", "api_rejected", "invalid_request":
		return reason
	case "":
		return "none"
	case "invalid_input", "invalid_kind", "invalid_state", "ingress_unavailable", "storage_unavailable", "source_time_out_of_range", "capacity", "route_changed", "route_changing", "route_busy", "route_unavailable", "route_outcome_unknown", "route_change_unavailable", "stream_capacity", "authorization_denied", "admission_unavailable", "platform_unavailable", "chat_unbound", "pairing_hint_text", "pairing_hint_usage", "pairing_hint_code", "received_expired", "route_identity_changed", "execution_outcome_unknown", "receipt_timeout", "process_exited", "delivery_failed", "cancelled", "invalid_target", "invalid_text", "invalid_rate_limit", "invalid_input_digest", "invalid_cursor", "received_time_out_of_range", "cursor_generation_conflict", "cursor_platform_invalid", "invalid_interaction", "target_busy", "capture_busy", "rejection_not_committed", "observer_registration_failed", "stdin_outcome_unknown", "session_restore_failed", "session_pin_persistence_failed", "session_ready_pin_unconfirmed", "control_parameters_unavailable", "new_done_confirmation", "transfer_guard":
		return reason
	case "normalization_cancelled", "normalization_failed", "normalization_result_unavailable",
		"normalization_unavailable", "normalization_wait_cancelled",
		"transport_identity_changed", "transport_ignored", "transport_stopped":
		// Raised by the shared normalizer and by transport liveness checks, and
		// reported as acceptance reasons. An unlisted value would collapse into
		// "other" and make a normalization failure look like an unknown refusal.
		return reason
	}
	return "other"
}

func (s *IMTextSender) TextSendMetrics() IMTextMetricsSnapshot {
	s.metrics.mu.Lock()
	defer s.metrics.mu.Unlock()
	snapshot := s.metrics.snapshot
	snapshot.Platform = MetricPlatform(s.options.Platform)
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
	m.targets[target] = IMTextTargetSnapshot{MetricReason(category), accepted, time.Now()}
}
func (s *IMTextSender) ObserveRateWait(wait time.Duration) {
	m := &s.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot.RateLimitWaits++
	m.snapshot.RateLimitWaitTotal += wait
	if wait > m.snapshot.RateLimitWaitMax {
		m.snapshot.RateLimitWaitMax = wait
	}
}

// SetSendSegmentForTest swaps the transport segment function. It is intended
// for tests that stage controlled transport failures; it must not be used
// while sends are in flight.
func (s *IMTextSender) SetSendSegmentForTest(segment func(context.Context, string, string) error) {
	s.options.SendSegment = segment
}

// TargetRefsForTest reports the active ordering-slot holders for one target.
func (s *IMTextSender) TargetRefsForTest(target string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.targets[target]; entry != nil {
		return entry.refs
	}
	return 0
}

// TargetCountForTest reports how many distinct targets currently hold state.
func (s *IMTextSender) TargetCountForTest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.targets)
}

// SendSegmentForTest returns the currently installed transport segment
// function so tests can wrap it.
func (s *IMTextSender) SendSegmentForTest() func(context.Context, string, string) error {
	return s.options.SendSegment
}

// DiagnosticTargetCountForTest reports how many distinct targets currently
// hold bounded failure diagnostics.
func (s *IMTextSender) DiagnosticTargetCountForTest() int {
	s.metrics.mu.Lock()
	defer s.metrics.mu.Unlock()
	return len(s.metrics.targets)
}
