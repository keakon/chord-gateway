package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/telegram"
)

// This fake only replaces Telegram transport. Authorization, binding writeback,
// durable cursor transactions, recovery, dispatch and Chord receipts are real.
type telegramGatewayAPI struct {
	botID   int64
	probe   []telegram.Update
	batches chan []telegram.Update
	mu      sync.Mutex
	probed  bool
	sent    []string
}

func (api *telegramGatewayAPI) GetMe(context.Context) (telegram.User, error) {
	return telegram.User{ID: api.botID, IsBot: true, Username: "GatewayBot"}, nil
}
func (api *telegramGatewayAPI) GetWebhookInfo(context.Context) (telegram.WebhookInfo, error) {
	return telegram.WebhookInfo{}, nil
}
func (api *telegramGatewayAPI) GetUpdates(ctx context.Context, offset *int64, timeout int) ([]telegram.Update, error) {
	api.mu.Lock()
	if !api.probed {
		api.probed = true
		batch := api.probe
		api.mu.Unlock()
		return batch, nil
	}
	api.mu.Unlock()
	if offset != nil && timeout == 0 {
		return nil, nil
	}
	select {
	case batch := <-api.batches:
		return batch, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (api *telegramGatewayAPI) SendMessage(_ context.Context, chat int64, text string) (telegram.Message, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.sent = append(api.sent, text)
	return telegram.Message{MessageID: int64(len(api.sent)), Chat: telegram.Chat{ID: chat}}, nil
}
func telegramGatewayUpdate(id, chat int64, text string) telegram.Update {
	kind := "private"
	if chat < 0 {
		kind = "supergroup"
	}
	m := &telegram.Message{MessageID: id, Date: time.Now().Unix(), Chat: telegram.Chat{ID: chat, Type: kind}, From: &telegram.User{ID: 123456}, Text: text}
	if strings.HasPrefix(text, "/") {
		token := regexp.MustCompile(`^/[A-Za-z0-9_]+(?:@[A-Za-z0-9_]+)?`).FindString(text)
		m.Entities = []telegram.Entity{{Type: "bot_command", Offset: 0, Length: im.UTF16Units(token)}}
	}
	return telegram.Update{UpdateID: id, Message: m}
}

type telegramGatewayFixture struct {
	router    *NotificationRouter
	service   *IngressService
	paths     *config.Paths
	botID     int64
	directory string
	processes []*ChordProcess
	adapters  int
	terminal  bytes.Buffer
}

func newTelegramGatewayFixture(t *testing.T, botID int64) *telegramGatewayFixture {
	return newTelegramGatewayFixtureWithPairing(t, botID, true)
}

func newTelegramGatewayFixtureWithPairing(t *testing.T, botID int64, paired bool) *telegramGatewayFixture {
	t.Helper()
	dir := t.TempDir()
	data := fmt.Sprintf("ims:\n  telegram:\n    bot_token: test-token\n    owner: {account_id: %q, user_id: '123456'}\n    chat_bindings: {'123456': project}\nworkspaces:\n  project: {path: %q}\n", strconv.FormatInt(botID, 10), dir)
	if !paired {
		data = "ims: {telegram: {bot_token: test-token}}\n"
	}
	store := testConfigStore(t, data)
	paths := &config.Paths{StateDir: t.TempDir()}
	mgr := NewChordManager(store.Snapshot(), paths)
	mgr.store = store
	r := NewNotificationRouter(mgr)
	f := &telegramGatewayFixture{router: r, paths: paths, botID: botID, directory: dir}
	r.admission = NewAdmission(NewPairingService(store, !paired, &f.terminal))
	mgr.admission = r.admission
	db := NewIngressStore()
	s := NewIngressService(db, r)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.service = s
	t.Cleanup(func() {
		s.Close()
		for _, p := range f.processes {
			p.mu.Lock()
			for _, observer := range p.inputObservers {
				if observer.timer != nil {
					observer.timer.Stop()
				}
			}
			p.mu.Unlock()
		}
		r.admission.Close()
		r.Close()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}
func (f *telegramGatewayFixture) installProcess(t *testing.T, chat int64) (*ChordProcess, *ingressTestWriter) {
	t.Helper()
	chatID := strconv.FormatInt(chat, 10)
	ws, err := f.router.getConfig().ResolveWorkspace("telegram", chatID)
	if err != nil {
		t.Fatal(err)
	}
	key := (processKey{workspaceID: ws.ID, imType: "telegram", chatID: chatID}).String()
	writer := &ingressTestWriter{}
	ready := make(chan struct{})
	close(ready)
	p := &ChordProcess{key: key, runtimeID: "telegram-runtime", sessionEpoch: 1, stdin: writer, readySignal: ready, state: ControlState{SessionID: "telegram-session"}}
	f.router.mgr.mu.Lock()
	f.router.mgr.procs[key] = p
	f.router.mgr.mu.Unlock()
	if err = f.router.mgr.pins.Set(pinKeyForBinding(key), "telegram-session"); err != nil {
		t.Fatal(err)
	}
	f.processes = append(f.processes, p)
	return p, writer
}
func (f *telegramGatewayFixture) adapter(t *testing.T, botID int64, probe ...telegram.Update) (*telegram.TelegramAdapter, *telegramGatewayAPI) {
	t.Helper()
	cfg := f.router.getConfig()
	imCfg := *cfg.IMConfigByType("telegram")
	telegramCfg := *imCfg.Telegram
	f.adapters++
	telegramCfg.BotToken = fmt.Sprintf("test-token-%d", f.adapters)
	imCfg.Telegram = &telegramCfg
	a, err := telegram.NewTelegramAdapter(cfg, imCfg, f.paths, f.router)
	if err != nil {
		t.Fatal(err)
	}
	api := &telegramGatewayAPI{botID: botID, probe: probe, batches: make(chan []telegram.Update, 8)}
	a.SetAPIForTest(api)
	// Sender limiting has separate tests; remove time delays here to observe
	// command and admission outcomes deterministically through the real router.
	sender, err := im.NewIMTextSender(im.IMTextSendOptions{Platform: "telegram", Budget: telegram.TextBudget, Measure: im.UTF16Units, SendSegment: func(ctx context.Context, chat, text string) error {
		id, err := strconv.ParseInt(chat, 10, 64)
		if err != nil {
			return err
		}
		_, err = api.SendMessage(ctx, id, text)
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	a.SetSenderForTest(sender)
	f.router.SetAdapter(a)
	return a, api
}
func startTelegramGateway(t *testing.T, a *telegram.TelegramAdapter) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- a.Connect() }()
	t.Cleanup(func() {
		a.Disconnect()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Telegram Connect did not stop")
		}
	})
	return done
}
func telegramGatewayRecord(t *testing.T, f *telegramGatewayFixture, chat, id int64, state im.IngressState) IngressRecord {
	t.Helper()
	var result IngressRecord
	waitIngressCondition(t, func() bool {
		records, err := f.service.store.ListRouteRecords(im.ChatIdentity{Platform: "telegram", AccountID: strconv.FormatInt(f.botID, 10), ChatID: strconv.FormatInt(chat, 10)})
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range records {
			if rec.BusinessID == fmt.Sprintf("update:%d", id) && (state == "" || rec.State == state) {
				result = rec
				return true
			}
		}
		return false
	})
	return result
}

func TestTelegramGatewayProbeBatchWaitsForReady(t *testing.T) {
	f := newTelegramGatewayFixture(t, 717001)
	process, writer := f.installProcess(t, 123456)
	a, _ := f.adapter(t, f.botID, telegramGatewayUpdate(10, 123456, "probe task"))
	startTelegramGateway(t, a)
	waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 1 })
	rec := telegramGatewayRecord(t, f, 123456, 10, "dispatching")
	if rec.Text != "probe task" || writer.snapshot()[0]["content"] != "probe task" {
		t.Fatal(rec, writer.snapshot())
	}
	payload, _ := json.Marshal(InputResultPayload{RequestID: "im-" + rec.RecordID, Status: inputQueued})
	process.mu.Lock()
	process.applyInputResult(&HeadlessEnvelope{Payload: payload, Seq: 1}, time.Now())
	process.mu.Unlock()
	telegramGatewayRecord(t, f, 123456, 10, "consumed")
	cursor, err := f.service.store.Cursor("telegram", strconv.FormatInt(f.botID, 10))
	if err != nil || cursor.Telegram == nil || cursor.Telegram.LastObservedUpdateID == nil || *cursor.Telegram.LastObservedUpdateID != 10 {
		t.Fatal(cursor, err)
	}
}

func TestTelegramGatewayPrivateStatusDoesNotSpawnSession(t *testing.T) {
	f := newTelegramGatewayFixture(t, 717002)
	a, api := f.adapter(t, f.botID, telegramGatewayUpdate(20, 123456, "/status"))
	startTelegramGateway(t, a)
	telegramGatewayRecord(t, f, 123456, 20, "completed")
	f.router.mgr.mu.Lock()
	count := len(f.router.mgr.procs)
	f.router.mgr.mu.Unlock()
	if count != 0 {
		t.Fatal("status started a Chord session")
	}
	waitIngressCondition(t, func() bool { api.mu.Lock(); defer api.mu.Unlock(); return len(api.sent) > 0 })
}

func TestTelegramGatewayGroupBindingReplyAndRejectedReplay(t *testing.T) {
	f := newTelegramGatewayFixture(t, 717003)
	const chat int64 = -100987654321
	rejected := telegramGatewayUpdate(30, chat, "before binding")
	a, api := f.adapter(t, f.botID, rejected)
	startTelegramGateway(t, a)
	old := telegramGatewayRecord(t, f, chat, 30, "rejected")
	if old.Reason != "chat_unbound" || old.Text != "" {
		t.Fatal(old)
	}
	api.batches <- []telegram.Update{telegramGatewayUpdate(31, chat, fmt.Sprintf("/bind@GatewayBot %q", f.directory))}
	telegramGatewayRecord(t, f, chat, 31, "completed")
	if f.router.getConfig().IMConfigByType("telegram").Bindings()[strconv.FormatInt(chat, 10)] != "project" {
		t.Fatal("group binding missing")
	}
	process, writer := f.installProcess(t, chat)
	reply := telegramGatewayUpdate(32, chat, "reply task with /path and @mention")
	reply.Message.ReplyToMessage = &telegram.Message{MessageID: 900, From: &telegram.User{ID: f.botID, IsBot: true}}
	api.batches <- []telegram.Update{rejected, reply}
	waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 1 })
	rec := telegramGatewayRecord(t, f, chat, 32, "dispatching")
	if writer.snapshot()[0]["content"] != reply.Message.Text {
		t.Fatal(writer.snapshot())
	}
	still, err := f.service.store.Get(old.RecordID)
	if err != nil || still.State != "rejected" {
		t.Fatal(still, err)
	}
	// Privacy controls platform delivery; Gateway imposes no reply-only filter.
	payload, _ := json.Marshal(InputResultPayload{RequestID: "im-" + rec.RecordID, Status: inputQueued})
	process.mu.Lock()
	process.applyInputResult(&HeadlessEnvelope{Payload: payload, Seq: 1}, time.Now())
	process.mu.Unlock()
	api.batches <- []telegram.Update{telegramGatewayUpdate(33, chat, "delivered owner text without a reply")}
	waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 2 })
	if writer.snapshot()[1]["content"] != "delivered owner text without a reply" {
		t.Fatal("Gateway added a reply-only filter", writer.snapshot())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	found := false
	for _, text := range api.sent {
		if strings.Contains(text, "/bind@GatewayBot") {
			found = true
		}
	}
	if !found {
		t.Fatal("group guidance omitted bot suffix", api.sent)
	}
}

