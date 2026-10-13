package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/keakon/chord-gateway/config"
)

var errBindStatusUnknown = errors.New("binding status could not be refreshed")

var errBindBusy = errors.New("binding has active work")
var errBindBlocked = errors.New("binding shutdown could not be confirmed")

const bindBusyMessage = "Finish or cancel active work and wait for it to stop before binding another directory."
const bindBlockedMessage = "Binding is paused because shutdown could not be confirmed. Check the local gateway and restart."

type BindResult struct {
	Directory         string
	PreviousDirectory string
	First, Already    bool
}
type BindingService struct{ router *NotificationRouter }

func findBindingWorkspace(cfg *config.Config, path string, info os.FileInfo) (string, error) {
	found := ""
	for _, ws := range cfg.Workspaces {
		canonical, existing, err := canonicalBindingDirectory(ws.Path)
		if err != nil {
			continue
		}
		if canonical == path || os.SameFile(info, existing) {
			if found != "" {
				return "", errors.New("multiple workspaces refer to this directory")
			}
			found = ws.ID
		}
	}
	return found, nil
}

func (b *BindingService) Bind(ctx context.Context, msg IncomingMessage, t AdmissionTicket, args []string) (BindResult, error) {
	r := b.router
	result := BindResult{}
	if !r.admission.Valid(t) {
		return result, errPairingDenied
	}
	path, info, err := canonicalBindingDirectory(args[0])
	if err != nil {
		return result, err
	}
	if r.mgr.transferBlocked("") {
		return result, errBindBusy
	}
	result.Directory = path
	g := t.gate
	g.opMu.Lock()
	defer g.opMu.Unlock()
	if !r.admission.Valid(t) {
		return result, errPairingDenied
	}
	_, generation, _ := g.snapshot()
	if generation != t.BindingGeneration {
		return result, errPairingDenied
	}
	cfg := r.getConfig()
	im := cfg.IMConfigByType(msg.IMType)
	oldID := im.Bindings()[msg.ChatID]
	if old := cfg.WorkspaceByID(oldID); old != nil {
		result.PreviousDirectory = old.Path
	}
	workspaceID, err := findBindingWorkspace(cfg, path, info)
	if err != nil {
		return result, err
	}
	if oldID != "" && oldID == workspaceID {
		result.Already = true
		return result, nil
	}
	_, _, phase := g.snapshot()
	if phase == "blocked" || phase == "switching" {
		return result, errBindBlocked
	}
	oldKey := ""
	if oldID != "" {
		oldKey = (processKey{workspaceID: oldID, imType: msg.IMType, chatID: msg.ChatID}).String()
	}
	newKey := ""
	if workspaceID != "" {
		newKey = (processKey{workspaceID: workspaceID, imType: msg.IMType, chatID: msg.ChatID}).String()
	}
	if err = r.prepareBindingSwitch(ctx, g, oldKey, newKey); err != nil {
		return result, err
	}
	result.First = im.OwnerIdentity() == nil
	_, err = r.mgr.store.Commit(ctx, ConfigMutation{
		Check: func() error {
			if !r.admission.Valid(t) {
				return errPairingDenied
			}
			return nil
		},
		Apply: func(doc *yaml.Node, current *config.Config) error {
			currentIM := current.IMConfigByType(msg.IMType)
			if currentIM == nil {
				return errPairingDenied
			}
			if owner := currentIM.OwnerIdentity(); owner != nil && (owner.AccountID != msg.AccountID || owner.UserID != msg.SenderID) {
				return errPairingDenied
			}
			id, e := findBindingWorkspace(current, path, info)
			if e != nil {
				return e
			}
			if id == "" {
				var random [16]byte
				for {
					if _, e = rand.Read(random[:]); e != nil {
						return errors.New("could not create workspace identifier")
					}
					id = "ws-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random[:]))
					if current.WorkspaceByID(id) == nil {
						break
					}
				}
			}
			root := doc.Content[0]
			ims, e := independentMapping(root, "ims")
			if e != nil {
				return e
			}
			platform, e := independentIMMapping(ims, msg.IMType)
			if e != nil {
				return e
			}
			if currentIM.OwnerIdentity() == nil {
				setMappingValue(platform, "owner", mappingNode(scalarNode("account_id", 0), scalarNode(msg.AccountID, 0), scalarNode("user_id", 0), scalarNode(msg.SenderID, 0)))
			}
			bindings, e := independentMapping(platform, "chat_bindings")
			if e != nil {
				return e
			}
			setMappingValue(bindings, msg.ChatID, scalarNode(id, 0))
			workspaces, e := independentMapping(root, "workspaces")
			if e != nil {
				return e
			}
			if current.WorkspaceByID(id) == nil {
				if e = upsertWorkspaceNode(workspaces, id, path); e != nil {
					return e
				}
			}
			workspaceID = id
			return nil
		},
	})
	if err != nil {
		if r.mgr.store.faulted.Load() {
			g.mu.Lock()
			g.phase = "blocked"
			g.mu.Unlock()
			return result, errConfigFaulted
		}
		if oldID != "" {
			g.install(t.Identity, oldID, t.AccountEpoch)
		} else {
			g.mu.Lock()
			g.phase = "unbound"
			g.mu.Unlock()
		}
		return result, err
	}
	r.admission.pairing.consume(msg.IMType)
	g.install(t.Identity, workspaceID, t.AccountEpoch)
	return result, nil
}

