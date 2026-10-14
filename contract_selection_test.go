package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestContractExplicitRepoOverridesInstalledBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH fixture uses a shell script")
	}
	repo := t.TempDir()
	mainDir := filepath.Join(repo, "cmd", "chord")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(repo, "go.mod"):     "module example.invalid/contract-fixture\n\ngo 1.27.0\n",
		filepath.Join(mainDir, "main.go"): "package main\nfunc main(){println(\"selected-source\")}\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binDir := t.TempDir()
	installed := filepath.Join(binDir, "chord")
	if err := os.WriteFile(installed, []byte("#!/bin/sh\necho stale-installed-binary\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CHORD_GATEWAY_CONTRACT_CHORD_BIN", "")
	t.Setenv("CHORD_GATEWAY_CONTRACT_CHORD_REPO", repo)
	selected := mustChordBinary(t)
	output, err := exec.Command(selected).CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "selected-source" {
		t.Fatalf("explicit source was not selected: binary=%s output=%s error=%v", selected, output, err)
	}
	// A deliberately supplied binary remains the strongest selection.
	t.Setenv("CHORD_GATEWAY_CONTRACT_CHORD_BIN", installed)
	if got := mustChordBinary(t); got != installed {
		t.Fatalf("explicit binary=%s want %s", got, installed)
	}
}
