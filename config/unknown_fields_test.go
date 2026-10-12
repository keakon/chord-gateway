package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/keakon/golog"
	"github.com/keakon/golog/log"

	"gopkg.in/yaml.v3"
)

func TestDecodeTargetsMatchConfig(t *testing.T) {
	// Config.UnmarshalYAML decodes Config through rawConfig and workspaces
	// through rawWorkspace. A field present in the shape but missing from the
	// decode target is silently dropped by Load while the walker reports it as
	// supported; a field present only there is reported as unsupported while it
	// works. ims and workspaces trade their slices for yaml.Node by design.
	pairs := []struct {
		name    string
		shape   reflect.Type
		decode  reflect.Type
		ignored map[string]bool
	}{
		{"rawConfig", reflect.TypeOf(Config{}), reflect.TypeOf(rawConfig{}), map[string]bool{"ims": true, "workspaces": true}},
		{"rawWorkspace", reflect.TypeOf(Workspace{}), reflect.TypeOf(rawWorkspace{}), nil},
	}
	for _, pair := range pairs {
		shapeFields := yamlConfigFields(pair.shape)
		decodeFields := yamlConfigFields(pair.decode)
		for name, typ := range shapeFields {
			if pair.ignored[name] {
				continue
			}
			decodeType, ok := decodeFields[name]
			if !ok {
				t.Errorf("%s is missing %s field %q", pair.name, pair.shape, name)
				continue
			}
			if decodeType != typ {
				t.Errorf("%s.%s type = %v, want %v", pair.name, name, decodeType, typ)
			}
		}
		for name := range decodeFields {
			if pair.ignored[name] {
				continue
			}
			if _, ok := shapeFields[name]; !ok {
				t.Errorf("%s has field %q that %s does not define", pair.name, name, pair.shape)
			}
		}
	}
}

func TestUnsupportedConfigFields(t *testing.T) {
	data := []byte(`
ims:
  feishu:
    app_id: cli_x
    app_secret: secret
    allowed_open_ids:
      - ou_owner
    chat_bindings:
      oc_chat: ws1
    approv: false
workspaces:
  ws1:
    path: /tmp/ws1
    pth: /tmp/other
chord_path: chord
event_visibility:
  activity: false
  background_result: false
  todos: true
  tool_result: false
`)
	issues := unsupportedFieldsFromYAML(t, data)
	joined := strings.Join(issues, "\n")
	for _, want := range []string{"ims.feishu.approv", "workspaces.ws1.pth", "event_visibility.activity", "event_visibility.tool_result"} {
		if !strings.Contains(joined, want) {
			t.Errorf("issues = %q, want a warning naming %q", joined, want)
		}
	}
	for _, supported := range []string{"app_secret", "allowed_open_ids", "oc_chat", "path", "chord_path", "background_result", "todos"} {
		if strings.Contains(joined, supported) {
			t.Errorf("issues = %q, must not flag supported key %q", joined, supported)
		}
	}
	if len(issues) != 4 {
		t.Errorf("issues = %q, want 4 entries", joined)
	}
	for _, issue := range issues {
		if !strings.Contains(issue, "line ") {
			t.Errorf("issue %q does not carry a line number", issue)
		}
	}
}

func TestUnsupportedConfigFieldsEmptyDocument(t *testing.T) {
	for _, doc := range []*yaml.Node{nil, {}} {
		if issues := unsupportedConfigFields(doc); issues != nil {
			t.Fatalf("issues = %v, want none", issues)
		}
	}
}

func unsupportedFieldsFromYAML(t *testing.T, data []byte) []string {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return unsupportedConfigFields(&doc)
}

