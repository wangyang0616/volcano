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

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// PlanningCandidate is the stable, read-only candidate view exposed to plugins.
// The planner owns all mutable bookkeeping; plugins only decide whether and how
// this prospective plan should be considered.
type PlanningCandidate struct {
	Unit api.FreeableUnit
	Plan *api.CandidatePlan
}

// CandidateFilterResult rejects a candidate before disruption scoring. Filters
// must be monotonic for one planning pass because a rejected unit is not revisited
// after more moves are committed. A nil result means pass. Reason is a bounded
// machine-readable value used by metrics; Message is optional operator-facing
// detail. MarkInfeasible additionally marks the unit's nodes as proven unable to
// drain, which makes them preferred receivers.
type CandidateFilterResult struct {
	Reason         string
	Message        string
	MarkInfeasible bool
}

// ReceiverCandidate is the planner-computed, policy-neutral receiver view. It
// contains facts that would otherwise force plugins to duplicate the planner's
// incremental accounting.
type ReceiverCandidate struct {
	Node              *schedapi.NodeInfo
	StaysOccupied     bool
	AvailableResource int64
}

type (
	CandidateFilterFn func(ctx *api.PlanContext, candidate *PlanningCandidate) *CandidateFilterResult
	// ReceiverPoolFn applies a snapshot-stable receiver-universe policy once per
	// planning pass. Pool functions are chained in canonical plugin-name order.
	ReceiverPoolFn func(ctx *api.PlanContext, nodes []*schedapi.NodeInfo) []*schedapi.NodeInfo
	// VictimOrderFn returns a negative value when left should be simulated first,
	// positive when right should be first, and zero to defer to the next plugin.
	VictimOrderFn func(left, right *schedapi.TaskInfo) int
	// ReceiverRankFn returns a fixed-width lexicographic rank. Larger values are
	// preferred. Terms run by phase, then canonical plugin-name order. Five
	// values cover the built-in Gang rank without allocating in large receiver
	// sets; a plugin needing more dimensions can register another term.
	ReceiverRankFn func(ctx *api.PlanContext, candidate *PlanningCandidate, receiver *ReceiverCandidate) ReceiverRank
)

// ReceiverRank is one plugin's allocation-free lexicographic rank vector.
type ReceiverRank [5]int64

// ReceiverRankPhase defines the stable, framework-owned receiver decision
// sequence. Plugins select a semantic phase instead of coordinating through
// private numeric priorities.
type ReceiverRankPhase int

const (
	// ReceiverRankPhaseStability prefers receivers that cannot be released by
	// this pass, avoiding the loss of another viable drain target.
	ReceiverRankPhaseStability ReceiverRankPhase = iota
	// ReceiverRankPhaseDisruption minimizes the future workload disruption caused
	// by consuming a receiver.
	ReceiverRankPhaseDisruption
	// ReceiverRankPhasePacking applies final resource-packing preferences.
	ReceiverRankPhasePacking
)

type namedCandidateFilter struct {
	name string
	fn   CandidateFilterFn
}

type namedVictimOrder struct {
	name string
	fn   VictimOrderFn
}

type namedReceiverRank struct {
	name  string
	phase ReceiverRankPhase
	order int
	fn    ReceiverRankFn
}

// ReceiverRankTerm is retained with an ordered receiver so logs can explain the
// plugin decisions without re-running policy functions.
type ReceiverRankTerm struct {
	Name   string
	Values ReceiverRank
}

// RankedReceiver is one receiver plus all lexicographic plugin ranks.
type RankedReceiver struct {
	Receiver *ReceiverCandidate
	Terms    []ReceiverRankTerm
}

func (s *Session) AddCandidateFilterFn(name string, fn CandidateFilterFn) {
	if fn != nil {
		s.candidateFilterFns = append(s.candidateFilterFns, namedCandidateFilter{name: name, fn: fn})
	}
}

func (s *Session) AddReceiverPoolFn(fn ReceiverPoolFn) {
	if fn != nil {
		s.receiverPoolFns = append(s.receiverPoolFns, fn)
	}
}

func (s *Session) AddVictimOrderFn(name string, fn VictimOrderFn) {
	if fn != nil {
		s.victimOrderFns = append(s.victimOrderFns, namedVictimOrder{name: name, fn: fn})
	}
}

