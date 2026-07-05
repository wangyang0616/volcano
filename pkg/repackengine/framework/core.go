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

	"volcano.sh/volcano/pkg/repackengine/api"
)

// Built-in core names (config repack.core).
const (
	CoreDrain         = "drain"         // P0: node-anchored greedy (algorithm A)
	CoreConcentration = "concentration" // future: Σused² hill-climb (algorithm B)
)

// Core is the single-select search strategy: given a Session (snapshot + the
// callbacks plugins registered), produce a RepackPlan. EXACTLY ONE core runs per
// pass (drain XOR concentration), unlike plugins (many, composable) and actions
// (ordered pipeline). ok=false means NoRepackNeeded.
type Core interface {
	Name() string
	Plan(ssn *Session) (*api.RepackPlan, bool)
}

var coreRegistry = map[string]func() Core{}

// RegisterCore registers a core factory under a config name.
func RegisterCore(name string, factory func() Core) { coreRegistry[name] = factory }

// GetCore returns a fresh core for the config name, ok=false if unknown.
func GetCore(name string) (Core, bool) {
	f, ok := coreRegistry[name]
	if !ok {
		return nil, false
	}
	return f(), true
}

// CoreNames lists registered core names, sorted.
func CoreNames() []string {
	out := make([]string, 0, len(coreRegistry))
	for n := range coreRegistry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
