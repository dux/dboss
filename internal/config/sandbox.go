package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Sandbox confines every command dboss runs for the app (procfile, cron, hooks, lifecycle steps,
// dboss exec) on Linux 6.12+: it reads its own folder and the system, writes ./tmp, ./log and a
// private /tmp, binds only its PORT and connects to no TCP port. Read, Write, Sockets, Bind and
// Connect allow more; Deny hides paths even inside the app folder. Paths are relative to the
// app folder. `sandbox: true` turns it on with nothing extra allowed, and writing the mapping
// turns it on unless it says enabled: false.
type Sandbox struct {
	Enabled bool  `yaml:"enabled" json:"enabled"`
	Read    List  `yaml:"read" json:"read"`
	Write   List  `yaml:"write" json:"write"`
	Deny    List  `yaml:"deny" json:"deny"`
	Sockets List  `yaml:"sockets" json:"sockets"`
	Bind    Ports `yaml:"bind" json:"bind"`
	Connect Ports `yaml:"connect" json:"connect"`
}

// sandboxFields is Sandbox without its decoder, so the mapping decodes through the struct tags.
type sandboxFields Sandbox

// UnmarshalYAML accepts true, false or the mapping. Decoding onto the current value keeps the
// keys the mapping leaves out, so an app's block overrides the host defaults key by key.
func (s *Sandbox) UnmarshalYAML(node *yaml.Node) error {
	enabled, mapping, err := sandboxNode(node)
	if err != nil || !mapping {
		s.Enabled = enabled
		return err
	}
	s.Enabled = true
	return node.Decode((*sandboxFields)(s))
}

// SandboxOverrides is the sandbox block as pointers; a list replaces the inherited one.
type SandboxOverrides struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Read    List  `yaml:"read,omitempty" json:"read,omitempty"`
	Write   List  `yaml:"write,omitempty" json:"write,omitempty"`
	Deny    List  `yaml:"deny,omitempty" json:"deny,omitempty"`
	Sockets List  `yaml:"sockets,omitempty" json:"sockets,omitempty"`
	Bind    Ports `yaml:"bind,omitempty" json:"bind,omitempty"`
	Connect Ports `yaml:"connect,omitempty" json:"connect,omitempty"`
}

type sandboxOverrideFields SandboxOverrides

// UnmarshalYAML accepts true, false or the mapping, like Sandbox.
func (s *SandboxOverrides) UnmarshalYAML(node *yaml.Node) error {
	enabled, mapping, err := sandboxNode(node)
	if err != nil {
		return err
	}
	if !mapping {
		s.Enabled = &enabled
		return nil
	}
	if err := node.Decode((*sandboxOverrideFields)(s)); err != nil {
		return err
	}
	if s.Enabled == nil {
		s.Enabled = &enabled
	}
	return nil
}

// sandboxKeys are the keys of the mapping form, checked here because a type with its own decoder
// is a leaf to the schema walk.
var sandboxKeys = []string{"enabled", "read", "write", "deny", "sockets", "bind", "connect"}

// sandboxNode reads the shorthand: a boolean is the switch itself, a mapping means on unless its
// enabled key says otherwise.
func sandboxNode(node *yaml.Node) (enabled, mapping bool, err error) {
	switch {
	case node.Kind == yaml.ScalarNode && node.Tag == "!!bool":
		return node.Value == "true", false, nil
	case node.Kind == yaml.ScalarNode && node.Tag == "!!null":
		return false, false, nil
	case node.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			known := false
			for _, name := range sandboxKeys {
				known = known || key.Value == name
			}
			if !known {
				return false, false, &Error{Line: key.Line, Key: "sandbox", Message: fmt.Sprintf("unknown key %q", key.Value), Hint: "valid keys here: " + strings.Join(sandboxKeys, ", ")}
			}
		}
		return true, true, nil
	}
	return false, false, &Error{Line: node.Line, Key: "sandbox", Message: "must be true, false or a mapping of sandbox settings"}
}

// Ports is a list of TCP ports that also accepts a single port, like List.
type Ports []int

func (p *Ports) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			*p = nil
			return nil
		}
		var port int
		if err := node.Decode(&port); err != nil {
			return &Error{Line: node.Line, Message: "must be a port or a list of ports"}
		}
		*p = Ports{port}
		return nil
	case yaml.SequenceNode:
		var ports []int
		if err := node.Decode(&ports); err != nil {
			return &Error{Line: node.Line, Message: "must be a port or a list of ports"}
		}
		*p = Ports(ports)
		return nil
	}
	return &Error{Line: node.Line, Message: "must be a port or a list of ports"}
}

func validateSandbox(s Sandbox) error {
	for _, paths := range []struct {
		key  string
		list List
	}{{"sandbox.read", s.Read}, {"sandbox.write", s.Write}, {"sandbox.deny", s.Deny}, {"sandbox.sockets", s.Sockets}} {
		for _, path := range paths.list {
			if strings.TrimSpace(path) == "" || strings.ContainsRune(path, 0) {
				return keyErr(paths.key, "has an empty path")
			}
		}
	}
	for _, ports := range []struct {
		key  string
		list Ports
	}{{"sandbox.bind", s.Bind}, {"sandbox.connect", s.Connect}} {
		for _, port := range ports.list {
			if port < 1 || port > 65535 {
				return keyErr(ports.key, "port %d is outside 1-65535", port)
			}
		}
	}
	return nil
}
