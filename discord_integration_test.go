package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/keakon/chord-gateway/internal/im"

	discord "github.com/keakon/chord-gateway/internal/platform/discord"
)

// This exercises the actual local Gateway, adapter, REST channel lookup,
// durable admission, and Chord input protocol across a resumed connection.
func TestDiscordGatewayDurableIngressAndResumeDelivery(t *testing.T) {
	sendTask, reconnect := make(chan struct{}), make(chan struct{})
	commands := make(chan int, 2)
	wsURL, _ := discordAdapterTestWS(t, func(conn *websocket.Conn, attempt int) {
		if discordAdapterHello(conn) != nil {
			return
		}
		op, err := discordAdapterCommand(conn)
		if err != nil {
			return
		}
		commands <- op
		if attempt == 1 {
			_ = discordAdapterReady(conn, discordAdapterAccountID(t), "456", "")
			<-sendTask
			if discordIntegrationMessage(t, conn, 2) != nil {
				return
			}
			<-reconnect
			_ = conn.WriteJSON(map[string]any{"op": 7})
		} else {
			// Discord replays missed dispatches before the final RESUMED barrier.
			_ = discordIntegrationMessage(t, conn, 3)
			_ = conn.WriteJSON(map[string]any{"op": 0, "t": "RESUMED", "s": 4, "d": map[string]any{}})
		}
		discordAdapterDrain(conn)
	})
	api := discordAdapterREST(t, wsURL, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/channels/500":
			// Existing public thread: the thread itself is both routing and send target.
			fmt.Fprint(w, `{"id":"500","type":11,"guild_id":"700","parent_id":"400","name":"test-thread"}`)
			return true
		case "/channels/500/messages":
			fmt.Fprint(w, `{"id":"999","channel_id":"500"}`)
			return true
		}
		return false
	})
	adapter, service, router := discordAdapterFixture(t, api.URL)
	notifications := make(chan struct{}, 16)
	adapter.SetIngressReadinessNotifier(func() {
		select {
		case notifications <- struct{}{}:
		default:
		}
	})
	result := make(chan error, 1)
	go func() { result <- adapter.Connect() }()
	discordAdapterWaitReady(t, adapter, notifications, result)
	writer, process, identity := discordIntegrationProcess(t, router, adapter, service)
	close(sendTask)
	waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 1 })
	records, err := service.store.ListRouteRecords(identity)
	if err != nil || len(records) != 1 {
		t.Fatalf("durable ingress records=%d error=%v", len(records), err)
	}
	record := records[0]
	if record.SemanticID != "message:1001" || record.ChatID != "500" || record.Kind != "task" || record.Text != "sample task" {
		t.Fatalf("gateway message changed identity: %#v", record)
	}
	input := writer.snapshot()[0]
	if input["content"] != "sample task" || input["request_id"] != "im-"+record.RecordID {
		t.Fatalf("Chord input = %#v", input)
	}
	receipt, _ := json.Marshal(InputResultPayload{RequestID: "im-" + record.RecordID, Status: inputQueued})
	process.mu.Lock()
	process.applyInputResult(&HeadlessEnvelope{Payload: receipt, Seq: 1}, time.Now())
	process.mu.Unlock()
	waitIngressCondition(t, func() bool { r, e := service.store.Get(record.RecordID); return e == nil && r.State == "consumed" })
	close(reconnect)
	waitIngressCondition(t, func() bool {
		for _, counter := range service.store.MetricsSnapshot().Counters {
			if counter.Platform == "discord" && counter.Event == "duplicate" && counter.Count == 1 {
				return true
			}
		}
		return false
	})
	if adapter.CurrentGenerationForTest(1) || !adapter.CurrentGenerationForTest(2) {
		t.Fatal("Resume did not move readiness to new generation")
	}
	if len(writer.snapshot()) != 1 {
		t.Fatal("Resume redelivery executed Chord input twice")
	}
	records, err = service.store.ListRouteRecords(identity)
	if err != nil || len(records) != 1 || records[0].RecordID != record.RecordID {
		t.Fatal("Resume allocated a second durable message", records, err)
	}
	for _, want := range []int{2, 6} {
		select {
		case got := <-commands:
			if got != want {
				t.Fatalf("gateway opcode=%d want=%d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("missing Identify/Resume")
		}
	}
	adapter.Disconnect()
	if err := discordAdapterResult(t, result); err != nil {
		t.Fatal(err)
	}
}

func discordIntegrationMessage(t *testing.T, conn *websocket.Conn, sequence int) error {
	return discordIntegrationMessageContent(t, conn, sequence, "1001", "<@"+discordAdapterAccountID(t)+"> sample task")
}
func discordIntegrationMessageContent(t *testing.T, conn *websocket.Conn, sequence int, id, content string) error {
	return conn.WriteJSON(map[string]any{"op": 0, "t": "MESSAGE_CREATE", "s": sequence, "d": map[string]any{
		"id": id, "channel_id": "500", "guild_id": "700", "type": 0, "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"author":  map[string]any{"id": "789", "username": "sample-owner", "bot": false},
		"content": content, "mentions": []any{map[string]any{"id": discordAdapterAccountID(t), "bot": true}},
	}})
}

func discordIntegrationProcess(t *testing.T, router *NotificationRouter, adapter *discord.DiscordAdapter, service *IngressService) (*ingressTestWriter, *ChordProcess, im.ChatIdentity) {
	t.Helper()
	message := im.IncomingMessage{IMType: "discord", AccountID: discordAdapterAccountID(t), ChatID: "500", SenderID: "789", ChatKind: "group", MessageID: "integration-bind", Text: fmt.Sprintf("/bind %q", t.TempDir())}
	if !dispatchPairingTest(router, message) {
		t.Fatal("thread binding failed")
	}
	workspace, err := router.getConfig().ResolveWorkspace("discord", "500")
	if err != nil {
		t.Fatal(err)
	}
	key := (processKey{workspaceID: workspace.ID, imType: "discord", chatID: "500"}).String()
	writer := &ingressTestWriter{}
	ready := make(chan struct{})
	close(ready)
	process := &ChordProcess{key: key, runtimeID: "runtime", sessionEpoch: 1, stdin: writer, readySignal: ready, state: ControlState{SessionID: "session"}}
	router.mgr.mu.Lock()
	router.mgr.procs[key] = process
	router.mgr.mu.Unlock()
	if err := router.mgr.pins.Set(pinKeyForBinding(key), "session"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adapter.Disconnect()
		service.Close()
		process.mu.Lock()
		for _, observer := range process.inputObservers {
			if observer.timer != nil {
				observer.timer.Stop()
			}
		}
		process.mu.Unlock()
	})
	return writer, process, im.ChatIdentity{Platform: "discord", AccountID: discordAdapterAccountID(t), ChatID: "500"}
}

