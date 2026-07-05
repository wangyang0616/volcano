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

package api

import (
	"sort"

	"volcano.sh/volcano/pkg/scheduler/api"
)

// FreeableUnit is one "thing worth freeing" contributed by a domain plugin: a
// set of member nodes that must ALL be vacated for the unit to count as freed,
// and a benefit weight. A node-level domain yields one unit per node (weight ~1);
// a hypernode-level domain yields one unit per hypernode (higher weight, since a
// whole topology block is far more valuable to a large gang). With several domain
// plugins enabled the core optimizes the combined weighted benefit (their union),
// i.e. a holistic node+hypernode optimum rather than an either/or.
type FreeableUnit struct {
	Level  string   // "node" / "hypernode" (for reporting)
	Nodes  []string // member nodes; freeing the unit requires emptying all of them
	Weight float64  // benefit of freeing this unit
}

// RepackPlan is the outcome of one core search pass — the algorithm-agnostic plan
// every core (drain, concentration, ...) returns.
type RepackPlan struct {
	Moves      []*Move        // every gang relocation (from -> to)
	FreedNodes []string       // nodes fully vacated by this plan
	FreedUnits []FreeableUnit // freeable units fully realized (node and/or hypernode)
	Before     ResourceFrag   // fragmentation measured before the plan
	Cost       DisruptionCost // disruption summary of Moves
}

// NodesFreed is the realized node-level benefit (whole nodes emptied).
func (p *RepackPlan) NodesFreed() int { return len(p.FreedNodes) }

// Benefit is the realized weighted benefit across all freed units (the objective
// the core maximizes). Falls back to node count when no units are recorded
// (node-only P0), so it equals NodesFreed in that case.
func (p *RepackPlan) Benefit() float64 {
	if p == nil {
		return 0
	}
	if len(p.FreedUnits) == 0 {
		return float64(len(p.FreedNodes))
	}
	var b float64
	for _, u := range p.FreedUnits {
		b += u.Weight
	}
	return b
}

// FragRateDelta is the change in fragmentation rate: -nodesFreed/M (design §4.12).
func (p *RepackPlan) FragRateDelta() float64 {
	if p == nil || p.Before.M == 0 {
		return 0
	}
	return -float64(len(p.FreedNodes)) / float64(p.Before.M)
}

// AffectedPodGroups returns the distinct PodGroups touched by this plan, sorted —
// the authoritative "which gangs were disrupted" list for the report/audit.
func (p *RepackPlan) AffectedPodGroups() []api.JobID {
	if p == nil {
		return nil
	}
	set := map[api.JobID]bool{}
	for _, m := range p.Moves {
		if m != nil && m.Task != nil && m.To != m.From {
			set[m.Task.Job] = true
		}
	}
	out := make([]api.JobID, 0, len(set))
	for j := range set {
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
