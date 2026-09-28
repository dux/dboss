package config

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// DevSuffix marks the development variant of a key: `listen: ":80"` next to `listen_dev: ":3000"`
// keeps both values in one file, and the session picks one at load time.
const DevSuffix = "_dev"

// TauriSuffix marks the desktop build variant of a key, read only by `dboss build tauri`:
// `js_tauri: bun run build` is the one-shot frontend build that never runs on the box.
const TauriSuffix = "_tauri"

// profileSuffixes are every variant suffix a document may carry. Loading picks at most one of
// them; the pairs carrying any other are dropped.
var profileSuffixes = []string{DevSuffix, TauriSuffix}

// Dev reports whether this is a development session: the config dboss was pointed at is an app
// (it has procfile:), so one app is being run from its own folder rather than a host serving an
// apps directory. It is the one check every dev-only behavior reads.
func (c Config) Dev() bool { return c.App != nil }

// applyProfile resolves the variant suffixes across a whole document, before the schema check turns
// it into structs. Every `<key><active>` replaces `<key>` and creates it when it is absent; every
// other variant is dropped, so an app file carries its dev and desktop values into a host unread.
// active is "" for the box, where every variant is dropped. The value replaces the base outright,
// so a block override names the leaf key it changes rather than restating the block. It reports
// whether the tree changed.
func applyProfile(node *yaml.Node, active string) bool {
	changed := false
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			changed = applyProfile(child, active) || changed
		}
	case yaml.MappingNode:
		for _, child := range node.Content {
			changed = applyProfile(child, active) || changed
		}
		changed = resolveProfile(node, active) || changed
	}
	return changed
}

// resolveProfile applies the suffixes to one mapping node and removes every pair that still carries one.
func resolveProfile(node *yaml.Node, active string) bool {
	overrides := []int{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if variantBase(node.Content[i].Value, active) != "" {
			overrides = append(overrides, i)
		}
	}
	if active != "" && len(overrides) > 0 {
		at := map[string]int{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			at[node.Content[i].Value] = i
		}
		for _, i := range overrides {
			base := variantBase(node.Content[i].Value, active)
			if j, ok := at[base]; ok {
				node.Content[j+1] = node.Content[i+1]
				continue
			}
			// No base key to override: rename the pair in place so it keeps its position.
			node.Content[i].Value = base
			at[base] = i
		}
	}
	changed := len(overrides) > 0
	kept := node.Content[:0]
	for i := 0; i+1 < len(node.Content); i += 2 {
		if isVariantKey(node.Content[i].Value) {
			changed = true
			continue
		}
		kept = append(kept, node.Content[i], node.Content[i+1])
	}
	node.Content = kept
	return changed
}

// variantBase is name without the active suffix, or "" when name does not carry it.
func variantBase(name, active string) string {
	if active == "" || len(name) <= len(active) || !strings.HasSuffix(name, active) {
		return ""
	}
	return strings.TrimSuffix(name, active)
}

// isVariantKey reports whether name carries any profile suffix.
func isVariantKey(name string) bool {
	return variantOf(name) != ""
}

// variantOf is name without its profile suffix, or "" when it carries none.
func variantOf(name string) string {
	for _, suffix := range profileSuffixes {
		if base := variantBase(name, suffix); base != "" {
			return base
		}
	}
	return ""
}

// hasTopKey reports whether the document's root mapping has name, so dev mode can be read off the
// tree before it is decoded.
func hasTopKey(root *yaml.Node, name string) bool {
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return false
	}
	content := root.Content[0].Content
	for i := 0; i+1 < len(content); i += 2 {
		if content[i].Value == name {
			return true
		}
	}
	return false
}
