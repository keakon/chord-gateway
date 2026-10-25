package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func testChatOperation(t *testing.T, m *ChordManager, key string) *chatOperation {
	t.Helper()
	operation, err := m.captureChatOperation(key)
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func bindTestOwner(t *testing.T, r *NotificationRouter, code, directory string) (im.IncomingMessage, string) {
	t.Helper()
	msg := im.IncomingMessage{IMType: "wechat", AccountID: "bot", ChatKind: "private", ChatID: "chat", SenderID: "owner", MessageID: "pair", Text: fmt.Sprintf("/bind %q %s", directory, code)}
	if !dispatchPairingTest(r, msg) {
		t.Fatal("owner not admitted")
	}
	ws, err := r.getConfig().ResolveWorkspace("wechat", "chat")
	if err != nil {
		t.Fatal(err)
	}
	return msg, (processKey{workspaceID: ws.ID, imType: "wechat", chatID: "chat"}).String()
}

func TestTakeoverPreflightKeepsOriginalBinding(t *testing.T) {
	for _, change := range []string{"rebind", "return_to_directory", "account_change"} {
		t.Run(change, func(t *testing.T) {
			r, code := pairedTestRouter(t)
			first, second := t.TempDir(), t.TempDir()
			msg, key := bindTestOwner(t, r, code, first)
			operation := testChatOperation(t, r.mgr, key)
			ws, _ := r.getConfig().ResolveWorkspace("wechat", "chat")
			entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			starts := make(chan struct{}, 1)
			r.mgr.takeoverCLI = func(_ context.Context, _ string, req takeoverRequest) (TransferStatus, error) {
				if req.Action != transferStatePreflight {
					starts <- struct{}{}
					return TransferStatus{}, errors.New("unexpected mutation")
				}
				close(entered)
				<-resume // Also exercise a late response from an uncancellable transport.
				return TransferStatus{State: transferStatePreflight, Workspace: req.Workspace, SourceRuntimeID: "source", TransferID: req.TransferID, SessionID: req.SessionID, TargetKind: "gateway", TargetInstanceID: req.TargetInstanceID}, nil
			}
			go func() {
				defer close(done)
				r.prepareIncomingTakeover(ws, "chat", key, parseTakeoverCommand("session-example"), operation)
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("preflight did not start")
			}
			if change == "account_change" {
				if err := r.admission.pairing.PlatformReady("wechat", "another-bot"); err == nil {
					t.Fatal("account change accepted")
				}
			} else {
				msg.MessageID = "switch"
				msg.Text = fmt.Sprintf("/bind %q", second)
				dispatchPairingTest(r, msg)
				current, _ := r.getConfig().ResolveWorkspace("wechat", "chat")
				canonical, _, _ := canonicalBindingDirectory(second)
				if current.Path != canonical {
					close(resume)
					<-done
					t.Fatal("directory not switched")
				}
				if change == "return_to_directory" {
					msg.MessageID = "return"
					msg.Text = fmt.Sprintf("/bind %q", first)
					dispatchPairingTest(r, msg)
				}
			}
			close(resume)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("old preflight did not exit")
			}
			if _, ok, err := r.mgr.transfers.Get(key); err != nil || ok {
				t.Fatal("stale preflight persisted a transfer guard")
			}
			select {
			case <-starts:
				t.Fatal("stale preflight started transfer")
			default:
			}
		})
	}
}

func TestTakeoverObserverRejectsOldBinding(t *testing.T) {
	for _, state := range []string{"waiting_source", "restoring"} {
		t.Run(state, func(t *testing.T) {
			r, code := pairedTestRouter(t)
			msg, key := bindTestOwner(t, r, code, t.TempDir())
			ws, _ := r.getConfig().ResolveWorkspace("wechat", "chat")
			operation := testChatOperation(t, r.mgr, key)
			msg.MessageID = "switch"
			msg.Text = fmt.Sprintf("/bind %q", t.TempDir())
			dispatchPairingTest(r, msg)
			record := takeoverTestRecord(state)
			record.Direction = "incoming"
			if err := r.mgr.transfers.update(key, &record, ""); err != nil {
				t.Fatal(err)
			}
			calls := 0
			r.mgr.takeoverCLI = func(context.Context, string, takeoverRequest) (TransferStatus, error) {
				calls++
				return TransferStatus{}, errors.New("unexpected CLI call")
			}
			r.observeTakeover(ws, "chat", key, true, nil, operation)
			if calls != 0 || r.mgr.GetProcessForKey(key) != nil {
				t.Fatal("old observer executed transfer work")
			}
			if !r.mgr.transferBlocked(key) {
				t.Fatal("stale observer cleared durable guard")
			}
		})
	}
}

