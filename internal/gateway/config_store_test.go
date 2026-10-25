package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/keakon/chord-gateway/config"
)

func testConfigStore(t *testing.T, data string) *ConfigStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
func testConfigMutation(key, value string) ConfigMutation {
	return ConfigMutation{Apply: func(doc *yaml.Node, _ *config.Config) error {
		setMappingValue(doc.Content[0], key, scalarNode(value, 0))
		return nil
	}}
}

func TestConfigStoreConcurrentAndExternalEdit(t *testing.T) {
	s := testConfigStore(t, "# retained\nims: {wechat: {}}\n")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, key := range []string{"chord_path", "chord_state_dir"} {
		wg.Go(func() { _, err := s.Commit(context.Background(), testConfigMutation(key, "/tmp/example")); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := s.Snapshot()
	if cfg.ChordPath != "/tmp/example" || cfg.ChordStateDir != "/tmp/example" {
		t.Fatal("lost concurrent mutation")
	}
	if err := os.WriteFile(s.path, []byte("# external edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(context.Background(), testConfigMutation("chord_path", "/tmp/changed")); !errors.Is(err, errConfigChanged) {
		t.Fatal("external edit not detected")
	}
	if s.Snapshot() != cfg {
		t.Fatal("failed mutation published")
	}
}

func TestConfigStoreFailurePhases(t *testing.T) {
	for _, stage := range []string{"write", "short_write", "sync", "close", "replace", "directory_sync", "check"} {
		t.Run(stage, func(t *testing.T) {
			s := testConfigStore(t, "ims: {wechat: {}}\n")
			original := s.Snapshot()
			failure := errors.New("injected")
			switch stage {
			case "write":
				s.writer.write = func(*os.File, []byte) (int, error) { return 0, failure }
			case "short_write":
				s.writer.write = func(*os.File, []byte) (int, error) { return 0, nil }
			case "sync":
				s.writer.sync = func(*os.File) error { return failure }
			case "close":
				s.writer.close = func(*os.File) error { return failure }
			case "replace":
				s.writer.replace = func(string, string) (CommitOutcome, error) { return CommitOutcome{BeforeReplace}, failure }
			case "directory_sync":
				s.writer.replace = func(temp, path string) (CommitOutcome, error) {
					if err := os.Rename(temp, path); err != nil {
						t.Fatal(err)
					}
					return CommitOutcome{Replaced}, failure
				}
			}
			mutation := testConfigMutation("chord_path", "/tmp/example")
			checks := 0
			if stage == "check" {
				mutation.Check = func() error {
					checks++
					if checks == 2 {
						return failure
					}
					return nil
				}
			}
			result, err := s.Commit(context.Background(), mutation)
			if err == nil {
				t.Fatal("failure ignored")
			}
			if s.Snapshot() != original {
				t.Fatal("failed mutation published")
			}
			if stage == "directory_sync" {
				if result.Phase != Replaced || !s.faulted.Load() {
					t.Fatal("uncertain replacement not faulted")
				}
				select {
				case <-s.Fatal():
				default:
					t.Fatal("fatal not signaled")
				}
			} else if result.Phase != BeforeReplace || s.faulted.Load() {
				t.Fatal("pre-replacement failure classified incorrectly")
			}
		})
	}
}

func TestConfigStoreSurfacesDuplicateKeyWarningsAndPersistsDeduped(t *testing.T) {
	s := testConfigStore(t, "ims: {wechat: {}}\nworkspaces: {one: {path: /tmp/one}, one: {path: /tmp/two}}\n")
	if len(s.duplicateKeyWarnings) != 1 || !strings.Contains(s.duplicateKeyWarnings[0], "duplicate key one in configuration.workspaces") {
		t.Fatalf("expected duplicate key warning, got %v", s.duplicateKeyWarnings)
	}
	if ws := s.Snapshot().WorkspaceByID("one"); ws == nil || ws.Path != "/tmp/one" {
		t.Fatalf("first occurrence must win, got %+v", s.Snapshot().Workspaces)
	}
	if _, err := s.Commit(context.Background(), testConfigMutation("chord_path", "/tmp/example")); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Load(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Workspaces) != 1 || reloaded.Workspaces[0].ID != "one" {
		t.Fatalf("duplicate workspace persisted, got %+v", reloaded.Workspaces)
	}
}

func TestConfigStorePreservesMixedExplicitRoutes(t *testing.T) {
	s := testConfigStore(t, "ims:\n  feishu: {app_id: app, app_secret: secret, owner: {account_id: app, user_id: owner}, chat_bindings: {existing: original}}\n  wechat: {}\nworkspaces: {original: {path: /tmp/original}}\n")
	_, err := s.Commit(context.Background(), ConfigMutation{Apply: func(doc *yaml.Node, _ *config.Config) error {
		node := mappingValue(doc.Content[0], "workspaces")
		return upsertWorkspaceNode(node, "added", "/tmp/added")
	}})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Load(s.path)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := reloaded.ResolveWorkspace("feishu", "existing")
	if err != nil || ws.ID != "original" {
		t.Fatal("explicit route changed")
	}
}

func TestIndependentMappingDoesNotMutateAlias(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("first: &shared {app_id: app, app_secret: secret}\nsecond: *shared\n"), &doc); err != nil {
		t.Fatal(err)
	}
	second, err := independentMapping(doc.Content[0], "second")
	if err != nil {
		t.Fatal(err)
	}
	setMappingValue(second, "app_id", scalarNode("changed", 0))
	if mappingValue(mappingValue(doc.Content[0], "first"), "app_id").Value != "app" {
		t.Fatal("alias source changed")
	}
}

func TestIndependentMappingMergePrecedence(t *testing.T) {
	for _, data := range []string{
		"<<: [{settings: {value: first}}, {settings: {value: second}}]\n",
		"<<: {settings: {value: inherited}}\nsettings: {value: first}\n",
		"defaults: &defaults {settings: {value: first}}\n<<: *defaults\n",
		"settings: {<<: [{value: first}, {value: second}]}\n",
		"settings: {<<: {value: inherited}, value: first}\n",
	} {
		t.Run(data, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(data), &doc); err != nil {
				t.Fatal(err)
			}
			settings, err := independentMapping(doc.Content[0], "settings")
			if err != nil {
				t.Fatal(err)
			}
			if value := mappingValue(settings, "value"); value == nil || value.Value != "first" {
				t.Fatalf("wrong effective value: %+v", value)
			}
			setMappingValue(settings, "value", scalarNode("changed", 0))
			if defaults := mappingValue(doc.Content[0], "defaults"); defaults != nil {
				if mappingValue(mappingValue(defaults, "settings"), "value").Value != "first" {
					t.Fatal("mutation changed merge source")
				}
			}
		})
	}
}
