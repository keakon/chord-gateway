package main

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/config"
)

// The user docs restate two event sets, and both have drifted from the code:
//
//   - the always-subscribed control-plane events, listed in
//     docs/event-visibility.md and docs/operations.md (each in both languages);
//   - the optional event_visibility flags, listed in docs/configuration.md and
//     repeated as YAML examples in config.example.yaml and three other pages.
//
// scripts/check-docs.py keeps a toolchain-free comparison of the required list
// between pages, so drift is still caught where Go is unavailable. These tests
// are the stricter counterpart: they compare the pages against the code, so a
// page can no longer agree with its siblings while disagreeing with the
// gateway.
//
// Every path here is repository-local and resolves relative to this package
// directory. The checks must keep working where only chord-gateway is checked
// out, so they never read a sibling checkout.

// requiredEventListDocs are the pages restating the always-subscribed set.
var requiredEventListDocs = []string{
	"docs/event-visibility.md",
	"docs/event-visibility_CN.md",
	"docs/operations.md",
	"docs/operations_CN.md",
}

// requiredEventListAnchors identify the event list inside a page. Only
// long-lived events belong here: a stale list that predates a new event must
// still be recognized, otherwise the failure reads as "list not found" instead
// of "list is missing the new events".
var requiredEventListAnchors = []string{"assistant_message", "idle", "error", "agent_done"}

// optionalFlagDocs are the pages restating the optional flags as a bullet list.
var optionalFlagDocs = []string{"docs/configuration.md", "docs/configuration_CN.md"}

// optionalFlagYAMLDocs are the files carrying an event_visibility YAML example.
var optionalFlagYAMLDocs = []string{
	"config.example.yaml",
	"docs/configuration.md",
	"docs/configuration_CN.md",
	"docs/event-visibility.md",
	"docs/event-visibility_CN.md",
}

func TestDocumentedRequiredEventsMatchCode(t *testing.T) {
	want := configuredHeadlessSubscribeEvents(&config.Config{})
	if duplicates := docDuplicatesIn(want); len(duplicates) > 0 {
		t.Fatalf("default subscribe list repeats %v", duplicates)
	}
	wantSet := docStringSet(want)
	for _, path := range requiredEventListDocs {
		got := documentedRequiredEvents(t, path)
		if missing := docNotIn(want, docStringSet(got)); len(missing) > 0 {
			t.Errorf("%s: required-event list is missing %v (the gateway subscribes to %d events, the page lists %d)",
				path, missing, len(want), len(got))
		}
		if extra := docNotIn(got, wantSet); len(extra) > 0 {
			t.Errorf("%s: required-event list documents %v, which is not in the default subscribe list", path, extra)
		}
	}
}

func TestDocumentedOptionalEventsMatchConfig(t *testing.T) {
	flags := docOptionalFlagEvents(t)
	wantFlags := docSortedKeys(flags)
	wantFlagSet := docStringSet(wantFlags)

	required := docStringSet(configuredHeadlessSubscribeEvents(&config.Config{}))
	for flag, event := range flags {
		if required[event] {
			t.Errorf("optional flag %q maps to %q, which is also always subscribed; an event must be one or the other", flag, event)
		}
	}

	for _, path := range optionalFlagDocs {
		got := documentedOptionalFlags(t, path)
		if missing := docNotIn(wantFlags, docStringSet(got)); len(missing) > 0 {
			t.Errorf("%s: optional-flag list is missing %v", path, missing)
		}
		if extra := docNotIn(got, wantFlagSet); len(extra) > 0 {
			t.Errorf("%s: optional-flag list documents %v, which EventVisibility has no field for", path, extra)
		}
	}

	for _, path := range optionalFlagYAMLDocs {
		blocks := eventVisibilityYAMLBlocks(t, path)
		if len(blocks) == 0 {
			t.Errorf("%s: no event_visibility example found", path)
			continue
		}
		for i, block := range blocks {
			// The examples list every flag rather than a subset, so a flag added
			// to EventVisibility must appear in each of them.
			if missing := docNotIn(wantFlags, docStringSet(block)); len(missing) > 0 {
				t.Errorf("%s: event_visibility example %d does not list %v", path, i+1, missing)
			}
			if extra := docNotIn(block, wantFlagSet); len(extra) > 0 {
				t.Errorf("%s: event_visibility example %d lists %v, which EventVisibility has no field for", path, i+1, extra)
			}
		}
	}
}