func TestUnsupportedConfigFieldsAcceptsNormalizedAdapterKeys(t *testing.T) {
	// parseIMsNode lowercases and trims adapter names before matching, so these
	// spellings do configure the adapter and must not be reported themselves.
	cases := []struct {
		name     string
		data     string
		wantPath string
	}{
		{
			name:     "mixed case",
			data:     "ims:\n  Feishu:\n    app_id: cli_x\n    approv: true\n",
			wantPath: "ims.Feishu.approv",
		},
		{
			name:     "padded",
			data:     "ims:\n  \" feishu \":\n    app_id: cli_x\n    approv: true\n",
			wantPath: "ims. feishu .approv",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := unsupportedFieldsFromYAML(t, []byte(tc.data))
			if len(issues) != 1 || !strings.Contains(issues[0], tc.wantPath) {
				t.Fatalf("issues = %q, want one warning naming %q", issues, tc.wantPath)
			}
		})
	}
}

func TestUnsupportedConfigFieldsSkipsMergeKeys(t *testing.T) {
	// Merge syntax is supported and inherited fields use the target schema.
	data := []byte(`
ims:
  wechat: {}
workspaces:
  ws1: &ws
    path: /tmp/ws1
  ws2:
    <<: *ws
`)
	if issues := unsupportedFieldsFromYAML(t, data); len(issues) != 0 {
		t.Fatalf("issues = %v, want none", issues)
	}
}

func TestLoadLogsUnsupportedFields(t *testing.T) {
	var logged strings.Builder
	log.SetLogfFunc(func(msg string, args ...interface{}) {
		fmt.Fprintf(&logged, msg, args...)
		logged.WriteByte('\n')
	}, golog.WarnLevel)
	t.Cleanup(func() { log.SetDefaultLogger(nil) })

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
ims:
  wechat: {}
workspaces:
  ws1: &ws
    path: ~/project
storage_dir: /tmp/x
event_visibility:
  <<: *ws
  activity: false
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].ID != "ws1" {
		t.Fatalf("loaded Workspaces = %#v", cfg.Workspaces)
	}
	for _, want := range []string{"unsupported field ignored", "storage_dir", "event_visibility.activity", "event_visibility.path", path} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log %q does not mention %q", logged.String(), want)
		}
	}
}

func TestUnsupportedConfigFieldsAliasesAndMerges(t *testing.T) {
	tests := []struct {
		name string
		data string
		want []string
	}{
		{
			name: "alias uses target schema",
			data: "workspaces:\n  ws1: &ws {path: /tmp/project}\nevent_visibility: *ws\n",
			want: []string{"event_visibility.path"},
		},
		{
			name: "merge uses target schema",
			data: "workspaces:\n  ws1: &ws {path: /tmp/project}\nevent_visibility:\n  <<: *ws\n  todos: true\n",
			want: []string{"event_visibility.path"},
		},
		{
			name: "explicit value overrides inherited mapping",
			data: "defaults: &base\n  event_visibility: {invalid: true}\n<<: *base\nevent_visibility: {todos: true}\n",
			want: []string{"defaults"},
		},
		{
			name: "earlier merge value wins",
			data: "first: &first {event_visibility: {todos: true}}\nsecond: &second {event_visibility: {invalid: true}}\n<<: [*first, *second]\n",
			want: []string{"first", "second"},
		},
		{
			name: "nested merges keep earlier value",
			data: "base: &base {event_visibility: {todos: true}}\nfirst: &first {<<: *base}\nsecond: &second {event_visibility: {invalid: true}}\n<<: [*first, *second]\n",
			want: []string{"base", "first", "second"},
		},
		{
			name: "repeated merged field reported once",
			data: "workspaces:\n  ws1: &ws {path: /tmp/project}\nevent_visibility: {<<: [*ws, *ws]}\n",
			want: []string{"event_visibility.path"},
		},
		{
			name: "recursive merge terminates",
			data: "event_visibility: &cycle {<<: *cycle, invalid: true}\n",
			want: []string{"event_visibility.invalid"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues := unsupportedFieldsFromYAML(t, []byte(tt.data))
			if len(issues) != len(tt.want) {
				t.Fatalf("issues = %v, want fields %v", issues, tt.want)
			}
			for i, field := range tt.want {
				if !strings.HasSuffix(issues[i], "field "+field+" is not supported") {
					t.Errorf("issue = %q, want field %q", issues[i], field)
				}
			}
		})
	}
}
