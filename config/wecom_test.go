package config

import (
	"strings"
	"testing"
)

func TestWecomCredentialsAndFixedEndpoint(t *testing.T) {
	for _, fields := range []string{"bot_id: example-bot", "secret: example-secret", "bot_id: ' ', secret: example-secret", "bot_id: example-bot, secret: ' '"} {
		cfg, _, err := ParseDocument([]byte("ims: {wecom: {" + fields + "}}"))
		if err != nil {
			t.Fatal(err)
		}
		if err = cfg.Validate(); err == nil || !strings.Contains(err.Error(), "bot_id, secret") {
			t.Fatalf("credentials accepted or leaked: %v", err)
		}
	}
	input := "ims: {wecom: {bot_id: example-bot, secret: example-secret, endpoint: 'wss://example.invalid'}}"
	cfg, doc, err := ParseDocument([]byte(input))
	if err != nil || cfg.Validate() != nil {
		t.Fatalf("configuration parse: %v", err)
	}
	issues := UnsupportedConfigFields(doc)
	if len(issues) != 1 || !strings.Contains(issues[0], "ims.wecom.endpoint is not supported") {
		t.Fatalf("endpoint override not diagnosed: %v", issues)
	}
}

func TestWecomOwnerAccountMismatchRemainsResettable(t *testing.T) {
	// Pairing enforces the connected account. Loading must still succeed so a
	// local reset can revoke a previous bot's owner after credentials change.
	cfg, _, err := ParseDocument([]byte("ims: {wecom: {bot_id: new-bot, secret: example-secret, owner: {account_id: old-bot, user_id: example-owner}}}"))
	if err != nil || cfg.Validate() != nil {
		t.Fatalf("changed bot configuration cannot be reset: %v", err)
	}
}