func (s *Session) AddReceiverRankFn(name string, phase ReceiverRankPhase, fn ReceiverRankFn) {
	if fn == nil {
		return
	}
	s.receiverRankFns = append(s.receiverRankFns, namedReceiverRank{
		name: name, phase: phase, order: len(s.receiverRankFns), fn: fn,
	})
	sort.SliceStable(s.receiverRankFns, func(i, j int) bool {
		if s.receiverRankFns[i].phase != s.receiverRankFns[j].phase {
			return s.receiverRankFns[i].phase < s.receiverRankFns[j].phase
		}
		return s.receiverRankFns[i].order < s.receiverRankFns[j].order
	})
}

// CandidateAdmissible applies every cheap plugin veto before candidate scoring.
func (s *Session) CandidateAdmissible(candidate *PlanningCandidate) *CandidateFilterResult {
	ctx := s.PlanContext()
	for _, filter := range s.candidateFilterFns {
		if result := filter.fn(ctx, candidate); result != nil {
			return result
		}
	}
	return nil
}

// ReceiverPool applies snapshot-stable receiver policies once. A ReceiverPoolFn
// is a filter-only extension: each plugin may retain nodes from the current pool,
// but cannot reintroduce a node removed by an earlier plugin, add a foreign node,
// or create duplicate capacity. The framework preserves the current pool's order;
// receiver ordering belongs to ReceiverRankFn.
func (s *Session) ReceiverPool(nodes []*schedapi.NodeInfo) []*schedapi.NodeInfo {
	pool := intersectReceiverPool(nodes, nodes)
	ctx := s.PlanContext()
	for _, fn := range s.receiverPoolFns {
		selected := fn(ctx, append([]*schedapi.NodeInfo(nil), pool...))
		pool = intersectReceiverPool(pool, selected)
	}
	return pool
}

// intersectReceiverPool returns the unique nodes from current that are named in
// selected, retaining current's order and NodeInfo instances. Node names are the
// scheduler's stable identity within a snapshot.
func intersectReceiverPool(current, selected []*schedapi.NodeInfo) []*schedapi.NodeInfo {
	selectedNames := make(map[string]bool, len(selected))
	for _, node := range selected {
		if node != nil && node.Name != "" {
			selectedNames[node.Name] = true
		}
	}
	retained := make([]*schedapi.NodeInfo, 0, min(len(current), len(selectedNames)))
	seen := make(map[string]bool, len(current))
	for _, node := range current {
		if node == nil || node.Name == "" || seen[node.Name] || !selectedNames[node.Name] {
			continue
		}
		seen[node.Name] = true
		retained = append(retained, node)
	}
	return retained
}

// OrderVictims applies configured lexicographic comparators and preserves the
// original order when every plugin abstains.
func (s *Session) OrderVictims(victims []*schedapi.TaskInfo) []*schedapi.TaskInfo {
	ordered := append([]*schedapi.TaskInfo(nil), victims...)
	sort.SliceStable(ordered, func(i, j int) bool {
		for _, term := range s.victimOrderFns {
			if comparison := term.fn(ordered[i], ordered[j]); comparison != 0 {
				return comparison < 0
			}
		}
		return false
	})
	return ordered
}

// OrderReceivers evaluates every receiver rank once, then performs a stable
// lexicographic sort. This keeps expensive Gang calculations O(receivers), not
// O(receivers*log(receivers)).
func (s *Session) OrderReceivers(candidate *PlanningCandidate, receivers []*ReceiverCandidate) []RankedReceiver {
	ctx := s.PlanContext()
	ranked := make([]RankedReceiver, 0, len(receivers))
	// All terms share one backing array. This keeps ranking O(receivers) policy
	// evaluations with O(1) allocations instead of allocating per receiver/term.
	allTerms := make([]ReceiverRankTerm, len(receivers)*len(s.receiverRankFns))
	for receiverIndex, receiver := range receivers {
		terms := allTerms[receiverIndex*len(s.receiverRankFns) : (receiverIndex+1)*len(s.receiverRankFns)]
		for rankIndex, rankFn := range s.receiverRankFns {
			terms[rankIndex] = ReceiverRankTerm{Name: rankFn.name, Values: rankFn.fn(ctx, candidate, receiver)}
		}
		ranked = append(ranked, RankedReceiver{Receiver: receiver, Terms: terms})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return compareReceiverRanks(ranked[i].Terms, ranked[j].Terms) > 0
	})
	return ranked
}

func compareReceiverRanks(left, right []ReceiverRankTerm) int {
	for termIndex := 0; termIndex < len(left) && termIndex < len(right); termIndex++ {
		leftValues, rightValues := left[termIndex].Values, right[termIndex].Values
		for valueIndex := range leftValues {
			switch {
			case leftValues[valueIndex] > rightValues[valueIndex]:
				return 1
			case leftValues[valueIndex] < rightValues[valueIndex]:
				return -1
			}
		}
	}
	return 0
}
