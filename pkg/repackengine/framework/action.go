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
	"sort"

	"k8s.io/klog/v2"
)

// Built-in action names (config repack.actions).
const ActionRepack = "repack"

// Action is one ordered planning stage of a repack pass (mirrors
// framework.Action in the scheduler). Side effects are committed by the Engine
// runtime only after the resulting plan has been durably persisted.
type Action interface {
	Name() string
	Execute(ssn *Session)
}

// ActionRegistration declares the implementation and semantic plugin
// capabilities required to execute an Action meaningfully.
type ActionRegistration struct {
	Factory  func() Action
	Requires []PluginCapability
}

var actionRegistry = map[string]ActionRegistration{}

// RegisterAction registers an action factory and its capability requirements
// under a config name.
func RegisterAction(name string, registration ActionRegistration) {
	registration.Requires = append([]PluginCapability(nil), registration.Requires...)
	actionRegistry[name] = registration
}

// GetAction returns a fresh action for the config name, ok=false if unknown.
func GetAction(name string) (Action, bool) {
	registration, ok := actionRegistry[name]
	if !ok || registration.Factory == nil {
		return nil, false
	}
	return registration.Factory(), true
}

// ActionRequires returns a copy of the plugin capabilities required by an
// action. Unknown actions return nil and are rejected separately by validation.
func ActionRequires(name string) []PluginCapability {
	return append([]PluginCapability(nil), actionRegistry[name].Requires...)
}

// ActionNames lists registered action names, sorted.
func ActionNames() []string {
	out := make([]string, 0, len(actionRegistry))
	for n := range actionRegistry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DefaultActions is the default pipeline when config names none.
func DefaultActions() []string { return []string{ActionRepack} }

// RunActions executes the named planning actions in order against the session.
// Unknown names are skipped with a warning (startup validation is responsible
// for rejecting invalid production configuration).
func RunActions(names []string, ssn *Session) {
	if len(names) == 0 {
		names = DefaultActions()
	}
	for _, name := range names {
		if ssn == nil || ssn.Context().Err() != nil {
			return
		}
		a, ok := GetAction(name)
		if !ok {
			klog.ErrorS(nil, "repack: unknown action in config, skipping", "action", name, "registered", ActionNames())
			continue
		}
		a.Execute(ssn)
	}
}
