package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestScopedIdentityCanonical(t *testing.T) {
	t.Parallel()
	parts := []string{"slack-thread", "channel:with/slashes", "123.456"}
	value, err := EncodeScopedID("target-v1:", parts...)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeScopedID(value, "target-v1:", 3)
	if err != nil || strings.Join(decoded, "|") != strings.Join(parts, "|") {
		t.Fatalf("%v %v", decoded, err)
	}
	for _, bad := range []string{value + "=", strings.Replace(value, "target-v1:", "other:", 1), "target-v1:" + base64.RawURLEncoding.EncodeToString([]byte(`["slack-thread", "channel:with/slashes", "123.456"]`)), "target-v1:" + base64.RawURLEncoding.EncodeToString([]byte(`[null,"a","b"]`))} {
		if _, err := DecodeScopedID(bad, "target-v1:", 3); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	for _, parts := range [][]string{nil, {""}, {strings.Repeat("x", 2048)}, {string([]byte{255})}} {
		if _, err := EncodeScopedID("target-v1:", parts...); err == nil {
			t.Fatal("accepted invalid parts")
		}
	}
	for _, prefix := range []string{"", ":", "a", "A:", "a/b:"} {
		if _, err := EncodeScopedID(prefix, "x"); err == nil {
			t.Fatalf("accepted %q", prefix)
		}
	}
}

func TestAccountLockStableAcrossStateDirectories(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	first, err := AcquireIMAccountLock("telegram", "test-account")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := AcquireIMAccountLock("telegram", "test-account"); err == nil {
		second.Close()
		t.Fatal("same account lock acquired twice")
	}
	other, err := AcquireIMAccountLock("slack", "test-account")
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	first.Close()
	again, err := AcquireIMAccountLock("telegram", "test-account")
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

func TestPlatformCommandHints(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		platform, kind string
		thread         bool
		want           string
	}{
		{"slack", "private", false, "/chord bind /project"}, {"slack", "group", false, "/chord bind /project"}, {"slack", "group", true, "@bot_name /bind /project (select the bot from Slack's @ menu)"},
		{"discord", "private", false, "/bind /project"}, {"discord", "group", false, "<@bot> /bind /project"},
		{"telegram", "group", false, "/bind@bot_name /project"}, {"telegram", "private", false, "/bind /project"},
		{"wecom", "group", false, "<@bot> /bind /project"}, {"dingtalk", "group", false, "<@bot> /bind /project"},
	} {
		got := FormatCommandHint(PlatformPresentation{tt.platform, "<@bot>", "bot_name"}, CommandTarget{ChatKind: tt.kind, Thread: tt.thread}, "/bind /project")
		if got != tt.want {
			t.Fatalf("%s: got %q want %q", tt.platform, got, tt.want)
		}
	}
}

func TestNotificationHintsPreserveModelContent(t *testing.T) {
	t.Parallel()
	r := &NotificationRouter{}
	hint := func(cmd string) string {
		return FormatCommandHint(PlatformPresentation{Platform: "slack"}, CommandTarget{}, cmd)
	}
	state := ControlState{PendingConfirm: &ConfirmPayload{ToolName: "Done", DoneReport: "Keep /allow and /tmp/path in report"}}
	got := r.formatNotification("confirm_request", state, hint)
	if !strings.Contains(got, "Keep /allow and /tmp/path in report") || !strings.Contains(got, "Reply /chord allow") {
		t.Fatal(got)
	}
	state.LastAssistantText = "Use /allow from /tmp/path"
	if got := r.formatNotification("assistant_message", state, hint); got != state.LastAssistantText {
		t.Fatal(got)
	}
}
