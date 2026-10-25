package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	disgo "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/gorilla/websocket"

	gateway "github.com/keakon/chord-gateway/internal/discordgateway"
	"github.com/keakon/chord-gateway/internal/im"
	discord "github.com/keakon/chord-gateway/internal/platform/discord"
)

// Fixture accounts must not contend on the production machine-wide account lock
// when separate race/coverage processes run the same tests concurrently.
func discordAdapterAccountID(t *testing.T) string {
	t.Helper()
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d/%s", os.Getpid(), t.Name())))
	return strconv.FormatUint(binary.BigEndian.Uint64(digest[:8])&((1<<63)-1)|1, 10)
}

func discordAdapterTestWS(t *testing.T, handler func(*websocket.Conn, int)) (string, *atomic.Int32) {
	t.Helper()
	count := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn, int(count.Add(1)))
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), count
}
func discordAdapterHello(conn *websocket.Conn) error {
	return conn.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
}
func discordAdapterCommand(conn *websocket.Conn) (int, error) {
	for {
		var message struct {
			Op int `json:"op"`
		}
		if err := conn.ReadJSON(&message); err != nil {
			return 0, err
		}
		if message.Op == 1 {
			if err := conn.WriteJSON(map[string]any{"op": 11}); err != nil {
				return 0, err
			}
			continue
		}
		return message.Op, nil
	}
}
func discordAdapterReady(conn *websocket.Conn, user, app, url string) error {
	return conn.WriteJSON(map[string]any{"op": 0, "t": "READY", "s": 1, "d": map[string]any{"v": 10, "user": map[string]any{"id": user, "bot": true}, "application": map[string]any{"id": app}, "guilds": []any{}, "session_id": "test-session", "resume_gateway_url": url}})
}
func discordAdapterDrain(conn *websocket.Conn) {
	for {
		var message struct {
			Op int `json:"op"`
		}
		if conn.ReadJSON(&message) != nil {
			return
		}
		if message.Op == 1 {
			if conn.WriteJSON(map[string]any{"op": 11}) != nil {
				return
			}
		}
	}
}
func discordAdapterSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("adapter lifecycle signal timed out")
	}
}
func discordAdapterResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Connect did not finish")
		return nil
	}
}
func discordAdapterFixture(t *testing.T, apiURL string) (*discord.DiscordAdapter, *IngressService, *NotificationRouter) {
	t.Helper()
	router, _ := pairedTestRouterWithConfig(t, fmt.Sprintf("ims: {wechat: {}, discord: {bot_token: placeholder, application_id: '456', owner: {account_id: %q, user_id: '789'}}}\n", discordAdapterAccountID(t)))
	service := NewIngressService(testIngressStore(t), router)
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	imCfg := router.getConfig().IMConfigByType("discord")
	adapter, err := discord.NewDiscordAdapter(router.getConfig(), *imCfg, nil, router)
	if err != nil {
		t.Fatal(err)
	}
	adapter.SetRESTForTest(discord.NewDiscordRESTClient(adapter.ContextForTest(), &http.Client{Timeout: 3 * time.Second}, "test-token", rest.WithURL(apiURL)))
	router.SetAdapter(adapter)
	t.Cleanup(adapter.Disconnect)
	return adapter, service, router
}
func discordAdapterREST(t *testing.T, wsURL string, override func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if override != nil && override(w, r) {
			return
		}
		switch r.URL.Path {
		case "/users/@me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": discordAdapterAccountID(t), "bot": true})
		case "/oauth2/applications/@me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "456", "bot": map[string]any{"id": discordAdapterAccountID(t), "bot": true}})
		case "/gateway/bot":
			_ = json.NewEncoder(w).Encode(map[string]any{"url": wsURL, "shards": 1, "session_start_limit": map[string]any{"total": 100, "remaining": 100, "reset_after": 60000, "max_concurrency": 1}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
func discordAdapterWaitReady(t *testing.T, a *discord.DiscordAdapter, notifications <-chan struct{}, result <-chan error) {
	t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		if a.IngressDeliveryReadiness(im.ChatIdentity{Platform: "discord", AccountID: discordAdapterAccountID(t), ChatID: "500"}).Ready {
			return
		}
		select {
		case <-notifications:
		case err := <-result:
			t.Fatalf("Connect returned before readiness: %v", err)
		case <-timeout.C:
			account, generation, ready, stopping := a.StateForTest()
			g := a.GatewayForTest()
			state := "not constructed"
			if g != nil {
				state = g.Status().String()
			}
			t.Fatalf("adapter did not become ready: account=%s generation=%d ready=%t stopping=%t gateway=%s", account, generation, ready, stopping, state)
		}
	}
}

func TestDiscordAdapterAuthenticatesAndRecoversBeforeReady(t *testing.T) {
	var adapter *discord.DiscordAdapter
	var service *IngressService
	var router *NotificationRouter
	identified := make(chan map[string]any, 1)
	preparedAtIdentify := make(chan bool, 1)
	wsURL, _ := discordAdapterTestWS(t, func(conn *websocket.Conn, _ int) {
		if discordAdapterHello(conn) != nil {
			return
		}
		for {
			var message map[string]any
			if conn.ReadJSON(&message) != nil {
				return
			}
			if message["op"] == float64(1) {
				_ = conn.WriteJSON(map[string]any{"op": 11})
				continue
			}
			service.mu.Lock()
			prepared := service.prepared[compositeKey("discord", discordAdapterAccountID(t))]
			service.mu.Unlock()
			preparedAtIdentify <- prepared
			identified <- message
			break
		}
		_ = discordAdapterReady(conn, discordAdapterAccountID(t), "456", "")
		discordAdapterDrain(conn)
	})
	var sequenceMu sync.Mutex
	var sequence []string
	api := discordAdapterREST(t, wsURL, func(_ http.ResponseWriter, r *http.Request) bool {
		sequenceMu.Lock()
		sequence = append(sequence, r.URL.Path)
		sequenceMu.Unlock()
		return false
	})
	adapter, service, router = discordAdapterFixture(t, api.URL)
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
	service.mu.Lock()
	prepared := service.prepared[compositeKey("discord", discordAdapterAccountID(t))]
	service.mu.Unlock()
	if !prepared {
		t.Fatal("PlatformReady preceded ingress recovery")
	}
	if !<-preparedAtIdentify {
		t.Fatal("Gateway Identify preceded ingress recovery")
	}
	router.admission.pairing.mu.Lock()
	platform := router.admission.pairing.platforms["discord"]
	account, ready := platform.account, platform.ready
	router.admission.pairing.mu.Unlock()
	if account != discordAdapterAccountID(t) || !ready {
		t.Fatalf("pairing identity = %s/%v", account, ready)
	}
	select {
	case message := <-identified:
		if message["op"] != float64(2) {
			t.Fatalf("first command = %v", message["op"])
		}
		data := message["d"].(map[string]any)
		expected := gateway.IntentGuilds | gateway.IntentGuildMessages | gateway.IntentDirectMessages
		if data["intents"] != float64(expected) {
			t.Fatalf("Identify intents = %v", data["intents"])
		}
		if data["compress"] == true {
			t.Fatal("compression enabled")
		}
	case <-time.After(time.Second):
		t.Fatal("missing Identify")
	}
	sequenceMu.Lock()
	got := strings.Join(sequence, ",")
	sequenceMu.Unlock()
	if got != "/users/@me,/oauth2/applications/@me,/gateway/bot" {
		t.Fatalf("authentication order: %s", got)
	}
	adapter.Disconnect()
	adapter.Disconnect()
	if err := discordAdapterResult(t, result); err != nil {
		t.Fatal(err)
	}
	if adapter.IngressDeliveryReadiness(im.ChatIdentity{Platform: "discord", AccountID: discordAdapterAccountID(t), ChatID: "500"}).Ready {
		t.Fatal("disconnected adapter ready")
	}
	lock, err := im.AcquireIMAccountLock("discord", discordAdapterAccountID(t))
	if err != nil {
		t.Fatalf("account lock leaked: %v", err)
	}
	lock.Close()
}

func TestDiscordAdapterRejectsRESTIdentityAndSharding(t *testing.T) {
	cases := []struct{ name, path, body, want string }{
		{"human", "/users/@me", `{"id":"123","bot":false}`, "bot_account_required"},
		{"zero-user", "/users/@me", `{"id":"0","bot":true}`, "bot_account_required"},
		{"application", "/oauth2/applications/@me", `{"id":"999","bot":{"id":"123","bot":true}}`, "application_identity_mismatch"},
		{"associated-bot", "/oauth2/applications/@me", `{"id":"456","bot":{"id":"999","bot":true}}`, "application_identity_mismatch"},
		{"sharding", "/gateway/bot", `{"url":"ws://127.0.0.1/unused","shards":2}`, "sharding_not_supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := discordAdapterREST(t, "ws://127.0.0.1/unused", func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != tc.path {
					return false
				}
				_, _ = w.Write([]byte(strings.ReplaceAll(tc.body, `"123"`, `"`+discordAdapterAccountID(t)+`"`)))
				return true
			})
			adapter, _, _ := discordAdapterFixture(t, api.URL)
			err := adapter.Connect()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Connect = %v, want %s", err, tc.want)
			}
			if adapter.CurrentGenerationForTest(1) {
				t.Fatal("rejected identity became ready")
			}
			if adapter.AccountIDForTest() != "" {
				lock, err := im.AcquireIMAccountLock("discord", discordAdapterAccountID(t))
				if err != nil {
					t.Fatalf("account lock leaked: %v", err)
				}
				lock.Close()
			}
		})
	}
}