func TestDiscordContentlessGuildReplyIsReportedAndNotIngested(t *testing.T) {
	discordContentlessReset(t)
	reply := make(chan struct{})
	wsURL, _ := discordAdapterTestWS(t, func(conn *websocket.Conn, _ int) {
		_ = discordAdapterHello(conn)
		_, _ = discordAdapterCommand(conn)
		_ = discordAdapterReady(conn, discordAdapterAccountID(t), "456", "")
		<-reply
		// A guild reply to the bot's message mentions it but carries no text,
		// because the Message Content intent is deliberately not requested.
		_ = discordIntegrationMessageContent(t, conn, 2, "1001", "")
		discordAdapterDrain(conn)
	})
	api := discordAdapterREST(t, wsURL, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/channels/500" {
			t.Error("dropped reply still looked up its channel")
			return true
		}
		return false
	})
	adapter, service, _ := discordAdapterFixture(t, api.URL)
	notifications := make(chan struct{}, 16)
	adapter.SetIngressReadinessNotifier(func() {
		select {
		case notifications <- struct{}{}:
		default:
		}
	})
	result := make(chan error, 1)
	go func() { result <- adapter.Connect() }()
	discordAdapterWaitReady(t, adapter, notifications, result)
	close(reply)
	waitIngressCondition(t, func() bool {
		for _, counter := range service.store.MetricsSnapshot().Counters {
			if counter.Platform == "discord" && counter.Event == "callback_dropped" && counter.Reason == "unsupported_content" && counter.Count == 1 {
				return true
			}
		}
		return false
	})
	records, err := service.store.ListRouteRecords(im.ChatIdentity{Platform: "discord", AccountID: discordAdapterAccountID(t), ChatID: "500"})
	if err != nil || len(records) != 0 {
		t.Fatalf("contentless reply became durable input: records=%d error=%v", len(records), err)
	}
	adapter.Disconnect()
	if err := discordAdapterResult(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestDiscordCapturedLookupSurvivesGatewayResume(t *testing.T) {
	sendTask, reconnect := make(chan struct{}), make(chan struct{})
	lookupEntered, releaseLookup := make(chan struct{}), make(chan struct{})
	var once sync.Once
	wsURL, _ := discordAdapterTestWS(t, func(conn *websocket.Conn, attempt int) {
		_ = discordAdapterHello(conn)
		_, _ = discordAdapterCommand(conn)
		if attempt == 1 {
			_ = discordAdapterReady(conn, discordAdapterAccountID(t), "456", "")
			<-sendTask
			_ = discordIntegrationMessage(t, conn, 2)
			<-reconnect
			_ = conn.WriteJSON(map[string]any{"op": 7})
		} else {
			_ = conn.WriteJSON(map[string]any{"op": 0, "t": "RESUMED", "s": 3, "d": map[string]any{}})
		}
		// The message already seen before disconnect is deliberately not replayed.
		discordAdapterDrain(conn)
	})
	api := discordAdapterREST(t, wsURL, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/channels/500":
			once.Do(func() { close(lookupEntered) })
			select {
			case <-releaseLookup:
			case <-r.Context().Done():
				return true
			}
			fmt.Fprint(w, `{"id":"500","type":11,"guild_id":"700","parent_id":"400"}`)
			return true
		case "/channels/500/messages":
			fmt.Fprint(w, `{"id":"999","channel_id":"500"}`)
			return true
		}
		return false
	})
	adapter, service, router := discordAdapterFixture(t, api.URL)
	notifications := make(chan struct{}, 16)
	adapter.SetIngressReadinessNotifier(func() {
		select {
		case notifications <- struct{}{}:
		default:
		}
	})
	result := make(chan error, 1)
	go func() { result <- adapter.Connect() }()
	discordAdapterWaitReady(t, adapter, notifications, result)
	writer, _, identity := discordIntegrationProcess(t, router, adapter, service)
	close(sendTask)
	discordAdapterSignal(t, lookupEntered)
	close(reconnect)
	waitIngressCondition(t, func() bool { return adapter.CurrentGenerationForTest(2) })
	close(releaseLookup)
	waitIngressCondition(t, func() bool { return len(writer.snapshot()) == 1 })
	records, err := service.store.ListRouteRecords(identity)
	if err != nil || len(records) != 1 || records[0].SemanticID != "message:1001" {
		t.Fatalf("captured pre-disconnect message was lost: %#v %v", records, err)
	}
	adapter.Disconnect()
	if err := discordAdapterResult(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestDiscordResumeCapturesControlBeforeResumed(t *testing.T) {
	reconnect, finishReplay := make(chan struct{}), make(chan struct{})
	wsURL, _ := discordAdapterTestWS(t, func(conn *websocket.Conn, attempt int) {
		_ = discordAdapterHello(conn)
		_, _ = discordAdapterCommand(conn)
		if attempt == 1 {
			_ = discordAdapterReady(conn, discordAdapterAccountID(t), "456", "")
			<-reconnect
			_ = conn.WriteJSON(map[string]any{"op": 7})
		} else {
			_ = discordIntegrationMessageContent(t, conn, 2, "1002", "<@"+discordAdapterAccountID(t)+"> /allow")
			// Observing this later sequence proves the preceding message capture ran.
			_ = conn.WriteJSON(map[string]any{"op": 0, "t": "CHANNEL_PINS_UPDATE", "s": 3, "d": map[string]any{"channel_id": "500", "guild_id": "700"}})
			<-finishReplay
			_ = conn.WriteJSON(map[string]any{"op": 0, "t": "RESUMED", "s": 4, "d": map[string]any{}})
		}
		discordAdapterDrain(conn)
	})
	api := discordAdapterREST(t, wsURL, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/channels/500":
			fmt.Fprint(w, `{"id":"500","type":11,"guild_id":"700","parent_id":"400"}`)
			return true
		case "/channels/500/messages":
			fmt.Fprint(w, `{"id":"999","channel_id":"500"}`)
			return true
		}
		return false
	})
	adapter, service, router := discordAdapterFixture(t, api.URL)
	notifications := make(chan struct{}, 16)
	adapter.SetIngressReadinessNotifier(func() {
		select {
		case notifications <- struct{}{}:
		default:
		}
	})
	result := make(chan error, 1)
	go func() { result <- adapter.Connect() }()
	discordAdapterWaitReady(t, adapter, notifications, result)
	writer, process, identity := discordIntegrationProcess(t, router, adapter, service)
	process.mu.Lock()
	process.state.PendingConfirm = &ConfirmPayload{RequestID: "original-confirm", ToolName: "Write"}
	process.mu.Unlock()
	close(reconnect)
	waitIngressCondition(t, func() bool {
		g := adapter.GatewayForTest()
		seq := g.LastSequenceReceived()
		return g.Generation() == 2 && seq != nil && *seq == 3
	})
	if adapter.IngressDeliveryReadiness(identity).Ready {
		t.Fatal("replay became ready before RESUMED")
	}
	records, err := service.store.ListRouteRecords(identity)
	if err != nil || len(records) != 0 || len(writer.snapshot()) != 0 {
		t.Fatal("replay delivered before RESUMED", records, err)
	}
	if snapshot := service.normalizer.Snapshot(); snapshot.Pending != 0 {
		t.Fatalf("replay occupied shared normalizer workers: %#v", snapshot)
	}
	process.mu.Lock()
	process.state.PendingConfirm = &ConfirmPayload{RequestID: "replacement-confirm", ToolName: "Write"}
	process.mu.Unlock()
	close(finishReplay)
	waitIngressCondition(t, func() bool {
		records, err := service.store.ListRouteRecords(identity)
		return err == nil && len(records) == 1 && records[0].State == "rejected"
	})
	records, err = service.store.ListRouteRecords(identity)
	if err != nil || records[0].ControlTarget == nil || records[0].ControlTarget.RequestID != "original-confirm" {
		t.Fatalf("replay recaptured replacement control: %#v %v", records, err)
	}
	if len(writer.snapshot()) != 0 {
		t.Fatal("replayed /allow approved a replacement confirm")
	}
	adapter.Disconnect()
	if err := discordAdapterResult(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestDiscordGatewayCaptureContentionDoesNotBlockTransport(t *testing.T) {
	for _, mode := range []string{"ready", "resume", "close"} {
		t.Run(mode, func(t *testing.T) {
			sendMessage, reconnect, finishReplay := make(chan struct{}), make(chan struct{}), make(chan struct{})
			heartbeatReceived := make(chan struct{})
			wsURL, _ := discordAdapterTestWS(t, func(conn *websocket.Conn, attempt int) {
				if discordAdapterHello(conn) != nil {
					return
				}
				if _, err := discordAdapterCommand(conn); err != nil {
					return
				}
				if attempt == 1 {
					if discordAdapterReady(conn, discordAdapterAccountID(t), "456", "") != nil {
						return
					}
					if mode == "resume" {
						select {
						case <-reconnect:
						case <-t.Context().Done():
							return
						}
						_ = conn.WriteJSON(map[string]any{"op": 7})
						discordAdapterDrain(conn)
						return
					}
				}
				select {
				case <-sendMessage:
				case <-t.Context().Done():
					return
				}
				if discordIntegrationMessageContent(t, conn, 2, "1003", "<@"+discordAdapterAccountID(t)+"> /allow") != nil {
					return
				}
				// This later dispatch proves capture returned on the reader, even
				// while the process lock is still held by a stalled stdin writer.
				if conn.WriteJSON(map[string]any{"op": 0, "t": "CHANNEL_PINS_UPDATE", "s": 3, "d": map[string]any{"channel_id": "500", "guild_id": "700"}}) != nil {
					return
				}
				if conn.WriteJSON(map[string]any{"op": 1, "d": nil}) != nil {
					return
				}
				var heartbeat struct {
					Op int `json:"op"`
				}
				if conn.ReadJSON(&heartbeat) != nil || heartbeat.Op != 1 {
					return
				}
				_ = conn.WriteJSON(map[string]any{"op": 11})
				close(heartbeatReceived)
				if mode == "resume" {
					select {
					case <-finishReplay:
					case <-t.Context().Done():
						return
					}
					_ = conn.WriteJSON(map[string]any{"op": 0, "t": "RESUMED", "s": 4, "d": map[string]any{}})
				}
				discordAdapterDrain(conn)
			})
			api := discordAdapterREST(t, wsURL, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/channels/500" {
					fmt.Fprint(w, `{"id":"500","type":11,"guild_id":"700","parent_id":"400"}`)
					return true
				}
				return false
			})
			adapter, service, router := discordAdapterFixture(t, api.URL)
			notifications := make(chan struct{}, 16)
			adapter.SetIngressReadinessNotifier(func() {
				select {
				case notifications <- struct{}{}:
				default:
				}
			})
			result := make(chan error, 1)
			go func() { result <- adapter.Connect() }()
			discordAdapterWaitReady(t, adapter, notifications, result)
			writer, process, identity := discordIntegrationProcess(t, router, adapter, service)
			process.mu.Lock()
			var unlock sync.Once
			defer unlock.Do(process.mu.Unlock)
			process.state.PendingConfirm = &ConfirmPayload{RequestID: "original-confirm", ToolName: "Write"}
			if mode == "resume" {
				close(reconnect)
			}
			close(sendMessage)
			waitIngressCondition(t, func() bool {
				g := adapter.GatewayForTest()
				sequence := g.LastSequenceReceived()
				return sequence != nil && *sequence == 3
			})
			discordAdapterSignal(t, heartbeatReceived)
			if mode == "close" {
				closed := make(chan struct{})
				go func() { adapter.Disconnect(); close(closed) }()
				discordAdapterSignal(t, closed)
				if err := discordAdapterResult(t, result); err != nil {
					t.Fatal(err)
				}
				return // The process lock remains held until this return.
			}
			// Let admission run only after a different request became current.
			// A failed receive-time capture must remain a refusal, never retarget.
			process.state.PendingConfirm = &ConfirmPayload{RequestID: "replacement-confirm", ToolName: "Write"}
			unlock.Do(process.mu.Unlock)
			if mode == "resume" {
				close(finishReplay)
			}
			waitIngressCondition(t, func() bool {
				records, err := service.store.ListRouteRecords(identity)
				return err == nil && len(records) == 1 && records[0].State == "rejected"
			})
			records, err := service.store.ListRouteRecords(identity)
			if err != nil || len(records) != 1 || records[0].Reason != "control_target_unavailable" || records[0].ControlTarget != nil || records[0].Text != "" {
				t.Fatalf("capture failure did not remain a durable refusal: %#v %v", records, err)
			}
			if len(writer.snapshot()) != 0 {
				t.Fatal("failed capture approved the replacement request")
			}
			adapter.Disconnect()
			if err := discordAdapterResult(t, result); err != nil {
				t.Fatal(err)
			}
		})
	}
}
