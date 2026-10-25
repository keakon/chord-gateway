package wechat

import (
	"fmt"
	"testing"
	"time"
)

func TestWechatPairedContextBounds(t *testing.T) {
	a := &WechatAdapter{}
	for i := 0; i < 300; i++ {
		a.cachePairedContext("bot", fmt.Sprint(i), "context", true)
	}
	if len(a.replyContexts) != 256 {
		t.Fatalf("bound cache size = %d", len(a.replyContexts))
	}
	for i := 0; i < 50; i++ {
		a.cachePairedContext("bot", fmt.Sprint(i+400), "context", false)
	}
	if len(a.replyContexts) != 286 {
		t.Fatalf("combined cache size = %d", len(a.replyContexts))
	}
	a.replyContexts["expired"] = WechatReplyContext{Expires: time.Now().Add(-time.Second)}
	a.cachePairedContext("bot", "new", "context", false)
	if _, ok := a.replyContexts["expired"]; ok {
		t.Fatal("expired flow context retained")
	}
}

func TestWechatContextPromotionKeepsCacheBounded(t *testing.T) {
	a := &WechatAdapter{}
	for i := 0; i < 256; i++ {
		a.cachePairedContext("bot", fmt.Sprint(i), "context", true)
	}
	a.cachePairedContext("bot", "new", "flow-context", false)
	a.cachePairedContext("bot", "new", "task-context", true)
	if len(a.replyContexts) != 256 {
		t.Fatalf("promotion exceeded bound: %d", len(a.replyContexts))
	}
	if !a.replyContexts[compositeKey("bot", "new")].Bound {
		t.Fatal("context was not promoted")
	}
}