func TestDiscordAdapterAccountLockCoversWorkerExit(t *testing.T) {
	wsURL, _ := discordAdapterTestWS(t, func(conn *websocket.Conn, _ int) {
		_ = discordAdapterHello(conn)
		_, _ = discordAdapterCommand(conn)
		_ = discordAdapterReady(conn, discordAdapterAccountID(t), "456", "")
		discordAdapterDrain(conn)
	})
	api := discordAdapterREST(t, wsURL, nil)
	adapter, _, _ := discordAdapterFixture(t, api.URL)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	adapter.SetIngressReadinessNotifier(func() {
		if adapter.IngressDeliveryReadiness(im.ChatIdentity{Platform: "discord", AccountID: discordAdapterAccountID(t), ChatID: "500"}).Ready {
			once.Do(func() { close(entered); <-release })
		}
	})
	result := make(chan error, 1)
	go func() { result <- adapter.Connect() }()
	discordAdapterSignal(t, entered)
	disconnected := make(chan struct{})
	go func() { adapter.Disconnect(); close(disconnected) }()
	discordAdapterSignal(t, adapter.ContextForTest().Done())
	if lock, err := im.AcquireIMAccountLock("discord", discordAdapterAccountID(t)); err == nil {
		lock.Close()
		t.Error("account lock released while gateway callback worker was active")
	}
	select {
	case <-disconnected:
		t.Error("Disconnect returned before callback exit")
	default:
	}
	close(release)
	discordAdapterSignal(t, disconnected)
	if err := discordAdapterResult(t, result); err != nil {
		t.Fatal(err)
	}
	lock, err := im.AcquireIMAccountLock("discord", discordAdapterAccountID(t))
	if err != nil {
		t.Fatalf("account lock not released: %v", err)
	}
	lock.Close()
}

