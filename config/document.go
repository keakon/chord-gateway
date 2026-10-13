package config

import (
	"bytes"
	"errors"
	"io"

	"gopkg.in/yaml.v3"
)

// ParseDocument returns a runtime configuration and its original YAML
// nodes. Path expansion never changes the document used for persistence.
func ParseDocument(data []byte) (*Config, *yaml.Node, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, errors.New("invalid configuration YAML")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("configuration must be a mapping")
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, nil, errors.New("configuration must contain one document")
	}
	var cfg Config
	if err := doc.Decode(&cfg); err != nil {
		var typeError *yaml.TypeError
		if errors.As(err, &typeError) {
			return nil, nil, errors.New("invalid configuration field types")
		}
		return nil, nil, err
	}
	cfg.ExpandPath(Expand)
	return &cfg, &doc, nil
}