func TestTelegramGatewayTokenRotationAndBotIsolation(t *testing.T) {
	f := newTelegramGatewayFixture(t, 717004)
	first, _ := f.adapter(t, f.botID, telegramGatewayUpdate(40, 123456, "/status"))
	firstDone := startTelegramGateway(t, first)
	telegramGatewayRecord(t, f, 123456, 40, "completed")
	first.Disconnect()
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("first poller did not stop")
	}
	// startTelegramGateway's cleanup still requires a completion notification.
	// A closed result channel remains readable after this explicit join.
	// Closing is safe because Connect has completed its sole send.
	close(firstDone)
	rotated, _ := f.adapter(t, f.botID, telegramGatewayUpdate(41, 123456, "/status"))
	rotatedDone := startTelegramGateway(t, rotated)
	telegramGatewayRecord(t, f, 123456, 41, "completed")
	msg := im.IncomingMessage{IMType: "telegram", AccountID: strconv.FormatInt(f.botID, 10), ChatKind: "private", ChatID: "123456", SenderID: "123456", MessageID: "authorize-rotation", Text: "task"}
	ticket, ok := f.router.admission.Authorize(msg)
	if !ok {
		t.Fatal("same Bot ID token rotation lost owner")
	}
	rotated.Disconnect()
	select {
	case <-rotatedDone:
	case <-time.After(3 * time.Second):
		t.Fatal("rotated poller did not stop")
	}
	close(rotatedDone)
	other, _ := f.adapter(t, f.botID+100)
	err := other.Connect()
	if !errors.Is(err, im.ErrPairingDenied) {
		t.Fatalf("different bot accepted old owner: %v", err)
	}
	if f.router.admission.Valid(ticket) {
		t.Fatal("different Bot ID retained old authorization")
	}
	cursor, err := f.service.store.Cursor("telegram", strconv.FormatInt(f.botID+100, 10))
	if err != nil || cursor.Generation != 0 {
		t.Fatal("different Bot reused old cursor", cursor, err)
	}
}

