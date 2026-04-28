package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve_XDGDefaults(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Resolve("", "", "", "", "", "", home)
	if err != nil {
		t.Fatal(err)
	}

	wantConfig := filepath.Join(home, ".config", "chord-gateway", "config.yaml")
	if p.ConfigFile != wantConfig {
		t.Errorf("ConfigFile = %q, want %q", p.ConfigFile, wantConfig)
	}
	wantState := filepath.Join(home, ".local", "state", "chord-gateway")
	if p.StateDir != wantState {
		t.Errorf("StateDir = %q, want %q", p.StateDir, wantState)
	}
	wantLog := filepath.Join(wantState, "gateway.log")
	if p.LogFile != wantLog {
		t.Errorf("LogFile = %q, want %q", p.LogFile, wantLog)
	}
	if p.ConfigHome != filepath.Join(home, ".config", "chord-gateway") {
		t.Errorf("ConfigHome = %q", p.ConfigHome)
	}
	if p.DedupeDir != wantState {
		t.Errorf("DedupeDir = %q, want %q", p.DedupeDir, wantState)
	}
}

func TestResolve_ConfigHomeOverride(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(home, "my-config")
	p, err := Resolve(explicit, "", "", "", "", "", home)
	if err != nil {
		t.Fatal(err)
	}

	if p.ConfigHome != explicit {
		t.Errorf("ConfigHome = %q, want %q", p.ConfigHome, explicit)
	}
	if p.ConfigFile != filepath.Join(explicit, "config.yaml") {
		t.Errorf("ConfigFile = %q", p.ConfigFile)
	}
}

func TestResolve_ConfigFileOverride(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	explicit := "/etc/chord-gateway/my.yaml"
	p, err := Resolve("/some/home", explicit, "", "", "", "", home)
	if err != nil {
		t.Fatal(err)
	}

	if p.ConfigFile != explicit {
		t.Errorf("ConfigFile = %q, want %q", p.ConfigFile, explicit)
	}
	// ConfigHome should still be the resolved one.
	if p.ConfigHome != "/some/home" {
		t.Errorf("ConfigHome = %q, want /some/home", p.ConfigHome)
	}
}

func TestResolve_StateDirOverride(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	explicit := "/tmp/my-state"
	p, err := Resolve("", "", explicit, "", "", "", home)
	if err != nil {
		t.Fatal(err)
	}

	if p.StateDir != explicit {
		t.Errorf("StateDir = %q, want %q", p.StateDir, explicit)
	}
	if p.LogFile != filepath.Join(explicit, "gateway.log") {
		t.Errorf("LogFile = %q", p.LogFile)
	}
	if p.DedupeDir != explicit {
		t.Errorf("DedupeDir = %q", p.DedupeDir)
	}
}

func TestResolve_LogFileOverride(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	explicit := "/var/log/chord-gw.log"
	p, err := Resolve("", "", "", explicit, "", "", home)
	if err != nil {
		t.Fatal(err)
	}

	if p.LogFile != explicit {
		t.Errorf("LogFile = %q, want %q", p.LogFile, explicit)
	}
}

func TestResolve_XDGConfigHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	xdgConfig := filepath.Join(home, ".xdg", "config")
	p, err := Resolve("", "", "", "", xdgConfig, "", home)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(xdgConfig, "chord-gateway", "config.yaml")
	if p.ConfigFile != want {
		t.Errorf("ConfigFile = %q, want %q", p.ConfigFile, want)
	}
}

func TestResolve_XDGStateHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	xdgState := filepath.Join(home, ".xdg", "state")
	p, err := Resolve("", "", "", "", "", xdgState, home)
	if err != nil {
		t.Fatal(err)
	}

	wantState := filepath.Join(xdgState, "chord-gateway")
	if p.StateDir != wantState {
		t.Errorf("StateDir = %q, want %q", p.StateDir, wantState)
	}
	wantLog := filepath.Join(wantState, "gateway.log")
	if p.LogFile != wantLog {
		t.Errorf("LogFile = %q, want %q", p.LogFile, wantLog)
	}
}

func TestResolve_AllOverrides(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Resolve(
		"/custom/config/home",
		"/custom/config/file.yaml",
		"/custom/state",
		"/custom/gw.log",
		"/xdg/config",
		"/xdg/state",
		home,
	)
	if err != nil {
		t.Fatal(err)
	}

	if p.ConfigHome != "/custom/config/home" {
		t.Errorf("ConfigHome = %q", p.ConfigHome)
	}
	if p.ConfigFile != "/custom/config/file.yaml" {
		t.Errorf("ConfigFile = %q", p.ConfigFile)
	}
	if p.StateDir != "/custom/state" {
		t.Errorf("StateDir = %q", p.StateDir)
	}
	if p.LogFile != "/custom/gw.log" {
		t.Errorf("LogFile = %q", p.LogFile)
	}
}

