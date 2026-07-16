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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

var _ = Describe("Repack nominations & reliability boundaries", func() {
	var ctx *e2eutil.TestContext
	var nodes []string

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 3)
	})
	AfterEach(func() {
		for _, n := range nodes {
			clearNPU(ctx, n)
		}
		e2eutil.CleanupTestContext(ctx)
	})

	// Execute records well-formed nominations wired to the plan. All assertions read
	// the terminal status object, so this is fully deterministic and stable.
	//
	// The fixture uses scheduler-placed native workloads, so the terminal status
	// covers the full replacement protocol rather than a Pod pinned by spec.nodeName.
	It("records well-formed nominations wired to the plan", func() {
		moving := occupyNativeDeployment(ctx, "nom-moving", nodes[0], "move", 4)
		staying := occupyNativeDeployment(ctx, "nom-staying", nodes[1], "stay", 2)
		defer deleteNativeWorkloads(ctx, moving, staying)

		run, err := newRun("nominations", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("Executed"))
		Expect(got.Status.Plan).NotTo(BeNil())
		Expect(got.Status.Nominations).NotTo(BeEmpty(), "Execute must record placement nominations")

		// Lookup sets built entirely from the plan (all from status -> stable).
		freed := map[string]bool{}
		for _, n := range got.Status.Plan.FreedNodes {
			freed[n] = true
		}
		moveTargets := map[string]bool{}
		movePGs := map[string]bool{}
		totalMovedPods := 0
		for _, m := range got.Status.Plan.Moves {
			movePGs[m.PodGroupName] = true
			totalMovedPods += len(m.Pods)
			for _, pm := range m.Pods {
				if pm.ToNode != "" {
					moveTargets[pm.ToNode] = true
				}
			}
		}
		fixture := map[string]bool{}
		for _, n := range nodes {
			fixture[n] = true
		}

		// Each nomination is well-formed and never targets a freed node.
		for _, nom := range got.Status.Nominations {
			Expect(nom.Namespace).To(Equal(ctx.Namespace), "nomination namespace")
			Expect(nom.NodeName).NotTo(BeEmpty(), "nomination must name a target node")
			Expect(fixture).To(HaveKey(nom.NodeName), "target node must be a real fixture node")
			Expect(freed).NotTo(HaveKey(nom.NodeName), "a nomination must never target a freed node")
			Expect(moveTargets).To(HaveKey(nom.NodeName), "nomination node must coincide with a plan move ToNode")
			Expect(movePGs).To(HaveKey(nom.PodGroupName), "nomination must reference a moved PodGroup")
			Expect(nom.ExpirationTime).NotTo(BeNil(), "nomination needs a TTL bound")
			Expect(nom.ExpirationTime.Time.After(time.Now())).To(BeTrue(), "nomination expiration must be in the future")
			Expect(nom.Phase).To(Equal(repackv1alpha1.PodPlacementPlaced), "terminal success requires verified placement")
			Expect(nom.SelectedNodeName).NotTo(BeEmpty(), "live receiver selection must be recorded")
			Expect(nom.ActualNodeName).To(Equal(nom.SelectedNodeName), "actual node must match the selected receiver")
		}

		// Exact linkage: one placement nomination per relocated pod.
		Expect(len(got.Status.Nominations)).To(Equal(totalMovedPods), "one nomination per relocated pod")
	})

	// Boundary: a goal for a resource that no node provides must not divide-by-zero
	// on the fragmentation denominator (M=0); it reports NoFragmentation, before==0.
	It("reports NoFragmentation for a goal resource no node provides", func() {
		occupy(ctx, "absent-a", nodes[0], 4)
		occupy(ctx, "absent-b", nodes[1], 2)

		// Fully-qualified (passes the CEL "/" rule) but advertised by no node.
		absent := v1.ResourceName("volcano.sh/e2e-absent")
		run, err := newRun("absent-res", repackv1alpha1.RepackModeDryRun).goal(absent).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("NoFragmentation"))
		Expect(got.Status.Plan.Summary.FragBeforePercent).To(BeEquivalentTo(0), "no provider -> 0% fragmentation, no NaN")
		Expect(got.Status.Plan.Moves).To(BeEmpty())
	})

	// Reliability: DryRun is read-only, so two identical runs against an unchanged
	// cluster must produce identical fragmentation numbers and plan shape.
	It("produces identical results for repeated DryRuns on an unchanged cluster", func() {
		occupy(ctx, "det-a", nodes[0], 4)
		occupy(ctx, "det-b", nodes[1], 2)

		first, err := newRun("determinism-1", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, first.Name)
		a := waitTerminal(ctx, first.Name)

		second, err := newRun("determinism-2", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, second.Name)
		b := waitTerminal(ctx, second.Name)

		Expect(a.Status.Plan).NotTo(BeNil())
		Expect(b.Status.Plan).NotTo(BeNil())
		Expect(b.Status.Plan.Summary.FragBeforePercent).To(Equal(a.Status.Plan.Summary.FragBeforePercent))
		Expect(b.Status.Plan.Summary.FragAfterPercent).To(Equal(a.Status.Plan.Summary.FragAfterPercent))
		Expect(b.Status.Plan.Summary.FreedNodeCount).To(Equal(a.Status.Plan.Summary.FreedNodeCount))
		Expect(len(b.Status.Plan.Moves)).To(Equal(len(a.Status.Plan.Moves)))
	})
})