func (p *ChordProcess) CanRebind() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.canRebindLocked()
}

func (p *ChordProcess) canRebindLocked() bool {
	s := p.state
	return !s.Busy && len(p.pendingInputs) == 0 && s.UnconfirmedInputs == 0 && (s.PausedInputs == nil || s.PausedInputs.PendingInputCount == 0) && s.PendingLocalShells == 0 && s.RunningJobs == 0 && s.PendingConfirm == nil && s.pendingQuestionCount() == 0 && s.PendingHandoff == nil && !p.inputBlockedLocked() && p.readyForBindingLocked()
}
func (p *ChordProcess) readyForBindingLocked() bool {
	if p.readySignal == nil {
		return p.exited
	}
	select {
	case <-p.readySignal:
		return !p.bindingGuard
	default:
		return false
	}
}

func (r *NotificationRouter) dispatchPaired(msg IncomingMessage) {
	if r.admission == nil || msg.ticket == nil {
		return
	}
	t := *msg.ticket
	if !r.admission.Valid(t) {
		return
	}
	if t.Kind == "paired_bind" {
		args, err := parsePairedBindArgs(msg.Text)
		if err != nil {
			r.sendPairingReply(msg, bindUsageMessage)
			return
		}
		result, err := (&BindingService{router: r}).Bind(context.Background(), msg, t, args)
		if err != nil {
			if errors.Is(err, errPairingDenied) {
				return
			}
			text := "Binding was not changed. Check the local gateway and try again."
			if result.PreviousDirectory != "" {
				text = fmt.Sprintf("Binding was not changed. Still bound to %s. Send a new task to continue.", displayBindingDirectory(result.PreviousDirectory))
			}
			switch {
			case errors.Is(err, errBindStatusUnknown):
				text = "Could not refresh pending input state. Binding was not changed; check /status and retry."
			case errors.Is(err, errBindBusy):
				text = bindBusyMessage
			case errors.Is(err, errBindBlocked):
				text = bindBlockedMessage
			case errors.Is(err, errBindDirectory):
				text = bindDirectoryMessage
			case errors.Is(err, errConfigChanged):
				text = "Configuration changed on disk. Restart the gateway before binding."
			}
			r.sendPairingReply(msg, text)
			return
		}
		text := fmt.Sprintf("Bound to %s.", displayBindingDirectory(result.Directory))
		if result.First {
			text = fmt.Sprintf("Paired and bound to %s. Send a task to start.", displayBindingDirectory(result.Directory))
		} else if result.Already {
			text = fmt.Sprintf("Already bound to %s.", displayBindingDirectory(result.Directory))
		}
		r.sendPairingReply(msg, text)
		return
	}
	if t.Kind == "owner_unbound" {
		r.sendPairingReply(msg, "This chat is not bound. Send /bind <directory>.")
		return
	}
	t.gate.opMu.Lock()
	defer t.gate.opMu.Unlock()
	if !r.admission.Valid(t) {
		return
	}
	r.dispatchMessage(msg)
}

func (r *NotificationRouter) sendPairingReply(msg IncomingMessage, text string) {
	if msg.ticket == nil || !r.admission.Valid(*msg.ticket) || time.Since(msg.ticket.ReceivedAt) > 30*time.Second {
		return
	}
	t := *msg.ticket
	_ = r.SendFlowReply(context.Background(), ReplyPermit{Identity: t.Identity, SenderID: t.SenderID, AccountEpoch: t.AccountEpoch, RequestID: msg.MessageID, Kind: "owner", ExpiresAt: t.ReceivedAt.Add(30 * time.Second), ticket: t}, text)
}

func displayBindingDirectory(path string) string {
	runes := []rune(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, path))
	if len(runes) > 512 {
		return string(runes[:509]) + "..."
	}
	return string(runes)
}
