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

package conf

import (
	"fmt"

	"volcano.sh/volcano/pkg/repackengine/framework"
)

func ValidatePluginOptions(options []framework.PluginOption) error {
	seen := make(map[string]bool, len(options))
	for _, option := range options {
		if option.Name == "" {
			return fmt.Errorf("repack plugin name must not be empty")
		}
		if seen[option.Name] {
			return fmt.Errorf("repack plugin %q is configured more than once", option.Name)
		}
		seen[option.Name] = true
		if !framework.HasPlugin(option.Name) {
			return fmt.Errorf("unknown repack plugin %q (registered: %v)", option.Name, framework.PluginNames())
		}
		if err := framework.ValidatePluginArguments(option.Name, option.Arguments); err != nil {
			return fmt.Errorf("invalid arguments for repack plugin %q: %w", option.Name, err)
		}
	}
	return nil
}

func ValidatePipeline(actions []string, plugins []framework.PluginOption) error {
	if len(actions) == 0 {
		actions = framework.DefaultActions()
	}
	if err := ValidatePluginOptions(plugins); err != nil {
		return err
	}
	enabledCapabilities := make(map[framework.PluginCapability]bool)
	for _, plugin := range plugins {
		for _, capability := range framework.PluginProvides(plugin.Name) {
			enabledCapabilities[capability] = true
		}
	}
	for _, plugin := range plugins {
		for _, required := range framework.PluginRequires(plugin.Name) {
			if !enabledCapabilities[required] {
				return fmt.Errorf("repack plugin %q requires capability %q, but no configured plugin provides it", plugin.Name, required)
			}
		}
	}
	for _, name := range actions {
		if _, ok := framework.GetAction(name); !ok {
			return fmt.Errorf("unknown repack action %q (registered: %v)", name, framework.ActionNames())
		}
		for _, required := range framework.ActionRequires(name) {
			if !enabledCapabilities[required] {
				return fmt.Errorf("repack action %q requires capability %q, but no configured plugin provides it", name, required)
			}
		}
	}
	return nil
}

// ValidateSession verifies that statically declared capabilities were actually
// registered by the opened plugins.
func ValidateSession(actions []string, plugins []framework.PluginOption, session *framework.Session) error {
	if len(actions) == 0 {
		actions = framework.DefaultActions()
	}
	for _, plugin := range plugins {
		for _, required := range framework.PluginRequires(plugin.Name) {
			if !session.ProvidesCapability(required) {
				return fmt.Errorf("repack plugin %q requires runtime capability %q, but no opened plugin registered its callback", plugin.Name, required)
			}
		}
	}
	for _, name := range actions {
		for _, required := range framework.ActionRequires(name) {
			if !session.ProvidesCapability(required) {
				return fmt.Errorf("repack action %q requires runtime capability %q, but no opened plugin registered its callback", name, required)
			}
		}
	}
	return nil
}
