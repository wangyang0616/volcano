/*
Copyright 2026 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package framework

import (
	"fmt"
	"math"
	"sort"
)

// Arguments carries one plugin's configuration, matching the scheduler's
// name-plus-arguments extension model.
type Arguments map[string]interface{}

// NonNegativeFloat64 returns a finite, non-negative numeric argument or the
// supplied default when omitted. YAML may decode a whole number as int, so both
// integer and floating-point forms are accepted. Zero intentionally disables a
// weighted score term.
func (a Arguments) NonNegativeFloat64(key string, defaultValue float64) (float64, error) {
	value, ok := a[key]
	if !ok {
		return defaultValue, nil
	}
	var parsed float64
	switch typed := value.(type) {
	case float64:
		parsed = typed
	case float32:
		parsed = float64(typed)
	case int:
		parsed = float64(typed)
	case int64:
		parsed = float64(typed)
	case int32:
		parsed = float64(typed)
	case uint:
		parsed = float64(typed)
	case uint64:
		parsed = float64(typed)
	case uint32:
		parsed = float64(typed)
	default:
		return 0, fmt.Errorf("argument %q must be a number, got %T", key, value)
	}
	if parsed < 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("argument %q must be finite and non-negative, got %v", key, parsed)
	}
	return parsed, nil
}

// ValidateKeys rejects misspelled or unsupported arguments deterministically.
func (a Arguments) ValidateKeys(allowed ...string) error {
	allowedSet := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = true
	}
	var unknown []string
	for key := range a {
		if !allowedSet[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("unsupported plugin arguments: %v", unknown)
}

// PluginOption selects one plugin and supplies its arguments. Plugin options
// are order-independent; OpenSession canonicalizes them by name before plugins
// register callbacks.
type PluginOption struct {
	Name      string    `yaml:"name"`
	Arguments Arguments `yaml:"arguments,omitempty"`
}

// PluginOptions builds argument-free options, primarily for command-line
// overrides and tests. Rich YAML configuration can populate Arguments directly.
func PluginOptions(names ...string) []PluginOption {
	options := make([]PluginOption, 0, len(names))
	for _, name := range names {
		options = append(options, PluginOption{Name: name})
	}
	return options
}

// Plugin is a repack capability/scenario (gang disruption, node consolidation, hypernode
// domain, PDB, ...). On OnSessionOpen it registers callbacks into the Session on
// one or more extension dimensions; actions and the planner consume the
// aggregated result. Mirrors a scheduler plugin. The dimensions are:
//
//   - AddMovableFn         — may a task move? (veto: gang breach, PDB, minRunDuration, scope)
//   - AddDomainFn          — what is a freeable unit? (node, hypernode/topology level)
//   - AddDisruptionScoreFn — soft cost of a plan on one dimension (ranking only)
//   - AddConstraintFn      — hard admissibility gate on a finished plan (veto: maxDisruptionScore)
//   - AddCandidateFilterFn — cheap pre-score candidate veto (repack budget, capacity, PDB)
//   - AddReceiverPoolFn    — snapshot-stable receiver-universe policy
//   - AddVictimOrderFn     — victim simulation order
//   - AddReceiverRankFn    — lexicographic receiver preference
//
// Search mechanics remain action/planner concerns; scenario semantics belong in
// plugins and compose through these callbacks.
type Plugin interface {
	Name() string
	OnSessionOpen(ssn *Session)
	OnSessionClose(ssn *Session)
}

// PluginCapability is a semantic ability consumed by an Action. Actions depend
// on capabilities instead of concrete plugin names, so one domain plugin can be
// replaced by another without changing the main pipeline.
type PluginCapability string

const (
	// CapabilityDomain means the plugin contributes at least one kind of
	// FreeableUnit (node, HyperNode, or another future consolidation domain).
	CapabilityDomain PluginCapability = "domain"
)

// PluginRegistration is the immutable metadata stored for a plugin name.
// Provides/Requires express capability-level composition and deliberately avoid
// coupling plugins to one another by name.
type PluginRegistration struct {
	Factory   func(Arguments) Plugin
	Validator func(Arguments) error
	Provides  []PluginCapability
	Requires  []PluginCapability
}

// pluginRegistry keys plugin factories by config name (last write wins, so
// tests/extensions can override a built-in).
var pluginRegistry = map[string]PluginRegistration{}

// RegisterPlugin registers a plugin factory, validation, and capability
// metadata under a config name.
func RegisterPlugin(name string, registration PluginRegistration) {
	registration.Provides = append([]PluginCapability(nil), registration.Provides...)
	registration.Requires = append([]PluginCapability(nil), registration.Requires...)
	pluginRegistry[name] = registration
}

// GetPlugin returns a fresh plugin for the config name, ok=false if unknown.
func GetPlugin(name string, arguments Arguments) (Plugin, bool) {
	registration, ok := pluginRegistry[name]
	if !ok {
		return nil, false
	}
	if registration.Factory == nil {
		return nil, false
	}
	return registration.Factory(arguments), true
}

// HasPlugin reports whether a plugin name is registered without constructing
// it. Engine validation uses this so plugin builders run exactly once per
// Session, even when a RepackRun is rejected for configuration errors.
func HasPlugin(name string) bool {
	registration, ok := pluginRegistry[name]
	return ok && registration.Factory != nil
}

// ValidatePluginArguments validates one registered plugin's arguments. Plugins
// without a validator accept an empty or plugin-defined argument map.
func ValidatePluginArguments(name string, arguments Arguments) error {
	registration, ok := pluginRegistry[name]
	if !ok {
		return fmt.Errorf("unknown repack plugin %q", name)
	}
	if registration.Validator == nil {
		return nil
	}
	return registration.Validator(arguments)
}

// PluginProvides returns a copy of the capabilities provided by a registered
// plugin. Unknown names return nil and are rejected separately by validation.
func PluginProvides(name string) []PluginCapability {
	return append([]PluginCapability(nil), pluginRegistry[name].Provides...)
}

// PluginRequires returns a copy of the capabilities required by a registered
// plugin.
func PluginRequires(name string) []PluginCapability {
	return append([]PluginCapability(nil), pluginRegistry[name].Requires...)
}

// PluginNames lists registered plugin names, sorted.
func PluginNames() []string {
	out := make([]string, 0, len(pluginRegistry))
	for n := range pluginRegistry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
