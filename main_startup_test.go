package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayStartupWarningsReachConfiguredLog(t *testing.T) {
	if path := os.Getenv("GATEWAY_TEST_WARNING_CONFIG"); path != "" {
		os.Args = []string{"chord-gateway", "--console", "--config", path}
		main()
		return
	}
	dir := t.TempDir()
	path, logfile := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "gateway.log")
	data := "ims: {wechat: {}}\nworkspaces:\n  sample:\n    path: " + filepath.ToSlash(dir) + "\nunknown_option: private-example-value\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestGatewayStartupWarningsReachConfiguredLog$")
	cmd.Env = append(os.Environ(), "GATEWAY_TEST_WARNING_CONFIG="+path, "CHORD_GATEWAY_STATE_DIR="+filepath.Join(dir, "state"), "CHORD_GATEWAY_LOG_FILE="+logfile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("startup failed: %v\n%s", err, out)
	}
	logged, err := os.ReadFile(logfile)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"terminal": string(out), "file": string(logged)} {
		if strings.Count(text, "unsupported field ignored: line 5: field unknown_option is not supported") != 1 {
			t.Fatalf("%s did not receive exactly one configuration warning: %s", name, text)
		}
		if strings.Contains(text, "private-example-value") {
			t.Fatalf("%s exposed the configuration value", name)
		}
	}
}
