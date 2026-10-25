package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"

	"github.com/keakon/chord-gateway/config"
)

var (
	errConfigChanged = errors.New("configuration changed on disk")
	errConfigFaulted = errors.New("configuration persistence outcome is unknown; restart required")
)

// ConfigStore serializes mutations and publishes only durable snapshots.
type ConfigStore struct {
	txMu                 sync.Mutex
	path                 string
	lastBytes            []byte
	warnings             []string
	duplicateKeyWarnings []string
	snapshot             atomic.Pointer[config.Config]
	faulted              atomic.Bool
	closing              atomic.Bool
	fatal                chan struct{}
	writer               configWriteOps
}

type ConfigMutation struct {
	Apply func(*yaml.Node, *config.Config) error
	// Check runs again immediately before replacement, including nonce expiry.
	Check func() error
}

func NewConfigStore(path string) (*ConfigStore, error) {
	path, err := config.CanonicalResource(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, doc, duplicateKeyWarnings, err := config.ParseDocument(data)
	if err != nil {
		return nil, err
	}
	if err = cfg.Validate(); err != nil {
		return nil, err
	}
	s := &ConfigStore{path: path, lastBytes: data, warnings: config.UnsupportedConfigFields(doc), duplicateKeyWarnings: duplicateKeyWarnings, fatal: make(chan struct{}), writer: defaultConfigWriteOps()}
	s.snapshot.Store(cfg)
	return s, nil
}
func (s *ConfigStore) Snapshot() *config.Config { return s.snapshot.Load() }
func (s *ConfigStore) Fatal() <-chan struct{}   { return s.fatal }

// storeWarningMessages renders the load-time configuration diagnostics a store
// collected, so both the gateway logger and the pairing CLI can surface them
// before a rewrite silently drops the offending lines.
func storeWarningMessages(s *ConfigStore) []string {
	messages := make([]string, 0, len(s.duplicateKeyWarnings)+len(s.warnings))
	for _, warning := range s.duplicateKeyWarnings {
		messages = append(messages, fmt.Sprintf("config %s: duplicate key ignored: %s", s.path, warning))
	}
	for _, warning := range s.warnings {
		messages = append(messages, fmt.Sprintf("config %s: unsupported field ignored: %s", s.path, warning))
	}
	return messages
}
func (s *ConfigStore) checkDisk() error {
	b, e := os.ReadFile(s.path)
	if e != nil {
		return e
	}
	if !bytes.Equal(b, s.lastBytes) {
		return errConfigChanged
	}
	return nil
}

func (s *ConfigStore) Commit(ctx context.Context, mutation ConfigMutation) (CommitOutcome, error) {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	before := CommitOutcome{BeforeReplace}
	if s.closing.Load() {
		return before, errors.New("configuration store is closing")
	}
	if s.faulted.Load() {
		return before, errConfigFaulted
	}
	if err := s.checkDisk(); err != nil {
		return before, err
	}
	current, doc, _, err := config.ParseDocument(s.lastBytes)
	if err != nil {
		return before, err
	}
	if err = current.Validate(); err != nil {
		return before, err
	}
	doc, err = copyConfigurationNodes(doc, map[*yaml.Node]bool{})
	if err != nil {
		return before, err
	}
	if mutation.Check != nil {
		if err = mutation.Check(); err != nil {
			return before, err
		}
	}
	if err = mutation.Apply(doc, current); err != nil {
		return before, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err = enc.Encode(doc); err != nil {
		return before, err
	}
	if err = enc.Close(); err != nil {
		return before, err
	}
	candidate, candidateDoc, candidateDuplicates, err := config.ParseDocument(buf.Bytes())
	if err != nil {
		return before, err
	}
	if err = candidate.Validate(); err != nil {
		return before, err
	}
	check := func() error {
		if s.closing.Load() {
			return errors.New("configuration store is closing")
		}
		if err := s.checkDisk(); err != nil {
			return err
		}
		if mutation.Check != nil {
			return mutation.Check()
		}
		return nil
	}
	outcome, err := writeConfigDurable(ctx, s.path, buf.Bytes(), check, s.writer)
	if err != nil {
		if outcome.Phase >= Replaced {
			s.faulted.Store(true)
			close(s.fatal)
		}
		return outcome, err
	}
	s.lastBytes = bytes.Clone(buf.Bytes())
	s.snapshot.Store(candidate)
	s.warnings = config.UnsupportedConfigFields(candidateDoc)
	s.duplicateKeyWarnings = candidateDuplicates
	return outcome, nil
}

func setMappingValue(node *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content, scalarNode(key, 0), value)
}
func removeMappingValue(node *yaml.Node, key string) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return
		}
	}
}

