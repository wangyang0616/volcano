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
// framework.Action in the scheduler). Side effects are committed by the engine
// driver only after the resulting plan has been durably persisted.
type Action interface {
	Name() string
	Execute(ssn *Session)
}

var actionRegistry = map[string]func() Action{}

// RegisterAction registers an action factory under a config name.
func RegisterAction(name string, factory func() Action) { actionRegistry[name] = factory }

// GetAction returns a fresh action for the config name, ok=false if unknown.
func GetAction(name string) (Action, bool) {
	f, ok := actionRegistry[name]
	if !ok {
		return nil, false
	}
	return f(), true
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
		a, ok := GetAction(name)
		if !ok {
			klog.ErrorS(nil, "repack: unknown action in config, skipping", "action", name, "registered", ActionNames())
			continue
		}
		a.Execute(ssn)
	}
}
