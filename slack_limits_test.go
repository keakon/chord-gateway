package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSlackLimitsChannelAndAccountPacing(t *testing.T) {
	limits := newSlackSendLimits()
	if limits.channelInterval != time.Second || limits.accountInterval != time.Second/5 {
		t.Fatal("production rate limits changed")
	}
	limits.channelInterval = 60 * time.Millisecond
	limits.accountInterval = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := limits.wait(ctx, "CCHAN1"); err != nil {
		t.Fatal(err)
	}
	first := time.Now()
	if err := limits.wait(ctx, "COTHER1"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(first); elapsed < 18*time.Millisecond {
		t.Fatalf("different channels bypassed account rate: %s", elapsed)
	}
	if err := limits.wait(ctx, "CCHAN1"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(first); elapsed < 58*time.Millisecond {
		t.Fatalf("channel token burst exceeded one: %s", elapsed)
	}
}

func TestSlackLimitsCancelledThreadDoesNotCancelSibling(t *testing.T) {
	limits := newSlackSendLimits()
	limits.channelInterval = time.Millisecond
	limits.accountInterval = time.Millisecond
	limits.accountNext = time.Now().Add(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := limits.wait(ctx, "CCHAN1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait was not cancelled: %v", err)
	}
	sibling, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := limits.wait(sibling, "CCHAN1"); err != nil {
		t.Fatalf("cancelled waiter affected sibling: %v", err)
	}
}
