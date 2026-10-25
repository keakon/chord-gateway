package gateway

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

// probeBindingLocked is called after identity, nonce and directory validation.
// The gate is released during network I/O, then all captured authority is checked
// again before the caller can mutate persistent routes or configuration.
func (b *BindingService) probeBindingLocked(ctx context.Context, t im.AdmissionTicket, path string, info os.FileInfo) error {
	if t.Identity.Platform != "wecom" && t.Identity.Platform != "dingtalk" {
		return nil
	}
	r := b.router
	validator, ok := r.findAdapterByType(t.Identity.Platform).(im.BindingTargetValidator)
	if !ok {
		return errors.New("platform binding probe is unavailable")
	}
	cfg := r.getConfig()
	_, generation, phase := t.Gate.Snapshot()
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
	t.Gate.OpMu.Unlock()
	err := validator.ValidateBindingTarget(probeCtx, im.BindingProbeTarget{Platform: t.Identity.Platform, AccountID: t.Identity.AccountID, ChatID: t.Identity.ChatID})
	t.Gate.OpMu.Lock()
	if err != nil {
		return errors.New("binding probe was not accepted; binding unchanged")
	}
	if probeCtx.Err() != nil {
		return probeCtx.Err()
	}
	_, currentGeneration, currentPhase := t.Gate.Snapshot()
	if cfg != r.getConfig() || generation != currentGeneration || phase != currentPhase || !r.admission.Valid(t) {
		return im.ErrPairingDenied
	}
	if r.ingressStore != nil {
		route, e := r.ingressStore.GetRoute(t.Identity)
		if e != nil && !errors.Is(e, errIngressMissing) {
			return e
		}
		if route.Revision != revision {
			return im.ErrPairingDenied
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
