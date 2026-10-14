package main

import (
	"testing"
	"time"
)

type testIngressReadiness struct {
	id     ChatIdentity
	result IngressDeliveryReadiness
}

func (p *testIngressReadiness) IngressDeliveryReadiness(id ChatIdentity) IngressDeliveryReadiness {
	p.id = id
	return p.result
}

func TestIngressDeliveryReadinessOptionalAndBounded(t *testing.T) {
	id := ChatIdentity{Platform: "wechat", AccountID: "account", ChatID: "chat"}
	for _, provider := range []any{nil, struct{}{}} {
		if got := checkIngressDeliveryReadiness(provider, id); !got.Ready || got.Reason != "" {
			t.Fatal(got)
		}
	}
	for _, tc := range []struct {
		name, reason, want string
		ready              bool
	}{
		{"ready", "ignored", "", true},
		{"context", IngressWaitingReplyContext, IngressWaitingReplyContext, false},
		{"account", IngressWaitingAccount, IngressWaitingAccount, false},
		{"platform", IngressWaitingPlatform, IngressWaitingPlatform, false},
		{"unknown", "arbitrary secret payload", IngressWaitingCondition, false},
		{"unset", "", IngressWaitingCondition, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &testIngressReadiness{result: IngressDeliveryReadiness{Ready: tc.ready, Reason: tc.reason}}
			got := checkIngressDeliveryReadiness(provider, id)
			if got.Ready != tc.ready || got.Reason != tc.want || provider.id != id {
				t.Fatalf("condition %v, identity %v", got, provider.id)
			}
		})
	}
}

func TestIngressDeliveryExpiryUsesOriginalReceivedTime(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		received time.Time
		expired  bool
	}{
		{time.Time{}, true},
		{now.Add(-24 * time.Hour), true},
		{now.Add(-24*time.Hour + time.Nanosecond), false},
		{now.Add(-25 * time.Hour), true},
	} {
		if got := ingressDeliveryExpired(tc.received, now); got != tc.expired {
			t.Errorf("received %s: got %t", tc.received, got)
		}
	}
}