func TestTelegramGatewayFirstPrivatePairingWritesOwnerAndBinding(t *testing.T) {
	f := newTelegramGatewayFixtureWithPairing(t, 717005, false)
	a, api := f.adapter(t, f.botID)
	startTelegramGateway(t, a)
	waitIngressCondition(t, func() bool { return a.ReadyForTest() })
	// Readiness is published after the sole terminal write, providing the
	// synchronization needed to inspect the first pairing code without a race.
	fields := strings.Fields(f.terminal.String())
	var code string
	for index, field := range fields {
		if field == "\"<directory>\"" && index+1 < len(fields) {
			code = fields[index+1]
			break
		}
	}
	if code == "" {
		t.Fatal("Telegram private pairing code was not displayed")
	}
	api.batches <- []telegram.Update{telegramGatewayUpdate(50, 123456, fmt.Sprintf("/bind %q %s", f.directory, code))}
	telegramGatewayRecord(t, f, 123456, 50, "completed")
	cfg := f.router.getConfig()
	owner := cfg.IMConfigByType("telegram").OwnerIdentity()
	if owner == nil || owner.AccountID != strconv.FormatInt(f.botID, 10) || owner.UserID != "123456" {
		t.Fatal(owner)
	}
	workspace, err := cfg.ResolveWorkspace("telegram", "123456")
	canonical, _, _ := canonicalBindingDirectory(f.directory)
	if err != nil || workspace.Path != canonical {
		t.Fatal(workspace, err)
	}
	persisted, err := config.Load(f.router.mgr.store.path)
	if err != nil || persisted.IMConfigByType("telegram").OwnerIdentity().UserID != "123456" || len(persisted.IMConfigByType("telegram").Bindings()) != 1 {
		t.Fatal(persisted, err)
	}
	f.router.mgr.mu.Lock()
	count := len(f.router.mgr.procs)
	f.router.mgr.mu.Unlock()
	if count != 0 {
		t.Fatal("first bind started a Chord session")
	}
}

