package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ParseDocument returns a runtime configuration, its original YAML nodes, and
// one warning per ignored duplicate key. Path expansion never changes the
// document used for persistence.
func ParseDocument(data []byte) (*Config, *yaml.Node, []string, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, nil, errors.New("invalid configuration YAML")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, nil, errors.New("configuration must be a mapping")
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, nil, nil, errors.New("configuration must contain one document")
	}
	var duplicateKeyWarnings []string
	dedupeMappingKeys(&doc, "configuration", &duplicateKeyWarnings, map[*yaml.Node]bool{})
	var cfg Config
	if err := doc.Decode(&cfg); err != nil {
		var typeError *yaml.TypeError
		if errors.As(err, &typeError) {
			return nil, nil, nil, errors.New("invalid configuration field types")
		}
		return nil, nil, nil, err
	}
	cfg.ExpandPath(Expand)
	return &cfg, &doc, duplicateKeyWarnings, nil
}

// dedupeMappingKeys drops later duplicate mapping keys from the document,
// keeping the first occurrence so runtime lookups (which read the first
// entry) stay consistent with the parsed configuration, and records one
// warning per dropped key. Merge keys are left untouched, and alias nodes are
// not followed because every anchor definition is part of the document tree.
func dedupeMappingKeys(node *yaml.Node, path string, warnings *[]string, visited map[*yaml.Node]bool) {
	if node == nil || visited[node] {
		return
	}
	visited[node] = true
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			dedupeMappingKeys(child, path, warnings, visited)
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			dedupeMappingKeys(child, path, warnings, visited)
		}
	case yaml.MappingNode:
		kept := make(map[string]bool, len(node.Content)/2)
		filtered := make([]*yaml.Node, 0, len(node.Content))
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key == nil || value == nil {
				filtered = append(filtered, key, value)
				continue
			}
			if key.Tag == "!!merge" {
				dedupeMappingKeys(value, path, warnings, visited)
				filtered = append(filtered, key, value)
				continue
			}
			if kept[key.Value] {
				*warnings = append(*warnings, fmt.Sprintf("line %d: duplicate key %s in %s is ignored; keeping the first occurrence", key.Line, key.Value, path))
				continue
			}
			kept[key.Value] = true
			dedupeMappingKeys(value, joinConfigPath(path, key.Value), warnings, visited)
			filtered = append(filtered, key, value)
		}
		node.Content = filtered
	}
}