// Materialize effective mappings before changing alias/merge-backed fields.
func independentMapping(parent *yaml.Node, key string) (*yaml.Node, error) {
	// Resolve the parent first: the requested child may exist only in a merge.
	effective, err := materializeMapping(parent, map[*yaml.Node]bool{})
	if err != nil {
		return nil, err
	}
	*parent = *effective
	node := mappingValue(parent, key)
	if node == nil || node.Tag == "!!null" {
		node = mappingNode()
		setMappingValue(parent, key, node)
		return node, nil
	}
	out, err := materializeMapping(node, map[*yaml.Node]bool{})
	if err != nil {
		return nil, err
	}
	setMappingValue(parent, key, out)
	return out, nil
}

// ims has already been materialized by independentMapping. Preserve the
// original key spelling while applying the same normalization as config loading.
func independentIMMapping(ims *yaml.Node, platform string) (*yaml.Node, error) {
	platform = config.NormalizeIMType(platform)
	for i := 0; i+1 < len(ims.Content); i += 2 {
		if config.NormalizeIMType(ims.Content[i].Value) == platform {
			return independentMapping(ims, ims.Content[i].Value)
		}
	}
	return independentMapping(ims, platform)
}

func materializeMapping(n *yaml.Node, seen map[*yaml.Node]bool) (*yaml.Node, error) {
	if n == nil || seen[n] {
		return nil, errors.New("cyclic configuration mapping")
	}
	seen[n] = true
	defer delete(seen, n)
	if n.Kind == yaml.AliasNode {
		return materializeMapping(n.Alias, seen)
	}
	if n.Kind != yaml.MappingNode {
		return nil, errors.New("expected configuration mapping")
	}
	out := *n
	out.Anchor = ""
	out.Content = nil
	keys := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Tag != "!!merge" {
			out.Content = append(out.Content, n.Content[i], n.Content[i+1])
			keys[n.Content[i].Value] = true
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Tag != "!!merge" {
			continue
		}
		merge := n.Content[i+1]
		parts := []*yaml.Node{merge}
		if merge.Kind == yaml.SequenceNode {
			parts = merge.Content
		}
		for _, part := range parts {
			m, e := materializeMapping(part, seen)
			if e != nil {
				return nil, e
			}
			for j := 0; j+1 < len(m.Content); j += 2 {
				if !keys[m.Content[j].Value] {
					out.Content = append(out.Content, m.Content[j], m.Content[j+1])
					keys[m.Content[j].Value] = true
				}
			}
		}
	}
	return &out, nil
}

// Resolve aliases into independent nodes before mutation; shared mappings can
// never inherit an authorization edit made for another platform.
func copyConfigurationNodes(node *yaml.Node, active map[*yaml.Node]bool) (*yaml.Node, error) {
	if node == nil {
		return nil, nil
	}
	if active[node] {
		return nil, errors.New("cyclic configuration aliases are not writable")
	}
	active[node] = true
	defer delete(active, node)
	if node.Kind == yaml.AliasNode {
		copy, err := copyConfigurationNodes(node.Alias, active)
		if err == nil {
			copy.Anchor = ""
		}
		return copy, err
	}
	copy := *node
	copy.Content = nil
	for _, child := range node.Content {
		cloned, err := copyConfigurationNodes(child, active)
		if err != nil {
			return nil, err
		}
		copy.Content = append(copy.Content, cloned)
	}
	return &copy, nil
}
