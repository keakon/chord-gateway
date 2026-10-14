package main

import (
	"context"
	"errors"
	"os"
	"time"
)

// BindingProbeTarget authorizes only the adapter's fixed diagnostic text. It
// deliberately cannot carry content, a URL, or a workspace operation.
type BindingProbeTarget struct{ Platform, AccountID, ChatID string }
type BindingTargetValidator interface {
	ValidateBindingTarget(context.Context, BindingProbeTarget) error
}

// probeBindingLocked is called after identity, nonce and directory validation.
// The gate is released during network I/O, then all captured authority is checked
// again before the caller can mutate persistent routes or configuration.
func (b *BindingService) probeBindingLocked(ctx context.Context, t AdmissionTicket, path string, info os.FileInfo) error {
	if t.Identity.Platform != "wecom" && t.Identity.Platform != "dingtalk" {
		return nil
	}
	r := b.router
	validator, ok := r.findAdapterByType(t.Identity.Platform).(BindingTargetValidator)
	if !ok {
		return errors.New("platform binding probe is unavailable")
	}
	cfg := r.getConfig()
	_, generation, phase := t.gate.snapshot()
	revision := ""
	if r.ingressStore != nil {
		route, err := r.ingressStore.GetRoute(t.Identity)
		if err != nil && !errors.Is(err, errIngressMissing) {
			return err
		}
		revision = route.Revision
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	t.gate.opMu.Unlock()
	err := validator.ValidateBindingTarget(probeCtx, BindingProbeTarget{t.Identity.Platform, t.Identity.AccountID, t.Identity.ChatID})
	t.gate.opMu.Lock()
	if err != nil {
		return errors.New("binding probe was not accepted; binding unchanged")
	}
	if probeCtx.Err() != nil {
		return probeCtx.Err()
	}
	_, currentGeneration, currentPhase := t.gate.snapshot()
	if cfg != r.getConfig() || generation != currentGeneration || phase != currentPhase || !r.admission.Valid(t) {
		return errPairingDenied
	}
	if r.ingressStore != nil {
		route, e := r.ingressStore.GetRoute(t.Identity)
		if e != nil && !errors.Is(e, errIngressMissing) {
			return e
		}
		if route.Revision != revision {
			return errPairingDenied
		}
	}
	canonical, currentInfo, e := canonicalBindingDirectory(path)
	if e != nil || canonical != path || !os.SameFile(info, currentInfo) {
		return errors.New("binding directory changed during probe")
	}
	if r.mgr.transferBlocked("") {
		return errBindBusy
	}
	return nil
}
