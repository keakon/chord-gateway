package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/internal/im"
)

const discordTestBotID = "9007199254740993"

func discordTestChannel(t *testing.T, kind discord.ChannelType, id string) discord.Channel {
	t.Helper()
	var channel discord.UnmarshalChannel
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"id":%q,"type":%d,"guild_id":"700","parent_id":"400","name":"sample"}`, id, kind)), &channel); err != nil {
		t.Fatal(err)
	}
	return channel.Channel
}

func discordTestMessage() discord.Message {
	return discord.Message{
		ID: snowflake.ID(9007199254740995), ChannelID: snowflake.ID(500),
		Author:    discord.User{ID: snowflake.ID(9007199254740997), Username: "sample-user"},
		CreatedAt: time.Now(), Content: "sample task", Type: discord.MessageTypeDefault,
	}
}

func discordTestGuildMessage(text string) discord.Message {
	message := discordTestMessage()
	guildID := snowflake.ID(700)
	message.GuildID, message.Content = &guildID, text
	message.Mentions = []discord.User{{ID: snowflake.ID(9007199254740993)}}
	return message
}

func TestDiscordInboundChannelClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  discord.ChannelType
		guild bool
		want  string
	}{
		{"dm", discord.ChannelTypeDM, false, "private"},
		{"guild_text", discord.ChannelTypeGuildText, true, "group"},
		{"public_thread", discord.ChannelTypeGuildPublicThread, true, "group"},
		{"private_thread", discord.ChannelTypeGuildPrivateThread, true, "group"},
		{"group_dm", discord.ChannelTypeGroupDM, false, "unsupported"},
		{"voice", discord.ChannelTypeGuildVoice, true, "unsupported"},
		{"forum_root", discord.ChannelTypeGuildForum, true, "unsupported"},
		{"announcement", discord.ChannelTypeGuildNews, true, "unsupported"},
		{"missing_guild_evidence", discord.ChannelTypeGuildText, false, "unsupported"},
		{"contradictory_dm", discord.ChannelTypeDM, true, "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got im.IngressEnvelope
			submissions := 0
			submit := func(ctx context.Context, key string, envelope im.IngressEnvelope, normalize func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
				submissions++
				if key != "500" {
					t.Fatalf("normalization key = %q", key)
				}
				var err error
				got, err = normalize(ctx, envelope)
				return nil, err
			}
			channel := discordTestChannel(t, tc.kind, "500")
			inbound := newDiscordInbound(t.Context(), discordTestBotID, "800", submit, func(context.Context, string) (discord.Channel, error) { return channel, nil }, func(generation uint64) bool { return generation == 3 }, func(generation uint64) bool { return generation == 3 }, nil)
			message := discordTestMessage()
			if tc.guild {
				message = discordTestGuildMessage("<@" + discordTestBotID + "> sample task")
			}
			if err := inbound.HandleMessage(3, message); err != nil {
				t.Fatal(err)
			}
			if submissions != 1 || got.Message.ChatKind != tc.want {
				t.Fatalf("submissions=%d kind=%q", submissions, got.Message.ChatKind)
			}
			if got.Message.MessageID != "message:9007199254740995" || got.SemanticID != got.Message.MessageID || got.Message.SenderID != "9007199254740997" || got.Message.AccountID != discordTestBotID || got.Message.ChatID != "500" || !got.SourceTime.Equal(message.CreatedAt) {
				t.Fatalf("identity changed: %#v", got)
			}
		})
	}
}

func TestDiscordInboundInvocationPreservesBody(t *testing.T) {
	for _, tc := range []struct {
		text, want string
		force      bool
	}{
		{"<@" + discordTestBotID + "> \t\n/bind /project", "/bind /project", false},
		{"<@!" + discordTestBotID + ">\u2003task <@123> <@" + discordTestBotID + ">", "task <@123> <@" + discordTestBotID + ">", false},
		{"task <@" + discordTestBotID + "> /bind /project", "task <@" + discordTestBotID + "> /bind /project", true},
		{"/bind /project <@" + discordTestBotID + ">", "/bind /project <@" + discordTestBotID + ">", true},
		{" <@" + discordTestBotID + "> /cancel", " <@" + discordTestBotID + "> /cancel", true},
		{"<@123> /answer <@" + discordTestBotID + ">", "<@123> /answer <@" + discordTestBotID + ">", true},
	} {
		t.Run(tc.text, func(t *testing.T) {
			var got im.IncomingMessage
			submit := func(_ context.Context, _ string, e im.IngressEnvelope, _ func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
				got = e.Message
				return nil, nil
			}
			inbound := newDiscordInbound(t.Context(), discordTestBotID, "800", submit, nil, func(uint64) bool { return true }, func(uint64) bool { return true }, nil)
			if err := inbound.HandleMessage(1, discordTestGuildMessage(tc.text)); err != nil {
				t.Fatal(err)
			}
			if got.Text != tc.want || got.ForceText != tc.force {
				t.Fatalf("text=%q ForceText=%t", got.Text, got.ForceText)
			}
			if tc.force && commandForIncomingMessage(got).Type != "send" {
				t.Fatal("body mention promoted a command")
			}
		})
	}
	var got im.IncomingMessage
	submit := func(_ context.Context, _ string, e im.IngressEnvelope, _ func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
		got = e.Message
		return nil, nil
	}
	inbound := newDiscordInbound(t.Context(), discordTestBotID, "800", submit, nil, func(uint64) bool { return true }, func(uint64) bool { return true }, nil)
	message := discordTestMessage()
	message.Content = "<@" + discordTestBotID + ">  /bind /project"
	if err := inbound.HandleMessage(1, message); err != nil {
		t.Fatal(err)
	}
	if got.Text != message.Content || got.ForceText {
		t.Fatal("DM invocation changed")
	}
}

func TestDiscordInboundFiltersBeforeLookup(t *testing.T) {
	for _, name := range []string{"bot", "system_author", "webhook", "system_message", "interaction", "not_mentioned", "text_mention_only"} {
		t.Run(name, func(t *testing.T) {
			message := discordTestGuildMessage("<@" + discordTestBotID + "> sample task")
			switch name {
			case "bot":
				message.Author.Bot = true
			case "system_author":
				message.Author.System = true
			case "webhook":
				id := snowflake.ID(123)
				message.WebhookID = &id
			case "system_message":
				message.Type = discord.MessageTypeUserJoin
			case "interaction":
				message.Type = discord.MessageTypeSlashCommand
			case "not_mentioned":
				message.Mentions = []discord.User{{ID: snowflake.ID(123)}}
			case "text_mention_only":
				message.Mentions = nil
			}
			submit := func(context.Context, string, im.IngressEnvelope, func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
				t.Fatal("irrelevant message submitted")
				return nil, nil
			}
			inbound := newDiscordInbound(t.Context(), discordTestBotID, "800", submit, nil, func(uint64) bool { return true }, func(uint64) bool { return true }, nil)
			if err := inbound.HandleMessage(1, message); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type discordTestIngressReceiver struct{ accepted chan im.IngressEnvelope }

func (r discordTestIngressReceiver) Accept(_ context.Context, envelope im.IngressEnvelope) im.ReceiveResult {
	r.accepted <- envelope
	return im.ReceiveResult{Disposition: im.ReceiveAccepted}
}

func TestDiscordInboundLookupDoesNotBlockGatewayAndPreservesCapturedGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	accepted := make(chan im.IngressEnvelope, 1)
	normalizer := NewIngressNormalizer(ctx, discordTestIngressReceiver{accepted})
	defer normalizer.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var generation atomic.Uint64
	generation.Store(1)
	resolve := func(ctx context.Context, id string) (discord.Channel, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return discordTestChannel(t, discord.ChannelTypeDM, id), nil
	}
	results := make(chan (<-chan im.ReceiveResult), 1)
	submit := func(ctx context.Context, key string, e im.IngressEnvelope, normalize func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
		future, err := normalizer.SubmitContext(ctx, key, func(ctx context.Context) (im.IngressEnvelope, error) { return normalize(ctx, e) })
		results <- future
		return future, err
	}
	inbound := newDiscordInbound(ctx, discordTestBotID, "800", submit, resolve, func(g uint64) bool { return generation.Load() == g }, func(g uint64) bool { return true }, nil)
	returned := make(chan error, 1)
	go func() { returned <- inbound.HandleMessage(1, discordTestMessage()) }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Gateway callback blocked on REST")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("lookup did not start")
	}
	generation.Store(2)
	close(release)
	result := WaitIngressNormalization(t.Context(), <-results)
	if result.Disposition != im.ReceiveAccepted {
		t.Fatal("captured message was lost across reconnect", result)
	}
	select {
	case envelope := <-accepted:
		if envelope.Message.MessageID != "message:9007199254740995" {
			t.Fatal("captured identity changed")
		}
	default:
		t.Fatal("captured message not admitted")
	}
	if err := inbound.HandleMessage(1, discordTestMessage()); !errors.Is(err, errDiscordStaleGeneration) {
		t.Fatal(err)
	}
}

func TestDiscordInboundChannelCacheAndBoundedSubmission(t *testing.T) {
	lookups := 0
	resolve := func(context.Context, string) (discord.Channel, error) {
		lookups++
		return discordTestChannel(t, discord.ChannelTypeDM, "500"), nil
	}
	submit := func(ctx context.Context, _ string, e im.IngressEnvelope, normalize func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
		_, err := normalize(ctx, e)
		return nil, err
	}
	inbound := newDiscordInbound(t.Context(), discordTestBotID, "800", submit, resolve, func(g uint64) bool { return g == 1 }, func(g uint64) bool { return g == 1 }, nil)
	inbound.CacheChannel(1, discordTestChannel(t, discord.ChannelTypeDM, "500"))
	if err := inbound.HandleMessage(1, discordTestMessage()); err != nil {
		t.Fatal(err)
	}
	if lookups != 0 {
		t.Fatal("cached channel looked up")
	}
	inbound.DeleteChannel(0, "500")
	if err := inbound.HandleMessage(1, discordTestMessage()); err != nil {
		t.Fatal(err)
	}
	if lookups != 0 {
		t.Fatal("stale generation deleted cache")
	}
	inbound.DeleteChannel(1, "500")
	if err := inbound.HandleMessage(1, discordTestMessage()); err != nil {
		t.Fatal(err)
	}
	if lookups != 1 {
		t.Fatal("deleted channel not refreshed")
	}
	for index := range discordChannelCacheCapacity + 50 {
		inbound.cacheChannel(fmt.Sprint(index), discordChannelMetadata{discord.ChannelTypeDM})
	}
	if len(inbound.channels) != discordChannelCacheCapacity {
		t.Fatal("channel cache unbounded")
	}
	inbound.submit = func(context.Context, string, im.IngressEnvelope, func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
		return nil, errIngressCapacity
	}
	if err := inbound.HandleMessage(1, discordTestMessage()); !errors.Is(err, errIngressCapacity) {
		t.Fatal("capacity error lost", err)
	}
}

func TestDiscordInboundPersistentResumeDeduplicationAndBodyCommand(t *testing.T) {
	router, _ := pairedTestRouterWithConfig(t, "ims: {wechat: {}, discord: {bot_token: placeholder, application_id: '800', owner: {account_id: '9007199254740993', user_id: '9007199254740997'}}}\n")
	if err := router.admission.pairing.PlatformReady("discord", discordTestBotID); err != nil {
		t.Fatal(err)
	}
	bind := im.IncomingMessage{IMType: "discord", AccountID: discordTestBotID, ChatKind: "group", ChatID: "500", SenderID: "9007199254740997", MessageID: "setup-binding", Text: fmt.Sprintf("/bind %q", t.TempDir())}
	if !dispatchPairingTest(router, bind) {
		t.Fatal("owner binding rejected")
	}
	store := testIngressStore(t)
	service := NewIngressService(store, router)
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	pauseIngressTestRoute(t, service, bind)
	futures := make(chan (<-chan im.ReceiveResult), 4)
	submit := func(ctx context.Context, key string, e im.IngressEnvelope, normalize func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
		future, err := router.SubmitIngressNormalization(ctx, key, e, normalize)
		if err == nil {
			futures <- future
		}
		return future, err
	}
	inbound := newDiscordInbound(t.Context(), discordTestBotID, "800", submit, nil, func(uint64) bool { return true }, func(uint64) bool { return true }, nil)
	inbound.CacheChannel(1, discordTestChannel(t, discord.ChannelTypeGuildPrivateThread, "500"))
	message := discordTestGuildMessage("/bind /another-project <@" + discordTestBotID + ">")
	if err := inbound.HandleMessage(1, message); err != nil {
		t.Fatal(err)
	}
	first := WaitIngressNormalization(t.Context(), <-futures)
	if first.Disposition != im.ReceiveAccepted {
		t.Fatal(first)
	}
	if err := inbound.HandleMessage(2, message); err != nil {
		t.Fatal(err)
	}
	duplicate := WaitIngressNormalization(t.Context(), <-futures)
	if duplicate.Disposition != im.ReceiveDuplicate || duplicate.RecordID != first.RecordID {
		t.Fatal(duplicate)
	}
	record, err := store.Get(first.RecordID)
	if err != nil || record.Kind != "task" || record.Text != message.Content || record.ChatID != "500" {
		t.Fatalf("body command changed into a control: %#v %v", record, err)
	}
	// A non-owner attempted /bind is a durable rejection. Resume redelivery
	// cannot turn it into a new pairing attempt or allocate a second record.
	message.ID++
	message.Author.ID = snowflake.ID(100)
	message.Content = "<@" + discordTestBotID + "> /bind /another-project"
	if err := inbound.HandleMessage(2, message); err != nil {
		t.Fatal(err)
	}
	rejected := WaitIngressNormalization(t.Context(), <-futures)
	if rejected.Disposition != im.ReceiveRejected || rejected.RecordID == "" {
		t.Fatal(rejected)
	}
	if err := inbound.HandleMessage(2, message); err != nil {
		t.Fatal(err)
	}
	rejectedDuplicate := WaitIngressNormalization(t.Context(), <-futures)
	if rejectedDuplicate.Disposition != im.ReceiveDuplicate || rejectedDuplicate.State != "rejected" || rejectedDuplicate.RecordID != rejected.RecordID {
		t.Fatal(rejectedDuplicate)
	}
	record, err = store.Get(rejected.RecordID)
	if err != nil || record.Text != "" {
		t.Fatal("rejected message retained its body", record, err)
	}
	message.ID++
	message.ChannelID = snowflake.ID(600)
	message.Author.ID = snowflake.ID(9007199254740997)
	message.GuildID = nil
	message.Content = "/bind /another-project"
	inbound.CacheChannel(2, discordTestChannel(t, discord.ChannelTypeGroupDM, "600"))
	if err := inbound.HandleMessage(2, message); err != nil {
		t.Fatal(err)
	}
	groupDM := WaitIngressNormalization(t.Context(), <-futures)
	if groupDM.Disposition != im.ReceiveRejected || groupDM.RecordID == "" {
		t.Fatal("GroupDM was treated as private", groupDM)
	}
}

func TestDiscordInboundIgnoresEmptyAndAttachmentOnlyMessages(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		guild      bool
	}{
		{"empty_dm", "", false}, {"whitespace_dm", " \t\n", false},
		{"mention_only", "<@" + discordTestBotID + ">", true},
		{"mention_whitespace", "<@!" + discordTestBotID + "> \n\t", true},
		{"surrounded_mention", " \t<@" + discordTestBotID + "> \n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := discordTestMessage()
			if tc.guild {
				message = discordTestGuildMessage(tc.text)
			} else {
				message.Content = tc.text
			}
			message.Attachments = []discord.Attachment{{Filename: "sample.txt"}}
			inbound := newDiscordInbound(t.Context(), discordTestBotID, "800", func(context.Context, string, im.IngressEnvelope, func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
				t.Fatal("unsupported empty text submitted")
				return nil, nil
			}, nil, func(uint64) bool { return true }, func(uint64) bool { return true }, nil)
			if err := inbound.HandleMessage(1, message); err != nil {
				t.Fatal(err)
			}
			delivery, err := inbound.CaptureMessage(1, message, nil)
			if err != nil || delivery != nil {
				t.Fatal("empty replay occupied delivery capacity", err)
			}
		})
	}
}

func discordContentlessReset(t *testing.T) {
	t.Helper()
	reset := func() {
		discordContentless.mu.Lock()
		defer discordContentless.mu.Unlock()
		discordContentless.last, discordContentless.reported = time.Time{}, 0
	}
	reset()
	t.Cleanup(reset)
}

func TestDiscordInboundReportsContentlessMentionWithoutFlooding(t *testing.T) {
	discordContentlessReset(t)
	var mu sync.Mutex
	var lines, events []string
	original := log.Warnf
	log.Warnf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { log.Warnf = original })
	submitted := 0
	inbound := newDiscordInbound(t.Context(), discordTestBotID, "800", func(context.Context, string, im.IngressEnvelope, func(context.Context, im.IngressEnvelope) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
		submitted++
		return nil, nil
	}, nil, func(uint64) bool { return true }, func(uint64) bool { return true }, func(event, reason string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event+"/"+reason)
	})
	reported := func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return len(lines), len(events)
	}
	// A guild reply to the bot mentions it but carries no text, because the
	// Message Content intent is deliberately not requested.
	for range 3 {
		if err := inbound.HandleMessage(1, discordTestGuildMessage("<@"+discordTestBotID+">")); err != nil {
			t.Fatal(err)
		}
	}
	if logs, metrics := reported(); logs != 1 || metrics != 3 || submitted != 0 {
		t.Fatalf("contentless mention reported incorrectly: logs=%d metrics=%d submitted=%d", logs, metrics, submitted)
	}
	discordContentless.mu.Lock()
	discordContentless.last = time.Now().Add(-2 * discordContentlessInterval)
	discordContentless.mu.Unlock()
	message := discordTestMessage()
	message.Content = " \t\n"
	if err := inbound.HandleMessage(1, message); err != nil {
		t.Fatal(err)
	}
	if logs, metrics := reported(); logs != 2 || metrics != 4 {
		t.Fatalf("later interval did not report: logs=%d metrics=%d", logs, metrics)
	}
	discordContentless.mu.Lock()
	discordContentless.reported, discordContentless.last = discordContentlessBudget, time.Time{}
	discordContentless.mu.Unlock()
	if err := inbound.HandleMessage(1, discordTestGuildMessage("<@!"+discordTestBotID+">")); err != nil {
		t.Fatal(err)
	}
	if logs, metrics := reported(); logs != 2 || metrics != 5 {
		t.Fatalf("diagnostic budget exceeded: logs=%d metrics=%d", logs, metrics)
	}
	mu.Lock()
	defer mu.Unlock()
	if lines[0] != "discord event=message_dropped category=contentless" {
		t.Fatalf("diagnostic text changed: %q", lines[0])
	}
	if events[0] != "callback_dropped/unsupported_content" {
		t.Fatalf("metric label changed: %q", events[0])
	}
}
