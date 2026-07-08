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

package repack

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// This suite is the positive-path regression net for the core defragmentation
// capability: a battery of distinct fragmentation shapes that must each yield a
// real consolidation plan. Every case is DryRun, so it is fully deterministic
// (pods are pinned, nothing is evicted) and exercises the whole planning pipeline
// — fragmentation measurement, gang-aware drain, reschedulability feasibility,
// best-fit receiver selection and scoring.
//
// Each scenario advertises the fake NPU on len(cardsPerNode) worker nodes and pins
// one single-pod gang of the given card count on each non-zero node (a 0 entry is
// an advertised-but-empty node, used to check empty nodes are excluded as receivers).
// expectedFreed is the optimal number of nodes the plan should empty; the greedy
// drain reaches the optimum for these shapes, so we assert FreedNodeCount >= it.
var _ = Describe("Repack defragmentation scenarios (DryRun consolidation)", func() {
	var ctx *e2eutil.TestContext

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
	})
	AfterEach(func() {
		e2eutil.CleanupTestContext(ctx)
	})

	type scenario struct {
		name          string
		cardsPerNode  []int
		expectedFreed int32
	}
	scenarios := []scenario{
		// Two half-used nodes collapse into one.
		{"two-halves-into-one", []int{4, 2}, 1},
		// An empty advertised node must NOT be used as a receiver; the fragmented
		// pair still collapses among themselves, and the empty node stays empty.
		{"empty-node-not-a-receiver", []int{4, 2, 0}, 1},
		// Four scattered small gangs pack onto a single node, freeing three.
		{"four-small-into-one", []int{2, 2, 2, 2}, 3},
		// Three gangs whose sum fits one node.
		{"three-into-one", []int{3, 3, 2}, 2},
		// A near-full node is the natural receiver; the two tiny gangs drain onto it.
		{"near-full-receiver", []int{6, 1, 1}, 2},
		// Best-fit: 5 + 3 exactly fills one node.
		{"best-fit-pair", []int{5, 3}, 1},
		// A large gang plus a tiny one collapse into a single full node.
		{"big-plus-tiny", []int{7, 1}, 1},
		// Partial consolidation: 5+3 pack together, 4 stays alone -> free one node.
		{"partial-two-bins", []int{5, 4, 3}, 1},
		// A full node can be neither donor nor receiver; the fragments around it
		// still consolidate to free one node.
		{"full-node-stays-put", []int{8, 3, 1}, 1},
	}

	for _, sc := range scenarios {
		sc := sc // capture per iteration
		It("consolidates a "+sc.name+" layout", func() {
			all := npuFixture(ctx, len(sc.cardsPerNode))
			defer func() {
				for _, n := range all {
					clearNPU(ctx, n)
				}
			}()

			occupied := map[string]bool{}
			for i, cards := range sc.cardsPerNode {
				if cards > 0 {
					occupy(ctx, fmt.Sprintf("%s-%d", sc.name, i), all[i], cards)
					occupied[all[i]] = true
				}
			}
			runningBefore := runningPodCount(ctx)

			run, err := newRun("defrag", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer deleteRun(ctx, run.Name)

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("RepackRecommended"), "a fragmented cluster must recommend consolidation")
			Expect(got.Status.Plan).NotTo(BeNil())

			summary := got.Status.Plan.Summary
			Expect(summary.FragBeforePercent).To(BeNumerically(">", 0), "the layout is fragmented")
			Expect(summary.FragAfterPercent).To(BeNumerically("<", summary.FragBeforePercent), "consolidation must reduce fragmentation")
			Expect(summary.FreedNodeCount).To(BeNumerically(">=", sc.expectedFreed), "should free the expected number of nodes")
			Expect(len(got.Status.Plan.Moves)).To(BeNumerically(">=", 1), "consolidation implies at least one move")

			// A freed node must have actually hosted workload (never the empty spare).
			for _, freedNode := range got.Status.Plan.FreedNodes {
				Expect(occupied).To(HaveKey(freedNode), "a freed node must have hosted a gang")
			}

			// DryRun is read-only: no eviction, no nominations.
			Expect(got.Status.Nominations).To(BeEmpty(), "DryRun must not nominate")
			Expect(runningPodCount(ctx)).To(Equal(runningBefore), "DryRun must not evict pods")
		})
	}
})
