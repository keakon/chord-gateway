package main

import (
	"context"

	"github.com/keakon/golog/log"
)

// The SDK can include complete inbound payloads in errors and debug messages.
// Only fixed classifications cross into the gateway log.
type feishuSafeEventLogger struct{}

func (feishuSafeEventLogger) Debug(context.Context, ...any) {}
func (feishuSafeEventLogger) Info(context.Context, ...any)  {}
func (feishuSafeEventLogger) Warn(context.Context, ...any)  { log.Warnf("feishu SDK event warning") }
func (feishuSafeEventLogger) Error(context.Context, ...any) {
	log.Errorf("feishu SDK event processing failed")
}