func TestResolveFromEnv_EmptyEnv(t *testing.T) {
	// Ensure no env vars are set; ResolveFromEnv falls back to XDG defaults.
	for _, k := range []string{envConfigHome, envConfigFile, envStateDir, envLogFile} {
		old := os.Getenv(k)
		os.Unsetenv(k)
		defer os.Setenv(k, old)
	}
	p, err := ResolveFromEnv("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wantConfig := filepath.Join(home, ".config", "chord-gateway", "config.yaml")
	if p.ConfigFile != wantConfig {
		t.Errorf("ConfigFile = %q, want %q", p.ConfigFile, wantConfig)
	}
}

func TestResolveFromEnv_ConfigFileEnv(t *testing.T) {
	for _, k := range []string{envConfigHome, envConfigFile, envStateDir, envLogFile} {
		old := os.Getenv(k)
		os.Unsetenv(k)
		defer os.Setenv(k, old)
	}
	os.Setenv(envConfigFile, "/env/config.yaml")
	p, err := ResolveFromEnv("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigFile != "/env/config.yaml" {
		t.Errorf("ConfigFile = %q, want /env/config.yaml", p.ConfigFile)
	}
}

func TestResolveFromEnv_ConfigHomeEnv(t *testing.T) {
	for _, k := range []string{envConfigHome, envConfigFile, envStateDir, envLogFile} {
		old := os.Getenv(k)
		os.Unsetenv(k)
		defer os.Setenv(k, old)
	}
	os.Setenv(envConfigHome, "/env/config/home")
	p, err := ResolveFromEnv("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigHome != "/env/config/home" {
		t.Errorf("ConfigHome = %q", p.ConfigHome)
	}
	// When only configHome is set via env, configFile should derive from it.
	if p.ConfigFile != "/env/config/home/config.yaml" {
		t.Errorf("ConfigFile = %q", p.ConfigFile)
	}
}

func TestResolveFromEnv_StateDirEnv(t *testing.T) {
	for _, k := range []string{envConfigHome, envConfigFile, envStateDir, envLogFile} {
		old := os.Getenv(k)
		os.Unsetenv(k)
		defer os.Setenv(k, old)
	}
	os.Setenv(envStateDir, "/env/state")
	p, err := ResolveFromEnv("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.StateDir != "/env/state" {
		t.Errorf("StateDir = %q", p.StateDir)
	}
	if p.LogFile != "/env/state/gateway.log" {
		t.Errorf("LogFile = %q", p.LogFile)
	}
}

func TestResolveFromEnv_LogFileEnv(t *testing.T) {
	for _, k := range []string{envConfigHome, envConfigFile, envStateDir, envLogFile} {
		old := os.Getenv(k)
		os.Unsetenv(k)
		defer os.Setenv(k, old)
	}
	os.Setenv(envLogFile, "/env/custom.log")
	p, err := ResolveFromEnv("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.LogFile != "/env/custom.log" {
		t.Errorf("LogFile = %q, want /env/custom.log", p.LogFile)
	}
}

func TestResolveFromEnv_CLIOverridesEnv(t *testing.T) {
	for _, k := range []string{envConfigHome, envConfigFile, envStateDir, envLogFile} {
		old := os.Getenv(k)
		os.Unsetenv(k)
		defer os.Setenv(k, old)
	}
	os.Setenv(envConfigFile, "/env/config.yaml")
	os.Setenv(envStateDir, "/env/state-dir")
	os.Setenv(envLogFile, "/env/custom.log")

	p, err := ResolveFromEnv("", "/cli/config.yaml", "/cli/state", "/cli/gw.log")
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigFile != "/cli/config.yaml" {
		t.Errorf("ConfigFile CLI override: got %q, want /cli/config.yaml", p.ConfigFile)
	}
	if p.StateDir != "/cli/state" {
		t.Errorf("StateDir CLI override: got %q, want /cli/state", p.StateDir)
	}
	if p.LogFile != "/cli/gw.log" {
		t.Errorf("LogFile CLI override: got %q, want /cli/gw.log", p.LogFile)
	}
}

func TestResolveFromEnv_EnvUsedWhenCLIAbsent(t *testing.T) {
	for _, k := range []string{envConfigHome, envConfigFile, envStateDir, envLogFile} {
		old := os.Getenv(k)
		os.Unsetenv(k)
		defer os.Setenv(k, old)
	}
	os.Setenv(envLogFile, "/env/custom.log")
	p, err := ResolveFromEnv("", "", "", "/cli/gw.log")
	if err != nil {
		t.Fatal(err)
	}
	if p.LogFile != "/cli/gw.log" {
		t.Errorf("LogFile CLI override: got %q, want /cli/gw.log", p.LogFile)
	}

	p, err = ResolveFromEnv("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.LogFile != "/env/custom.log" {
		t.Errorf("LogFile env fallback: got %q, want /env/custom.log", p.LogFile)
	}
}

func TestExpand(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		input, want string
	}{
		{"~/.config/foo", filepath.Join(home, ".config/foo")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"~", home},
	}
	for _, tc := range tests {
		got := Expand(tc.input)
		if got != tc.want {
			t.Errorf("Expand(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
