package main

import (
	"bytes"
	"testing"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/buildinfo"
)

func TestRootCommandVersionUsesBuildIdentity(t *testing.T) {
	paths := &config.Paths{ConfigFile: "config.yaml"}
	flagConfig := paths.ConfigFile
	cmd := newRootCmd(paths, &flagConfig)

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}

	want := "chord-gateway version " + buildinfo.Current().Short() + "\n"
	if got := out.String(); got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}