func TestDiscordAdapterReadyMismatchAndFatalCloseStop(t *testing.T) {
	for _, mode := range []string{"ready-user", "ready-application", "fatal-close"} {
		t.Run(mode, func(t *testing.T) {
			fail := make(chan struct{})
			wsURL, count := discordAdapterTestWS(t, func(conn *websocket.Conn, _ int) {
				_ = discordAdapterHello(conn)
				_, _ = discordAdapterCommand(conn)
				user, app := discordAdapterAccountID(t), "456"
				if mode == "ready-user" {
					user = "999"
				}
				if mode == "ready-application" {
					app = "999"
				}
				_ = discordAdapterReady(conn, user, app, "")
				if mode == "fatal-close" {
					<-fail
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4004, "fatal"), time.Now().Add(time.Second))
					return
				}
				discordAdapterDrain(conn)
			})
			api := discordAdapterREST(t, wsURL, nil)
			adapter, _, _ := discordAdapterFixture(t, api.URL)
			notifications := make(chan struct{}, 16)
			adapter.SetIngressReadinessNotifier(func() {
				select {
				case notifications <- struct{}{}:
				default:
				}
			})
			result := make(chan error, 1)
			go func() { result <- adapter.Connect() }()
			if mode == "fatal-close" {
				discordAdapterWaitReady(t, adapter, notifications, result)
				close(fail)
			}
			if err := discordAdapterResult(t, result); err == nil {
				t.Fatal("fatal identity/close returned nil")
			}
			if adapter.IngressDeliveryReadiness(im.ChatIdentity{Platform: "discord", AccountID: discordAdapterAccountID(t), ChatID: "500"}).Ready {
				t.Fatal("fatal connection remained ready")
			}
			if count.Load() != 1 {
				t.Fatalf("fatal connection reconnected %d times", count.Load())
			}
			adapter.Disconnect()
			adapter.Disconnect()
		})
	}
}

