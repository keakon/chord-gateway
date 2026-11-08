package gateway

import (
	"reflect"
	"testing"
)

func TestStoreWarningMessages(t *testing.T) {
	store := &ConfigStore{
		path:                 "/tmp/config.yaml",
		duplicateKeyWarnings: []string{"line 3: duplicate key owner"},
		warnings:             []string{"line 5: field unknown_option is not supported"},
		fallbackWarnings:     []string{`invalid idle_timeout "soon" ignored, using the 30m default`},
	}
	got := storeWarningMessages(store)
	want := []string{
		"config /tmp/config.yaml: duplicate key ignored: line 3: duplicate key owner",
		"config /tmp/config.yaml: unsupported field ignored: line 5: field unknown_option is not supported",
		`config /tmp/config.yaml: invalid idle_timeout "soon" ignored, using the 30m default`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("storeWarningMessages = %q, want %q", got, want)
	}
}

// config.Load runs before the gateway logger is installed, so an invalid
// idle_timeout must reach operators through the store's warning replay.
func TestStoreReplaysIdleTimeoutFallback(t *testing.T) {
	store := testConfigStore(t, "ims: {wechat: {}}\nidle_timeout: not-a-duration\n")
	got := storeWarningMessages(store)
	want := `config ` + store.path + `: invalid idle_timeout "not-a-duration" ignored, using the 30m default`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("storeWarningMessages = %q, want [%q]", got, want)
	}
}
