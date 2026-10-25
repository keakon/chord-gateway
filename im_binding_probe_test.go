package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"

	"github.com/keakon/chord-gateway/config"
)

type bindingProbeAdapter struct {
	pairingTestAdapter
	probe func(context.Context, im.BindingProbeTarget) error
	calls int
}

func (a *bindingProbeAdapter) Type() string { return "wecom" }
func (a *bindingProbeAdapter) ValidateBindingTarget(ctx context.Context, target im.BindingProbeTarget) error {
	a.calls++
	return a.probe(ctx, target)
}

func TestBindingProbeRevalidatesAuthority(t *testing.T) {
	for _, scenario := range []string{"success", "failure", "account_changed", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			store := testConfigStore(t, "ims: {wecom: {bot_id: bot, secret: placeholder}}\n")
			mgr := NewChordManager(store.Snapshot(), &config.Paths{StateDir: t.TempDir()})
			mgr.store = store
			r := NewNotificationRouter(mgr)
			var terminal bytes.Buffer
			pairing := NewPairingService(store, true, &terminal)
			r.admission = NewAdmission(pairing)
			mgr.admission = r.admission
			t.Cleanup(func() { r.admission.Close(); r.Close() })
			if err := pairing.PlatformReady("wecom", "bot"); err != nil {
				t.Fatal(err)
			}
			fields := strings.Fields(terminal.String())
			code := ""
			for i, field := range fields {
				if field == `"<directory>"` {
					code = fields[i+1]
				}
			}
			dir := t.TempDir()
			msg := im.IncomingMessage{IMType: "wecom", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "message", Text: fmt.Sprintf("/bind %q %s", dir, code)}
			ticket, ok := r.admission.Authorize(msg)
			if !ok {
				t.Fatal("admission failed")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			adapter := &bindingProbeAdapter{}
			adapter.probe = func(ctx context.Context, target im.BindingProbeTarget) error {
				if target != (im.BindingProbeTarget{Platform: "wecom", AccountID: "bot", ChatID: "chat"}) {
					t.Errorf("wrong target: %+v", target)
				}
				// The callback must be able to acquire the gate: no network under opMu.
				if !ticket.Gate.OpMu.TryLock() {
					t.Error("gate held across network")
				} else {
					ticket.Gate.OpMu.Unlock()
				}
				switch scenario {
				case "failure":
					return errors.New("rejected")
				case "account_changed":
					_ = pairing.PlatformReady("wecom", "other")
				case "cancelled":
					cancel()
				}
				return nil
			}
			r.SetAdapter(adapter)
			result, err := (&BindingService{router: r}).Bind(ctx, msg, ticket, []string{dir, code})
			if scenario == "success" {
				if err != nil || !result.First {
					t.Fatalf("%+v %v", result, err)
				}
				msg.Text = fmt.Sprintf("/bind %q", dir)
				ticket, ok = r.admission.Authorize(msg)
				if !ok {
					t.Fatal("second admission")
				}
				again, err := (&BindingService{router: r}).Bind(ctx, msg, ticket, []string{dir})
				if err != nil || !again.Already {
					t.Fatalf("%+v %v", again, err)
				}
			} else if err == nil || store.Snapshot().IMConfigByType("wecom").OwnerIdentity() != nil {
				t.Fatal("probe failure committed owner")
			}
			if adapter.calls != 1 {
				t.Fatal("idempotent binding repeated probe")
			}
		})
	}
}
