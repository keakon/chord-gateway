package main

import (
	"reflect"
	"testing"
)

func TestStoreWarningMessages(t *testing.T) {
	store := &ConfigStore{
		path:                 "/tmp/config.yaml",
		duplicateKeyWarnings: []string{"line 3: duplicate key owner"},
		warnings:             []string{"line 5: field unknown_option is not supported"},
	}
	got := storeWarningMessages(store)
	want := []string{
		"config /tmp/config.yaml: duplicate key ignored: line 3: duplicate key owner",
		"config /tmp/config.yaml: unsupported field ignored: line 5: field unknown_option is not supported",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("storeWarningMessages = %q, want %q", got, want)
	}
}