func TestDiscordAdapterConcurrentConnectDisconnect(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	api := discordAdapterREST(t, "ws://127.0.0.1/unused", func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/users/@me" {
			return false
		}
		once.Do(func() { close(entered) })
		<-r.Context().Done()
		return true
	})
	adapter, _, _ := discordAdapterFixture(t, api.URL)
	result := make(chan error, 1)
	go func() { result <- adapter.Connect() }()
	discordAdapterSignal(t, entered)
	if err := adapter.Connect(); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("second Connect = %v", err)
	}
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() { defer workers.Done(); adapter.Disconnect() }()
	}
	workers.Wait()
	if err := discordAdapterResult(t, result); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Connect(); err != nil {
		t.Fatalf("Connect after shutdown = %v", err)
	}
	if !errors.Is(adapter.SendText("500", "text"), context.Canceled) {
		t.Fatal("send allowed after disconnect")
	}
}

func TestDiscordAdapterDisconnectBeforeConnect(t *testing.T) {
	api := discordAdapterREST(t, "ws://127.0.0.1/unused", nil)
	adapter, _, _ := discordAdapterFixture(t, api.URL)
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() { defer workers.Done(); adapter.Disconnect() }()
	}
	workers.Wait()
	if err := adapter.Connect(); err != nil {
		t.Fatalf("Connect after shutdown = %v", err)
	}
}

func TestDiscordAdapterReadinessScopeAndOldGeneration(t *testing.T) {
	api := discordAdapterREST(t, "ws://127.0.0.1/unused", nil)
	adapter, _, _ := discordAdapterFixture(t, api.URL)
	adapter.SetSessionForTest(discordAdapterAccountID(t), 2, true)
	adapter.OnStateChangeForTest(gateway.StatusDisconnected, 1)
	if !adapter.CurrentGenerationForTest(2) {
		t.Fatal("old generation invalidated current readiness")
	}
	for _, id := range []im.ChatIdentity{{Platform: "wechat", AccountID: discordAdapterAccountID(t), ChatID: "500"}, {Platform: "discord", AccountID: "999", ChatID: "500"}} {
		if got := adapter.IngressDeliveryReadiness(id); got.Ready || got.Reason != im.IngressWaitingAccount {
			t.Fatal(fmt.Sprint(got))
		}
	}
}

func TestDiscordAdapterAcceptsOptionalApplicationBotFields(t *testing.T) {
	for _, body := range []string{`{"id":"456"}`, `{"id":"456","bot":{"id":"123"}}`} {
		t.Run(body, func(t *testing.T) {
			api := discordAdapterREST(t, "ws://127.0.0.1/unused", func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/oauth2/applications/@me" {
					return false
				}
				_, _ = w.Write([]byte(strings.ReplaceAll(body, `"123"`, `"`+discordAdapterAccountID(t)+`"`)))
				return true
			})
			adapter, _, _ := discordAdapterFixture(t, api.URL)
			account, err := adapter.AuthenticateForTest(t.Context())
			if err != nil || account != discordAdapterAccountID(t) {
				t.Fatalf("valid optional application fields rejected: %s %v", account, err)
			}
		})
	}
}