func TestConsoleRepliesWithAdmissionAndMultipleAdapters(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprint(multi), func(t *testing.T) {
			s := testConfigStore(t, fmt.Sprintf("chord_path: %q\nims: {wechat: {}}\nworkspaces: {main: {path: %q}}\n", filepath.Join(t.TempDir(), "unavailable-chord"), t.TempDir()))
			paths := &config.Paths{StateDir: t.TempDir()}
			m := NewChordManager(s.Snapshot(), paths)
			m.store = s
			r := NewNotificationRouter(m)
			r.admission = NewAdmission(NewPairingService(s, false, nil))
			m.admission = r.admission
			defer r.Close()
			defer r.admission.Close()
			a, err := NewWechatAdapter(s.Snapshot(), *s.Snapshot().IMConfigByType("wechat"), paths, r)
			if err != nil {
				t.Fatal(err)
			}
			feishu := &stubIMAdapter{typ: "feishu"}
			if multi {
				r.SetAdapter(&MultiAdapter{adapters: []im.IMAdapter{feishu, a}, router: r})
			} else {
				r.SetAdapter(a)
			}
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			original := os.Stdout
			os.Stdout = writer
			defer func() { os.Stdout = original; reader.Close(); writer.Close() }()
			r.HandleIncomingMessage(im.IncomingMessage{IMType: "console", ChatID: "console", SenderID: "console", Text: "/sessions"})
			// An IM login cannot redirect output from an existing console session.
			a.token.Store(&TokenData{AccountID: "bot", BaseURL: "https://example.invalid"})
			key := (processKey{workspaceID: "main", imType: "console", chatID: "console"}).String()
			if !r.sendTextForKey(key, "console", "task result") {
				t.Fatal("console task output failed")
			}
			writer.Close()
			os.Stdout = original
			output, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(output), "Could not query sessions") || !strings.Contains(string(output), "[console] task result") {
				t.Fatalf("missing console output: %s", output)
			}
			if len(feishu.sentMessages()) != 0 {
				t.Fatal("console output reached Feishu")
			}
		})
	}
}

func TestPairingAcceptsEmptyMappingNodes(t *testing.T) {
	for _, empty := range []string{"null", "", "{}"} {
		t.Run(empty, func(t *testing.T) {
			r, code := pairedTestRouter(t)
			s := testConfigStore(t, fmt.Sprintf("ims:\n  wechat:\n        chat_bindings: %s\nworkspaces: %s\n", empty, empty))
			r.mgr.store = s
			r.admission.pairing.store = s
			_, key := bindTestOwner(t, r, code, t.TempDir())
			reloaded, err := NewConfigStore(s.path)
			if err != nil {
				t.Fatal(err)
			}
			owner := reloaded.Snapshot().IMConfigByType("wechat").OwnerIdentity()
			if owner == nil || owner.UserID != "owner" || len(reloaded.Snapshot().Workspaces) != 1 {
				t.Fatal("pairing not persisted")
			}
			if key == "" {
				t.Fatal("missing process key")
			}
		})
	}
}

func TestTakeoverObserverDiscardsLateAccountResult(t *testing.T) {
	r, code := pairedTestRouter(t)
	_, key := bindTestOwner(t, r, code, t.TempDir())
	ws, _ := r.getConfig().ResolveWorkspace("wechat", "chat")
	operation := testChatOperation(t, r.mgr, key)
	record := takeoverTestRecord("waiting_source")
	record.Direction = "incoming"
	record.Status.TargetKind = "gateway"
	if err := r.mgr.transfers.update(key, &record, ""); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.mgr.takeoverCLI = func(context.Context, string, takeoverRequest) (TransferStatus, error) {
		calls++
		_ = r.admission.pairing.PlatformReady("wechat", "another-bot")
		result := record.Status
		result.State = transferStatePrepareFailed
		return result, nil
	}
	r.observeTakeover(ws, "chat", key, true, nil, operation)
	if calls != 1 || !r.mgr.transferBlocked(key) {
		t.Fatal("late account result cleared transfer guard")
	}
}

func TestCancelledTakeoverRestorePreservesGuard(t *testing.T) {
	r, code := pairedTestRouter(t)
	_, key := bindTestOwner(t, r, code, t.TempDir())
	record := takeoverTestRecord("released")
	record.Direction = "incoming"
	if err := r.mgr.transfers.update(key, &record, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.mgr.restoreTransferredSession(ctx, key, record); !errors.Is(err, context.Canceled) {
		t.Fatalf("restore returned %v", err)
	}
	got, ok, err := r.mgr.transfers.Get(key)
	if err != nil || !ok || got.Status.State != "released" || r.mgr.GetProcessForKey(key) != nil {
		t.Fatal("cancelled restore changed durable or runtime state")
	}
}
