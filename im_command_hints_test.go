package main

import "testing"

func TestSlackCommandHintsKeepArgumentSyntax(t *testing.T) {
	p := PlatformPresentation{Platform: "slack", BotMention: "<@bot-example>", BotUsername: "ExampleBot"}
	for _, command := range []string{`/bind "/path/with spaces" code-example`, `/answer @question-example "two words"`, "/deny reason with  spaces", "/allow"} {
		if got, want := FormatCommandHint(p, CommandTarget{}, command), "/chord "+command[1:]; got != want {
			t.Fatalf("root: got %q want %q", got, want)
		}
		if got, want := FormatCommandHint(p, CommandTarget{Thread: true, ChatKind: "group"}, command), "@ExampleBot "+command+" (select the bot from Slack's @ menu)"; got != want {
			t.Fatalf("thread: got %q want %q", got, want)
		}
	}
	if got := FormatCommandHint(p, CommandTarget{}, "Open /tmp/example"); got != "Open /tmp/example" {
		t.Fatal(got)
	}
}

func TestSlackCommandTargetRecognizesDMThreads(t *testing.T) {
	thread, err := EncodeScopedID("target-v1:", "slack-thread", "dm-example", "123.000001")
	if err != nil {
		t.Fatal(err)
	}
	target := commandTargetForChat("slack", thread)
	if !target.Thread || target.ChatKind != "group" {
		t.Fatalf("target = %#v", target)
	}
	if commandTargetForChat("slack", "channel-example").Thread {
		t.Fatal("root target became a thread")
	}
}