// docOptionalFlagEvents maps each event_visibility flag (its YAML key, which is
// what users write in a config) to the event it subscribes to. The mapping is
// resolved behaviourally — enable one flag, see which event appears — instead of
// assuming a field's YAML tag matches its event name.
func docOptionalFlagEvents(t *testing.T) map[string]string {
	t.Helper()
	typ := reflect.TypeOf(config.EventVisibility{})
	base := docStringSet(configuredHeadlessSubscribeEvents(&config.Config{}))
	events := make(map[string]string, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Bool {
			t.Fatalf("EventVisibility.%s is not a bool; this helper assumes one bool per flag", field.Name)
		}
		cfg := &config.Config{}
		reflect.ValueOf(&cfg.EventVisibility).Elem().Field(i).SetBool(true)
		added := docNotIn(configuredHeadlessSubscribeEvents(cfg), base)
		if len(added) != 1 {
			t.Fatalf("enabling EventVisibility.%s subscribes to %v, want exactly one new event", field.Name, added)
		}
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			t.Fatalf("EventVisibility.%s has no yaml tag to document", field.Name)
		}
		if _, exists := events[name]; exists {
			t.Fatalf("two EventVisibility fields share the yaml key %q", name)
		}
		events[name] = added[0]
	}
	return events
}

func docReadRepoFile(t *testing.T, path string) string {
	t.Helper()
	// Tests run with this package's directory as the working directory, and this
	// package is the repository root, so the documented paths are relative to it.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

var docEventBullet = regexp.MustCompile("^- `([a-z_]+)`$")

// docBulletBlocks returns every contiguous run of "- `name`" bullets in a page.
func docBulletBlocks(text string) [][]string {
	var blocks [][]string
	var current []string
	for _, line := range strings.Split(text, "\n") {
		if match := docEventBullet.FindStringSubmatch(line); match != nil {
			current = append(current, match[1])
			continue
		}
		if len(current) > 0 {
			blocks = append(blocks, current)
			current = nil
		}
	}
	if len(current) > 0 {
		blocks = append(blocks, current)
	}
	return blocks
}

// documentedRequiredEvents returns the always-subscribed event list a page
// documents, located by anchor events rather than by line number.
func documentedRequiredEvents(t *testing.T, path string) []string {
	t.Helper()
	var longest []string
	for _, block := range docBulletBlocks(docReadRepoFile(t, path)) {
		if !docContainsAll(block, requiredEventListAnchors) {
			continue
		}
		if len(block) > len(longest) {
			longest = block
		}
	}
	if longest == nil {
		t.Fatalf("%s: no bullet list contains %v; expected the always-subscribed event set", path, requiredEventListAnchors)
	}
	return longest
}

// documentedOptionalFlags returns the flag bullets of a page's event_visibility
// section. The section is located by its heading rather than by the flags it
// lists, so a stale or empty list cannot hide the check.
func documentedOptionalFlags(t *testing.T, path string) []string {
	t.Helper()
	lines := strings.Split(docReadRepoFile(t, path), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "## ") || !strings.Contains(trimmed, "event_visibility") {
			continue
		}
		if blocks := docBulletBlocks(strings.Join(lines[i:], "\n")); len(blocks) > 0 {
			return blocks[0]
		}
		t.Fatalf("%s: the event_visibility section has no flag list", path)
	}
	t.Fatalf("%s: no event_visibility section heading found", path)
	return nil
}

// eventVisibilityYAMLBlocks returns the flag keys of every `event_visibility:`
// block in a file. Each block is expected to list every flag: that is how the
// pages write them, and a block showing a subset would silently stop
// documenting flags added later.
func eventVisibilityYAMLBlocks(t *testing.T, path string) [][]string {
	t.Helper()
	var blocks [][]string
	var current []string
	inBlock := false
	for _, line := range strings.Split(docReadRepoFile(t, path), "\n") {
		if line == "event_visibility:" {
			if inBlock {
				blocks = append(blocks, current)
			}
			current = nil
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "  "); ok {
			if name, _, found := strings.Cut(rest, ":"); found {
				if name = strings.TrimSpace(name); name != "" {
					current = append(current, name)
				}
			}
			continue
		}
		blocks = append(blocks, current)
		current = nil
		inBlock = false
	}
	if inBlock {
		blocks = append(blocks, current)
	}
	return blocks
}

func docStringSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

// docNotIn returns the entries of names that set does not contain, sorted. Keep
// the argument order in mind at every call site: `docNotIn(want, gotSet)` is what
// the page is missing, `docNotIn(got, wantSet)` is what the page documents but the
// code does not subscribe to.
func docNotIn(names []string, set map[string]bool) []string {
	var missing []string
	for _, name := range names {
		if !set[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func docSortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func docContainsAll(names, wanted []string) bool {
	for _, want := range wanted {
		found := false
		for _, name := range names {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func docDuplicatesIn(names []string) []string {
	seen := make(map[string]bool, len(names))
	duplicates := make(map[string]bool)
	for _, name := range names {
		if seen[name] {
			duplicates[name] = true
			continue
		}
		seen[name] = true
	}
	return docSortedKeys(duplicates)
}
