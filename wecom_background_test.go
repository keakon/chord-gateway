package main

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"

	wecom "github.com/keakon/chord-gateway/internal/platform/wecom"
)

// No callback is ever submitted in this fixture. These tests verify proactive
// delivery independently of callback lifetime, not a real-account ten-minute
// task or the platform's actual quota and product permissions.
func wecomBackgroundTarget(t *testing.T, r *NotificationRouter, a *wecom.WecomAdapter, msg im.IncomingMessage, kind string) (key, rawTarget string) {
	t.Helper()
	chat := msg.ChatID
	rawTarget = "owner"
	if kind == "group" {
		rawTarget = "background-group"
		chat, _ = wecom.EncodeWecomTarget(kind, rawTarget)
		cfg := *r.getConfig()
		cfg.IMs = slices.Clone(cfg.IMs)
		for i := range cfg.IMs {
			if cfg.IMs[i].Wecom != nil {
				wc := *cfg.IMs[i].Wecom
				wc.ChatBindings = maps.Clone(wc.ChatBindings)
				wc.ChatBindings[chat] = "project"
				cfg.IMs[i].Wecom = &wc
			}
		}
		r.mgr.store.snapshot.Store(&cfg)
		r.ensureOwnedGate("wecom", chat, "project")
	}
	oldKey := (processKey{workspaceID: "project", imType: "wecom", chatID: msg.ChatID}).String()
	key = (processKey{workspaceID: "project", imType: "wecom", chatID: chat}).String()
	if key != oldKey {
		r.mgr.mu.Lock()
		p := r.mgr.procs[oldKey]
		delete(r.mgr.procs, oldKey)
		r.mgr.procs[key] = p
		r.mgr.mu.Unlock()
		p.mu.Lock()
		p.key = key
		p.mu.Unlock()
	}
	return key, rawTarget
}

func TestWecomFinalAndBackgroundResultsWithoutAnyCallback(t *testing.T) {
	for _, kind := range []string{"private", "group"} {
		t.Run(kind, func(t *testing.T) {
			a, msg, store, r := wecomAdapterFixture(t)
			key, rawTarget := wecomBackgroundTarget(t, r, a, msg, kind)
			var mu sync.Mutex
			var targets, texts []string
			startWecomAdapterTestWS(t, a, func(target, text string) int {
				mu.Lock()
				defer mu.Unlock()
				targets = append(targets, target)
				texts = append(texts, text)
				return 0
			})
			final := strings.Repeat("长任务😀", 600)
			background := ControlState{LastBackgroundResult: &BackgroundResultPayload{Content: "background job finished"}}
			r.HandleChordEvent(key, "assistant_message", ControlState{LastAssistantText: final})
			r.HandleChordEvent(key, "background_result", background)
			r.Close() // Drain both notifications and their explicit send ACKs.
			expected, err := a.TextSenderForTest().Segments(final)
			if err != nil {
				t.Fatal(err)
			}
			expected = append(expected, wecom.RenderTextForTest(r.formatNotification("background_result", background)))
			mu.Lock()
			defer mu.Unlock()
			if len(expected) < 4 || !slices.Equal(texts, expected) {
				t.Fatalf("final/background ordering or segmentation failed: got %d, want %d", len(texts), len(expected))
			}
			for _, target := range targets {
				if target != rawTarget {
					t.Fatalf("result sent to %q instead of original %q", target, rawTarget)
				}
			}
			if countWecomIngressEvent(store, "received") != 0 {
				t.Fatal("proactive result test unexpectedly depended on callback acceptance")
			}
		})
	}
}

func TestWecomBackgroundResultVisibilityUsesHeadlessSubscription(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "default"
		if !enabled {
			name = "disabled"
		}
		t.Run(name, func(t *testing.T) {
			a, msg, _, r := wecomAdapterFixture(t)
			if !enabled {
				cfg := *r.getConfig()
				cfg.EventVisibility.BackgroundResult = &enabled
				r.mgr.store.snapshot.Store(&cfg)
			}
			var mu sync.Mutex
			var texts []string
			startWecomAdapterTestWS(t, a, func(_, text string) int {
				mu.Lock()
				defer mu.Unlock()
				texts = append(texts, text)
				return 0
			})
			key := (processKey{workspaceID: "project", imType: "wecom", chatID: msg.ChatID}).String()
			// Visibility is enforced at the existing subscription boundary. Feed
			// only subscribed Chord events through the normal router entry point.
			subscribed := slices.Contains(configuredHeadlessSubscribeEvents(r.getConfig()), "background_result")
			if subscribed != enabled {
				t.Fatalf("background subscription=%v, want %v", subscribed, enabled)
			}
			if subscribed {
				r.HandleChordEvent(key, "background_result", ControlState{LastBackgroundResult: &BackgroundResultPayload{Content: "background visibility sample"}})
			}
			r.HandleChordEvent(key, "assistant_message", ControlState{LastAssistantText: "final remains visible"})
			r.Close()
			mu.Lock()
			defer mu.Unlock()
			want := 1
			if enabled {
				want++
			}
			if len(texts) != want || texts[len(texts)-1] != "final remains visible" {
				t.Fatalf("visibility output=%q", texts)
			}
		})
	}
}

func TestWecomBackgroundResultsRejectUnboundAndRetiredTargets(t *testing.T) {
	for _, scenario := range []string{"unbound", "retired_binding", "queued_replaced_runtime"} {
		t.Run(scenario, func(t *testing.T) {
			a, msg, _, r := wecomAdapterFixture(t)
			var mu sync.Mutex
			sends := 0
			startWecomAdapterTestWS(t, a, func(_, _ string) int { mu.Lock(); defer mu.Unlock(); sends++; return 0 })
			key := (processKey{workspaceID: "project", imType: "wecom", chatID: msg.ChatID}).String()
			state := ControlState{LastBackgroundResult: &BackgroundResultPayload{Content: "must not reach replacement"}}
			switch scenario {
			case "unbound":
				chat, _ := wecom.EncodeWecomTarget("group", "unbound-group")
				key = (processKey{workspaceID: "project", imType: "wecom", chatID: chat}).String()
				r.HandleChordEvent(key, "background_result", state)
			case "retired_binding":
				target, ok := r.targetForKey(key)
				if !ok {
					t.Fatal("fixture target missing")
				}
				target.Lease.Close()
				r.HandleChordEvent(key, "background_result", state)
			case "queued_replaced_runtime":
				blocked, release := make(chan struct{}), make(chan struct{})
				if r.outbound.enqueue(key, func() { close(blocked); <-release }) != outboundQueued {
					t.Fatal("could not block notification shard")
				}
				<-blocked
				r.HandleChordEvent(key, "background_result", state)
				r.mgr.mu.Lock()
				r.mgr.procs[key] = &ChordProcess{bindingInstanceID: "replacement-runtime"}
				r.mgr.mu.Unlock()
				close(release)
			}
			r.Close()
			mu.Lock()
			defer mu.Unlock()
			if sends != 0 {
				t.Fatalf("unauthorized background result sent: %d", sends)
			}
		})
	}
}