func TestTelegramGatewaySuffixedControlsCaptureSharedTargets(t *testing.T) {
	f := newTelegramGatewayFixture(t, 717006)
	process, writer := f.installProcess(t, 123456)
	process.mu.Lock()
	process.state.PendingConfirm = &ConfirmPayload{RequestID: "confirmation"}
	process.state.applyQuestionState(&QuestionPayload{RequestID: "question", Version: 3})
	process.state.Busy = true
	process.state.TurnStartedAt = time.Now()
	turnID := uint64(process.state.TurnStartedAt.UnixNano())
	process.mu.Unlock()
	msg := im.IncomingMessage{IMType: "telegram", AccountID: strconv.FormatInt(f.botID, 10), ChatID: "123456"}
	pauseIngressTestRoute(t, f.service, msg)
	a, api := f.adapter(t, f.botID)
	startTelegramGateway(t, a)
	api.batches <- []telegram.Update{
		telegramGatewayUpdate(60, 123456, "/allow@GatewayBot"),
		telegramGatewayUpdate(61, 123456, "/answer@GatewayBot @question selected answer"),
		telegramGatewayUpdate(62, 123456, "/cancel@GatewayBot"),
		telegramGatewayUpdate(63, 123456, "/summary@GatewayBot"),
	}
	for _, id := range []int64{60, 61, 62, 63} {
		rec := telegramGatewayRecord(t, f, 123456, id, "received")
		if rec.Kind != "control" || rec.ControlTarget == nil || rec.ControlTarget.RuntimeID != process.runtimeID {
			t.Fatal(rec)
		}
		switch id {
		case 60:
			if rec.ControlTarget.RequestID != "confirmation" {
				t.Fatal(rec.ControlTarget)
			}
		case 61:
			if rec.ControlTarget.RequestID != "question" || rec.ControlTarget.Version != 3 {
				t.Fatal(rec.ControlTarget)
			}
		case 62:
			if rec.ControlTarget.TurnID != turnID {
				t.Fatal(rec.ControlTarget)
			}
		}
		var cmd im.IncomingMessage
		waitIngressCondition(t, func() bool {
			f.service.mu.Lock()
			defer f.service.mu.Unlock()
			control, exists := f.service.controls[rec.RecordID]
			cmd = control.message
			return exists
		})
		if strings.Contains(strings.Fields(cmd.Text)[0], "@GatewayBot") {
			t.Fatal("Telegram suffix reached the common command model", cmd.Text)
		}
	}
	if len(writer.snapshot()) != 0 {
		t.Fatal("paused controls dispatched before target inspection")
	}
}