func TestDiscordAdapterRefreshesIdentifyQuotaBeforeDial(t *testing.T) {
	for _, mode := range []string{"success", "temporary", "unauthorized", "forbidden", "sharding", "empty_gateway", "invalid_quota"} {
		t.Run(mode, func(t *testing.T) {
			wsURL, sockets := discordAdapterTestWS(t, func(conn *websocket.Conn, _ int) {
				_ = discordAdapterHello(conn)
				_, _ = discordAdapterCommand(conn)
				_ = discordAdapterReady(conn, discordAdapterAccountID(t), "456", "")
				discordAdapterDrain(conn)
			})
			var queries atomic.Int32
			api := discordAdapterREST(t, wsURL, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/gateway/bot" {
					return false
				}
				query := queries.Add(1)
				if query > 1 {
					if mode == "unauthorized" || mode == "forbidden" {
						status := 401
						if mode == "forbidden" {
							status = 403
						}
						w.WriteHeader(status)
						fmt.Fprint(w, `{"message":"rejected"}`)
						return true
					}
					if mode == "temporary" && query == 2 {
						w.WriteHeader(503)
						fmt.Fprint(w, `{"message":"temporary"}`)
						return true
					}
					if mode == "empty_gateway" {
						fmt.Fprint(w, `null`)
						return true
					}
				}
				shards, remaining, reset := 1, 1, 1000
				if query == 1 {
					remaining, reset = 0, 0
				}
				if query > 1 && mode == "sharding" {
					shards = 2
				}
				if query > 1 && mode == "invalid_quota" {
					remaining, reset = 0, 0
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"url": wsURL, "shards": shards, "session_start_limit": map[string]any{"total": 100, "remaining": remaining, "reset_after": reset, "max_concurrency": 1}})
				return true
			})
			adapter, _, _ := discordAdapterFixture(t, api.URL)
			notifications := make(chan struct{}, 16)
			adapter.SetIngressReadinessNotifier(func() {
				select {
				case notifications <- struct{}{}:
				default:
				}
			})
			result := make(chan error, 1)
			go func() { result <- adapter.Connect() }()
			if mode == "success" || mode == "temporary" {
				discordAdapterWaitReady(t, adapter, notifications, result)
				adapter.Disconnect()
				if err := discordAdapterResult(t, result); err != nil {
					t.Fatal(err)
				}
				want := int32(2)
				if mode == "temporary" {
					want = 3
				}
				if queries.Load() != want || sockets.Load() != 1 {
					t.Fatalf("quota refresh attempts=%d sockets=%d", queries.Load(), sockets.Load())
				}
			} else {
				if err := discordAdapterResult(t, result); err == nil || !strings.Contains(err.Error(), "identify_quota_unavailable") {
					t.Fatalf("fatal quota refresh=%v", err)
				}
				if queries.Load() != 2 || sockets.Load() != 0 {
					t.Fatalf("fatal quota dialed or retried: queries=%d sockets=%d", queries.Load(), sockets.Load())
				}
			}
		})
	}
}

func TestDiscordAdapterIdentifyQuotaRefreshCancellation(t *testing.T) {
	entered := make(chan struct{})
	var queries atomic.Int32
	api := discordAdapterREST(t, "ws://127.0.0.1/unused", func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/gateway/bot" {
			return false
		}
		if queries.Add(1) == 1 {
			fmt.Fprint(w, `{"url":"ws://127.0.0.1/unused","shards":1,"session_start_limit":{"total":100,"remaining":0,"reset_after":0,"max_concurrency":1}}`)
			return true
		}
		close(entered)
		<-r.Context().Done()
		return true
	})
	adapter, _, _ := discordAdapterFixture(t, api.URL)
	result := make(chan error, 1)
	go func() { result <- adapter.Connect() }()
	discordAdapterSignal(t, entered)
	adapter.Disconnect()
	if err := discordAdapterResult(t, result); err != nil {
		t.Fatal(err)
	}
	if queries.Load() != 2 {
		t.Fatal("cancelled quota refresh retried")
	}
}

func TestDiscordAdapterChannelCacheSurvivesReconnectAndIdentifies(t *testing.T) {
	api := discordAdapterREST(t, "ws://127.0.0.1/unused", nil)
	adapter, _, _ := discordAdapterFixture(t, api.URL)
	current, live, observe := adapter.GenerationGuardsForTest()
	inbound := discord.NewDiscordInbound(t.Context(), discordAdapterAccountID(t), "456", nil, nil, current, live, observe)
	adapter.SetInboundForTest(inbound)
	channel := discordTestGuildChannel(t, disgo.ChannelTypeGuildText, "500")
	adapter.SetSessionForTest(discordAdapterAccountID(t), 1, true)
	inbound.CacheChannel(1, channel)
	cached := func() bool {
		_, exists := inbound.ChannelForTest("500")
		return exists
	}
	// Every transient state of the next connection attempt keeps classification,
	// including the Resume handshake that replays the missed window.
	for _, state := range []gateway.Status{gateway.StatusDisconnected, gateway.StatusConnecting, gateway.StatusWaitingForHello, gateway.StatusResuming, gateway.StatusWaitingForReady} {
		adapter.OnStateChangeForTest(state, 2)
		if !cached() {
			t.Fatalf("%s discarded channel classification", state)
		}
	}
	// A replayed metadata dispatch refreshes classification before the resume
	// barrier opens for input.
	adapter.ApplyChannelMetadataForTest(gateway.EventChannelCreate{GuildChannel: discordTestGuildChannel(t, disgo.ChannelTypeGuildText, "600")}, 2)
	kind, exists := inbound.ChannelForTest("600")
	if !exists || kind != disgo.ChannelTypeGuildText {
		t.Fatal("replay discarded channel classification", kind, exists)
	}
	// A generation that lost the connection cannot write to the cache.
	adapter.ApplyChannelMetadataForTest(gateway.EventChannelCreate{GuildChannel: discordTestGuildChannel(t, disgo.ChannelTypeGuildText, "700")}, 1)
	if _, stale := inbound.ChannelForTest("700"); stale {
		t.Fatal("classification accepted a stale generation")
	}
	// A fresh Identify after a ready session replaces classification.
	adapter.SetSessionForTest(adapter.AccountIDForTest(), 3, true)
	adapter.OnStateChangeForTest(gateway.StatusIdentifying, 4)
	if cached() {
		t.Fatal("fresh Identify kept the previous session classification")
	}
}

