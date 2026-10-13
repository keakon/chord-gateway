package config

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// UnsupportedConfigFields reports every YAML key the gateway does not define,
// with its line, so a typo or a removed option (for example the former
// event_visibility.activity) is visible in the log instead of silently doing
// nothing. Load shares its parsed document with this check.
func UnsupportedConfigFields(doc *yaml.Node) []string {
	if doc == nil || len(doc.Content) == 0 {
		return nil
	}
	var issues []string
	collectUnsupportedFields(doc.Content[0], reflect.TypeFor[Config](), "", &issues)
	return issues
}

func collectUnsupportedFields(node *yaml.Node, typ reflect.Type, path string, issues *[]string) {
	if node == nil {
		return
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		fields := yamlConfigFields(typ)
		if typ == reflect.TypeFor[Config]() {
			// Only these two fields have a custom wire shape. All field names
			// and value schemas come from the actual configuration types.
			fields["ims"] = reflect.TypeFor[IMAdapterConfig]()
			fields["workspaces"] = reflect.TypeFor[map[string]Workspace]()
		}
		// parseIMsNode matches adapter names after normalizing them (lowercase
		// and trimmed); go-yaml matches every other key exactly.
		normalizeKeys := typ == reflect.TypeFor[IMAdapterConfig]()
		walkYAMLMapping(node, make(map[string]bool), make(map[*yaml.Node]bool), func(key, value *yaml.Node) {
			fieldType, known := fields[key.Value]
			if !known && normalizeKeys {
				fieldType, known = fields[NormalizeIMType(key.Value)]
			}
			if !known {
				*issues = append(*issues, fmt.Sprintf("line %d: field %s is not supported", key.Line, joinConfigPath(path, key.Value)))
				return
			}
			collectUnsupportedFields(value, fieldType, joinConfigPath(path, key.Value), issues)
		})
	case reflect.Map:
		// Map keys are user data (workspace IDs, chat IDs, adapter names), so
		// only the values can carry unsupported fields.
		if node.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			collectUnsupportedFields(node.Content[i+1], typ.Elem(), joinConfigPath(path, node.Content[i].Value), issues)
		}
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			return
		}
		for _, item := range node.Content {
			collectUnsupportedFields(item, typ.Elem(), path, issues)
		}
	}
}

// walkYAMLMapping visits effective entries without changing the source nodes.
// Explicit keys override merges, and earlier maps in a merge sequence win.
// A shared visited set bounds aliases and recursive merge graphs; each target
// schema gets its own traversal so aliases are checked in their use context.
func walkYAMLMapping(node *yaml.Node, keys map[string]bool, visited map[*yaml.Node]bool, visit func(*yaml.Node, *yaml.Node)) {
	if node == nil || visited[node] {
		return
	}
	visited[node] = true
	switch node.Kind {
	case yaml.AliasNode:
		walkYAMLMapping(node.Alias, keys, visited, visit)
	case yaml.SequenceNode:
		for _, item := range node.Content {
			walkYAMLMapping(item, keys, visited, visit)
		}
	case yaml.MappingNode:
		var merge *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag == "!!merge" {
				merge = value
				continue
			}
			if !keys[key.Value] {
				keys[key.Value] = true
				visit(key, value)
			}
		}
		walkYAMLMapping(merge, keys, visited, visit)
	}
}

// yamlConfigFields maps each yaml key of a struct type to its field type, using
// the same default (lowercased field name) go-yaml applies without a tag.
func yamlConfigFields(typ reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		if name == "-" {
			continue
		}
		fields[name] = field.Type
	}
	return fields
}

func joinConfigPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
