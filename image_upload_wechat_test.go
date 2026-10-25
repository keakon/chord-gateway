package main

import (
	"context"
	"errors"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestWechatImageCancellationBeforeReading(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a := newTestWechatAdapter(t)
	a.SetConnectionForTest(ctx, nil)
	if err := a.SendImage("sample-chat", &im.ReceivedImage{Path: "missing-image"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled upload = %v", err)
	}
	// Slot occupancy is asserted in internal/im/image_upload_test.go.
}
