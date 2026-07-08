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

import "sort"

// Plugin is a repack capability/scenario (gang awareness, node domain, hypernode
// domain, PDB, ...). On OnSessionOpen it registers callbacks into the Session on
// one of the five extension dimensions; actions and the core consume the
// aggregated result. Mirrors a scheduler plugin. The dimensions are:
//
//   - AddMovableFn         — may a task move? (veto: gang breach, PDB, minRunDuration, scope)
//   - AddPredicateFn       — may a task land on a node? (extra fit: DRA, topology)
//   - AddDomainFn          — what is a freeable unit? (node, hypernode/topology level)
//   - AddDisruptionScoreFn — soft cost of a plan on one dimension (ranking only)
//   - AddConstraintFn      — hard admissibility gate on a finished plan (veto: maxDisruptionScore)
//
// The search objective (consolidation vs relief-driven) and move-unit bundling
// (disruptionPolicy.bundlePolicy) are NOT plugin dimensions — they are Core
// concerns: the selected Core reads the run's goal and shapes its own units.
type Plugin interface {
	Name() string
	OnSessionOpen(ssn *Session)
	OnSessionClose(ssn *Session)
}

// pluginRegistry keys plugin factories by config name (last write wins, so
// tests/extensions can override a built-in).
var pluginRegistry = map[string]func() Plugin{}

// RegisterPlugin registers a plugin factory under a config name.
func RegisterPlugin(name string, factory func() Plugin) { pluginRegistry[name] = factory }

// GetPlugin returns a fresh plugin for the config name, ok=false if unknown.
func GetPlugin(name string) (Plugin, bool) {
	f, ok := pluginRegistry[name]
	if !ok {
		return nil, false
	}
	return f(), true
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