func discordTestGuildChannel(t *testing.T, kind disgo.ChannelType, id string) disgo.GuildChannel {
	t.Helper()
	channel, ok := discordTestChannel(t, kind, id).(disgo.GuildChannel)
	if !ok {
		t.Fatalf("channel %s is not a guild channel", id)
	}
	return channel
}

func TestDiscordAdapterClassifiesChannelAndThreadEvents(t *testing.T) {
	api := discordAdapterREST(t, "ws://127.0.0.1/unused", nil)
	adapter, _, _ := discordAdapterFixture(t, api.URL)
	current, live, observe := adapter.GenerationGuardsForTest()
	inbound := discord.NewDiscordInbound(t.Context(), discordAdapterAccountID(t), "456", nil, nil, current, live, observe)
	adapter.SetInboundForTest(inbound)
	adapter.SetSessionForTest(discordAdapterAccountID(t), 1, true)
	kindOf := func(id string) (disgo.ChannelType, bool) {
		return inbound.ChannelForTest(id)
	}
	text := discordTestGuildChannel(t, disgo.ChannelTypeGuildText, "500")
	thread := discordTestChannel(t, disgo.ChannelTypeGuildPublicThread, "501").(disgo.GuildThread)
	adapter.ApplyChannelMetadataForTest(gateway.EventChannelCreate{GuildChannel: text}, 1)
	adapter.ApplyChannelMetadataForTest(gateway.EventChannelUpdate{GuildChannel: discordTestGuildChannel(t, disgo.ChannelTypeGuildNews, "500")}, 1)
	adapter.ApplyChannelMetadataForTest(gateway.EventThreadCreate{GuildThread: thread}, 1)
	adapter.ApplyChannelMetadataForTest(gateway.EventThreadListSync{Threads: []disgo.GuildThread{thread}}, 1)
	adapter.ApplyChannelMetadataForTest(gateway.EventThreadUpdate{GuildThread: thread}, 1)
	if kind, exists := kindOf("500"); !exists || kind != disgo.ChannelTypeGuildNews {
		t.Fatalf("channel classification = %v %t", kind, exists)
	}
	if kind, exists := kindOf("501"); !exists || kind != disgo.ChannelTypeGuildPublicThread {
		t.Fatalf("thread classification = %v %t", kind, exists)
	}
	// A nil payload and a stale generation are refused without touching the cache.
	adapter.ApplyChannelMetadataForTest(gateway.EventChannelCreate{}, 1)
	adapter.ApplyChannelMetadataForTest(gateway.EventChannelDelete{GuildChannel: discordTestGuildChannel(t, disgo.ChannelTypeGuildText, "502")}, 1)
	if _, exists := kindOf("502"); exists {
		t.Fatal("nil channel cached")
	}
	deleted := gateway.EventChannelDelete{GuildChannel: text}
	adapter.ApplyChannelMetadataForTest(deleted, 1)
	adapter.ApplyChannelMetadataForTest(gateway.EventThreadDelete{ID: thread.ID()}, 1)
	if _, exists := kindOf("500"); exists {
		t.Fatal("deleted channel retained")
	}
	if _, exists := kindOf("501"); exists {
		t.Fatal("deleted thread retained")
	}
}
