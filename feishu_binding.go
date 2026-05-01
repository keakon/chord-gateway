package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/keakon/chord-gateway/config"
)

func currentFeishuBinding(cfg *config.Config, chatID string) string {
	if cfg == nil {
		return ""
	}
	imCfg := cfg.IMConfigByType("feishu")
	if imCfg == nil || imCfg.Feishu == nil {
		return ""
	}
	return strings.TrimSpace(imCfg.Feishu.ChatBindings[chatID])
}

func workspaceByID(cfg *config.Config, workspaceID string) *config.Workspace {
	if cfg == nil {
		return nil
	}
	return cfg.WorkspaceByID(workspaceID)
}

func upsertFeishuBindingConfigFile(configFile, chatID, workspaceID, workspacePath string) (*config.Config, error) {
	chatID = strings.TrimSpace(chatID)
	workspaceID = strings.TrimSpace(workspaceID)
	workspacePath = strings.TrimSpace(workspacePath)
	if chatID == "" {
		return nil, fmt.Errorf("chat_id is required")
	}
	if workspaceID == "" {
		return nil, fmt.Errorf("workspace_id is required")
	}
	if workspacePath == "" {
		return nil, fmt.Errorf("workspace path is required")
	}
	if !config.IsAllowedWorkspacePath(workspacePath) {
		return nil, fmt.Errorf("workspace path %q must start with /, ~, a Windows drive prefix, or a UNC prefix", workspacePath)
	}
	expandedWorkspacePath := config.Expand(workspacePath)
	info, err := os.Stat(expandedWorkspacePath)
	if err != nil {
		return nil, fmt.Errorf("workspace path %q is not accessible: %w", workspacePath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace path %q is not a directory", workspacePath)
	}

	original, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	updated, err := renderUpdatedFeishuBindingConfig(original, chatID, workspaceID, workspacePath)
	if err != nil {
		return nil, err
	}
	updatedCfg, err := parseConfigBytes(updated)
	if err != nil {
		return nil, err
	}
	if err := updatedCfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	if !bytes.Equal(original, updated) {
		if err := writeFileAtomically(configFile, updated, 0o644); err != nil {
			return nil, err
		}
	}
	return updatedCfg, nil
}

func renderUpdatedFeishuBindingConfig(data []byte, chatID, workspaceID, workspacePath string) ([]byte, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0] == nil || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse config: expected top-level mapping")
	}
	root := doc.Content[0]

	workspacesNode, err := ensureWorkspacesMappingNode(root)
	if err != nil {
		return nil, err
	}
	feishuNode, err := ensureFeishuConfigNode(root)
	if err != nil {
		return nil, err
	}

	if err := upsertWorkspaceNode(workspacesNode, workspaceID, workspacePath); err != nil {
		return nil, err
	}
	if err := upsertChatBindingNode(feishuNode, chatID, workspaceID); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return buf.Bytes(), nil
}

func parseConfigBytes(data []byte) (*config.Config, error) {
	var cfg config.Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.ExpandPath(config.Expand)
	return &cfg, nil
}

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

func ensureFeishuConfigNode(root *yaml.Node) (*yaml.Node, error) {
	imsNode, _ := ensureIMsMappingNode(root)
	feishuNode := mappingValue(imsNode, "feishu")
	if feishuNode == nil {
		feishuNode = mappingNode()
		imsNode.Content = append(imsNode.Content, scalarNode("feishu", 0), feishuNode)
	}
	if feishuNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse config: ims.feishu must be a YAML mapping")
	}
	return feishuNode, nil
}

func ensureIMsMappingNode(root *yaml.Node) (*yaml.Node, error) {
	imsNode := mappingValue(root, "ims")
	if imsNode == nil {
		imsNode = mappingNode()
		root.Content = append(root.Content, scalarNode("ims", 0), imsNode)
		return imsNode, nil
	}
	if imsNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse config: ims must be a YAML mapping")
	}
	return imsNode, nil
}

func ensureWorkspacesMappingNode(root *yaml.Node) (*yaml.Node, error) {
	workspacesNode := mappingValue(root, "workspaces")
	if workspacesNode == nil {
		workspacesNode = mappingNode()
		root.Content = append(root.Content, scalarNode("workspaces", 0), workspacesNode)
		return workspacesNode, nil
	}
	if workspacesNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse config: workspaces must be a YAML mapping")
	}
	return workspacesNode, nil
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

func upsertChatBindingNode(feishuNode *yaml.Node, chatID, workspaceID string) error {
	chatBindingsNode := mappingValue(feishuNode, "chat_bindings")
	if chatBindingsNode == nil {
		chatBindingsNode = mappingNode(
			scalarNode(chatID, yamlStringStyle(chatID)),
			scalarNode(workspaceID, yamlStringStyle(workspaceID)),
		)
		feishuNode.Content = append(feishuNode.Content,
			scalarNode("chat_bindings", 0),
			chatBindingsNode,
		)
		return nil
	}
	if chatBindingsNode.Kind != yaml.MappingNode {
		return fmt.Errorf("parse config: ims.feishu.chat_bindings must be a YAML mapping")
	}
	chatBindingsNode.Style = 0
	for i := 0; i+1 < len(chatBindingsNode.Content); i += 2 {
		keyNode := chatBindingsNode.Content[i]
		valueNode := chatBindingsNode.Content[i+1]
		if keyNode == nil || valueNode == nil || keyNode.Value != chatID {
			continue
		}
		valueNode.Value = workspaceID
		valueNode.Style = yamlStringStyle(workspaceID)
		valueNode.Tag = "!!str"
		return nil
	}
	chatBindingsNode.Content = append(chatBindingsNode.Content,
		scalarNode(chatID, yamlStringStyle(chatID)),
		scalarNode(workspaceID, yamlStringStyle(workspaceID)),
	)
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
