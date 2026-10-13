package main

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	value, _ := mappingEntry(node, key)
	return value
}

func mappingEntry(node *yaml.Node, key string) (value *yaml.Node, keyNode *yaml.Node) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i] != nil && node.Content[i].Value == key {
			return node.Content[i+1], node.Content[i]
		}
	}
	return nil, nil
}

func workspaceEntryByID(node *yaml.Node, workspaceID string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	return mappingValue(node, workspaceID)
}

func upsertWorkspaceNode(workspacesNode *yaml.Node, workspaceID, workspacePath string) error {
	workspacesNode.Style = 0
	workspaceNode := workspaceEntryByID(workspacesNode, workspaceID)
	if workspaceNode == nil {
		workspacesNode.Content = append(workspacesNode.Content,
			scalarNode(workspaceID, yamlStringStyle(workspaceID)),
			mappingNode(
				scalarNode("path", 0), scalarNode(workspacePath, yamlStringStyle(workspacePath)),
			),
		)
		return nil
	}
	if workspaceNode.Kind != yaml.MappingNode {
		return fmt.Errorf("parse config: workspace %q must be a YAML mapping", workspaceID)
	}
	pathNode := mappingValue(workspaceNode, "path")
	if pathNode == nil {
		workspaceNode.Style = 0
		workspaceNode.Content = append(workspaceNode.Content,
			scalarNode("path", 0),
			scalarNode(workspacePath, yamlStringStyle(workspacePath)),
		)
		return nil
	}
	if pathNode.Value != workspacePath {
		return fmt.Errorf("workspace already exists with path %q, refusing to overwrite with %q; edit the config file directly to change the path", pathNode.Value, workspacePath)
	}
	return nil
}

func mappingNode(content ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: content}
}

func scalarNode(value string, style yaml.Style) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: style}
}

func yamlStringStyle(value string) yaml.Style {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, ":#{}[],&*?|<>=!%@`\"'\n\t") || strings.Contains(value, " ") || value == "-" {
		return yaml.DoubleQuotedStyle
	}
	if strings.HasPrefix(value, "- ") {
		return yaml.DoubleQuotedStyle
	}
	lower := strings.ToLower(value)
	switch lower {
	case "null", "true", "false", "yes", "no", "on", "off", "~":
		return yaml.DoubleQuotedStyle
	}
	return 0
}
