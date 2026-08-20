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
	"math"
	"reflect"
	"testing"
)

type argumentSpyPlugin struct{}

func (*argumentSpyPlugin) Name() string            { return "argument-spy" }
func (*argumentSpyPlugin) OnSessionOpen(*Session)  {}
func (*argumentSpyPlugin) OnSessionClose(*Session) {}

type orderingSpyPlugin struct{ name string }

func (p *orderingSpyPlugin) Name() string          { return p.name }
func (*orderingSpyPlugin) OnSessionOpen(*Session)  {}
func (*orderingSpyPlugin) OnSessionClose(*Session) {}

func TestOpenSessionPassesPluginArguments(t *testing.T) {
	const name = "test-argument-spy"
	var received Arguments
	RegisterPlugin(name, PluginRegistration{
		Factory: func(arguments Arguments) Plugin {
			received = arguments
			return &argumentSpyPlugin{}
		},
	})
	t.Cleanup(func() { delete(pluginRegistry, name) })

	ssn := OpenSession(SessionConfig{}, []PluginOption{{
		Name:      name,
		Arguments: Arguments{"weight": 2},
	}})
	CloseSession(ssn)

	got, err := received.NonNegativeFloat64("weight", 0)
	if err != nil || got != 2 {
		t.Fatalf("plugin weight=%v, want 2", got)
	}
}

func TestOpenSessionCanonicalizesPluginOrder(t *testing.T) {
	const first, second = "test-order-a", "test-order-z"
	for _, name := range []string{first, second} {
		pluginName := name
		RegisterPlugin(pluginName, PluginRegistration{
			Factory: func(Arguments) Plugin { return &orderingSpyPlugin{name: pluginName} },
		})
		t.Cleanup(func() { delete(pluginRegistry, pluginName) })
	}

	configured := []PluginOption{{Name: second}, {Name: first}}
	ssn := OpenSession(SessionConfig{}, configured)
	defer CloseSession(ssn)

	got := make([]string, 0, len(ssn.plugins))
	for _, plugin := range ssn.plugins {
		got = append(got, plugin.Name())
	}
	if want := []string{first, second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("opened plugins=%v, want canonical order %v", got, want)
	}
	if configured[0].Name != second || configured[1].Name != first {
		t.Fatalf("OpenSession mutated caller configuration: %+v", configured)
	}
}

func TestNonNegativeFloat64AndKeyValidation(t *testing.T) {
	arguments := Arguments{"integer": 2, "zero": 0.0}
	if got, err := arguments.NonNegativeFloat64("integer", 1); err != nil || got != 2 {
		t.Fatalf("integer=%v err=%v, want 2", got, err)
	}
	if got, err := arguments.NonNegativeFloat64("zero", 1); err != nil || got != 0 {
		t.Fatalf("zero=%v err=%v, want explicit zero", got, err)
	}
	if got, err := arguments.NonNegativeFloat64("omitted", 0.3); err != nil || got != 0.3 {
		t.Fatalf("omitted=%v err=%v, want default 0.3", got, err)
	}
	for name, value := range map[string]interface{}{
		"negative": -0.1,
		"nan":      math.NaN(),
		"infinite": math.Inf(1),
		"string":   "1.0",
	} {
		if _, err := (Arguments{"weight": value}).NonNegativeFloat64("weight", 1); err == nil {
			t.Errorf("%s value %v should be rejected", name, value)
		}
	}
	if err := (Arguments{"movedPodWeight": 1}).ValidateKeys("movedPodsWeight"); err == nil {
		t.Fatal("unknown/misspelled argument should be rejected")
	}
}
