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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

var _ = Describe("Repack Execute, scope, maxPerRun & lifecycle", func() {
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

	// C6: Execute actually commits — it evicts and writes durable nominations.
	It("Execute evicts and records nominations", func() {
		moving := occupyNativeDeployment(ctx, "exec-moving", nodes[0], "move", 4)
		staying := occupyNativeDeployment(ctx, "exec-staying", nodes[1], "stay", 2)
		defer deleteNativeWorkloads(ctx, moving, staying)

		run, err := newRun("execute", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("Executed"))
		Expect(got.Status.Plan).NotTo(BeNil())
		Expect(got.Status.Plan.Summary).NotTo(BeNil())
		Expect(got.Status.Plan.Summary.ResolvedScope).NotTo(BeNil())
		Expect(got.Status.Plan.Summary.ResolvedScope.NodeCount).To(BeEquivalentTo(3))
		Expect(got.Status.Plan.Summary.ResolvedScope.PodGroupCount).To(BeNumerically(">=", 2))
		Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeNumerically(">=", 1), "Execute must preserve the predicted node-freeing benefit")
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MetricsVerified).To(BeTrue())
		Expect(got.Status.Result.FreedNodeCount).To(BeNumerically(">=", 1), "Execute must report nodes actually free after replacement binding")
		Expect(got.Status.Result.FragAfterPercent).To(BeNumerically("<=", got.Status.Plan.Summary.FragBeforePercent),
			"Execute terminal status must report the remeasured cluster fragmentation")
		Expect(got.Status.Nominations).NotTo(BeEmpty(), "Execute must record placement nominations")
	})

	// C8: after an Execute finishes, a second Execute within the cooldown window is
	// gated (Queued/ExecuteCoolingDown). (K=1 concurrent AnotherRunActive is timing-
	// racy in e2e — the gate logic itself is unit-tested in state.EvaluateGate.)
	It("gates a second Execute during the cooldown window", func() {
		moving := occupyNativeDeployment(ctx, "cooldown-moving", nodes[0], "move", 4)
		staying := occupyNativeDeployment(ctx, "cooldown-staying", nodes[1], "stay", 2)
		defer deleteNativeWorkloads(ctx, moving, staying)

		first, err := newRun("cooldown-1", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, first.Name)
		waitTerminal(ctx, first.Name)

		second, err := newRun("cooldown-2", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, second.Name)
		Expect(waitCondition(ctx, second.Name, "Queued")).To(Equal("ExecuteCoolingDown"))
	})

	// C9: if every eviction is rejected (a maxUnavailable=0 PDB), Execute fails.
	It("fails with ExecuteFailed when all evictions are blocked by a PDB", func() {
		jobA := occupy(ctx, "pdb-a", nodes[0], 4)
		jobB := occupy(ctx, "pdb-b", nodes[1], 2)
		// Every movable fixture pod is protected. This makes every possible plan
		// eviction fail, rather than allowing the planner to choose an unprotected
		// alternative and accidentally turn this into a non-asserting test.
		blockAll := intstr.FromInt(0)
		for _, job := range []*batchv1alpha1.Job{jobA, jobB} {
			pdbName := "block-" + job.Name
			_, err := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Create(context.TODO(),
				&policyv1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{Name: pdbName},
					Spec: policyv1.PodDisruptionBudgetSpec{
						MaxUnavailable: &blockAll,
						Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"volcano.sh/job-name": job.Name}},
					},
				}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() int32 {
				pdb, getErr := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Get(context.TODO(), pdbName, metav1.GetOptions{})
				if getErr != nil {
					return -1
				}
				return pdb.Status.DisruptionsAllowed
			}, repackTimeout, repackPoll).Should(Equal(int32(0)), "PDB must become effective before Execute")
		}

		run, err := newRun("execfail", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(completeReason(got)).To(Equal("ExecuteFailed"))
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackFailed))
		Expect(got.Status.Plan).NotTo(BeNil())
		Expect(got.Status.Plan.Summary).NotTo(BeNil())
		Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeNumerically(">=", 1),
			"the complete pre-eviction plan must remain visible after rejection")
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MovedCardCount).To(BeEquivalentTo(0))
		Expect(got.Status.Result.MetricsVerified).To(BeFalse())
		Expect(got.Status.Nominations).To(BeEmpty(), "rejected evictions must not retain placement intents")
	})

	// E16: scope.nodes.exclude — an excluded node is never a drain target, so it is
	// not freed even if it could be.
	It("scope.nodes.exclude keeps a node from being drained", func() {
		occupy(ctx, "sc-a", nodes[0], 4)
		occupy(ctx, "sc-b", nodes[1], 2)

		scope := &repackv1alpha1.RepackScope{
			Nodes: &repackv1alpha1.RepackSelectorTerm{
				Exclude: &repackv1alpha1.RepackSelector{Names: []string{nodes[0], nodes[1]}},
			},
		}
		run, err := newRun("scope-nodes", repackv1alpha1.RepackModeDryRun).goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		// Both occupied nodes are excluded from draining -> nothing to free.
		Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeEquivalentTo(0))
		Expect(got.Status.Plan.FreedNodes).NotTo(ContainElement(nodes[0]))
		Expect(got.Status.Plan.FreedNodes).NotTo(ContainElement(nodes[1]))
	})

	// E14: scope.podGroups.include by exact name — only the selected gang may move.
	It("scope.podGroups.include limits which gangs move", func() {
		occupy(ctx, "inc-a", nodes[0], 4)
		occupy(ctx, "inc-b", nodes[1], 2)
		pgs := podGroupNames(ctx)
		Expect(len(pgs)).To(BeNumerically(">=", 2))

		// Include only the first PodGroup.
		scope := &repackv1alpha1.RepackScope{
			PodGroups: &repackv1alpha1.RepackSelectorTerm{
				Include: &repackv1alpha1.RepackSelector{Names: []string{pgs[0]}},
			},
		}
		run, err := newRun("scope-pg", repackv1alpha1.RepackModeDryRun).goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		// Every move must belong to the included PodGroup.
		for _, m := range got.Status.Plan.Moves {
			Expect(ctx.Namespace + "/" + m.PodGroupName).To(Equal(pgs[0]))
		}
	})

	// E14b/C6b: labels from a generic ReplicaSet-owned workload are projected to
	// its automatic PodGroup, and its replacement retains that PodGroup because
	// it is created by the same ReplicaSet.
	It("selects a native workload by PodGroup labels and places its replacement", func() {
		moving := occupyNativeDeployment(ctx, "native-moving", nodes[0], "move", 4)
		staying := occupyNativeDeployment(ctx, "native-staying", nodes[1], "stay", 2)
		defer func() {
			_ = ctx.Kubeclient.AppsV1().Deployments(ctx.Namespace).Delete(context.TODO(), moving.deployment.Name, metav1.DeleteOptions{})
			_ = ctx.Kubeclient.AppsV1().Deployments(ctx.Namespace).Delete(context.TODO(), staying.deployment.Name, metav1.DeleteOptions{})
		}()

		Eventually(func() map[string]string {
			pg, getErr := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Get(context.TODO(), moving.podGroup, metav1.GetOptions{})
			if getErr != nil {
				return nil
			}
			return pg.Labels
		}, repackTimeout, repackPoll).Should(HaveKeyWithValue(nativeScopeLabel, "move"), "automatic PodGroup must expose pod-template labels")

		scope := &repackv1alpha1.RepackScope{PodGroups: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nativeScopeLabel: "move"}}},
		}}
		run, err := newRun("native-selector", repackv1alpha1.RepackModeExecute).goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("Executed"))
		Expect(got.Status.Plan.Moves).NotTo(BeEmpty())
		for _, move := range got.Status.Plan.Moves {
			Expect(move.PodGroupName).To(Equal(moving.podGroup), "only the selector-matched native PodGroup may move")
		}
		Expect(got.Status.Nominations).To(HaveLen(1), "one relocated native replica needs one nomination")
		nomination := got.Status.Nominations[0]
		Expect(nomination.PodGroupName).To(Equal(moving.podGroup))
		Expect(nomination.SelectedNodeName).NotTo(BeEmpty(), "engine must persist its live receiver selection")
		Expect(got.Status.Plan.FreedNodes).NotTo(ContainElement(nomination.SelectedNodeName), "engine must not select a node this run frees")
		Expect(nomination.ActualNodeName).To(Equal(nomination.SelectedNodeName), "controller must record the actual placement")
		Eventually(func() bool {
			pods, listErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: nativeWorkloadLabel + "=" + moving.deployment.Name,
			})
			if listErr != nil || len(pods.Items) != 1 {
				return false
			}
			replacement := pods.Items[0]
			return replacement.Name != moving.podName &&
				replacement.Annotations["scheduling.k8s.io/group-name"] == moving.podGroup &&
				replacement.Spec.NodeName == nomination.SelectedNodeName
		}, repackTimeout, repackPoll).Should(BeTrue(), "Deployment replacement must retain its ReplicaSet-derived PodGroup")
	})

	// F18: maxPerRun.podGroups caps the number of gangs a single run relocates.
	It("maxPerRun.podGroups caps moved gangs", func() {
		occupy(ctx, "cap-a", nodes[0], 2)
		occupy(ctx, "cap-b", nodes[1], 2)
		occupy(ctx, "cap-c", nodes[2], 2)

		one := int32(1)
		run, err := newRun("maxperrun", repackv1alpha1.RepackModeDryRun).goal(npuResource).
			maxPerRun(&repackv1alpha1.MaxPerRun{PodGroups: &one}).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("RepackRecommended"))
		Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeEquivalentTo(1))
		Expect(got.Status.Plan.Moves).To(HaveLen(1), "maxPerRun.podGroups=1 must allow exactly one gang move")
	})

	// F19: maxPerRun.resources is measured in user-facing whole accelerator
	// cards. The only useful relocation here moves a 2-card gang.
	It("maxPerRun.resources caps moved accelerator cards", func() {
		occupy(ctx, "resource-cap-a", nodes[0], 4)
		occupy(ctx, "resource-cap-b", nodes[1], 2)

		blocked, err := newRun("resource-cap-blocked", repackv1alpha1.RepackModeDryRun).goal(npuResource).
			maxPerRun(&repackv1alpha1.MaxPerRun{
				Resources: v1.ResourceList{npuResource: resource.MustParse("1")},
			}).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, blocked.Name)
		blockedRun := waitTerminal(ctx, blocked.Name)
		Expect(blockedRun.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(blockedRun)).To(Equal("BelowGoalThreshold"))
		Expect(blockedRun.Status.Plan.Moves).To(BeEmpty())

		admitted, err := newRun("resource-cap-admitted", repackv1alpha1.RepackModeDryRun).goal(npuResource).
			maxPerRun(&repackv1alpha1.MaxPerRun{
				Resources: v1.ResourceList{npuResource: resource.MustParse("2")},
			}).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, admitted.Name)
		admittedRun := waitTerminal(ctx, admitted.Name)
		Expect(admittedRun.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(admittedRun)).To(Equal("RepackRecommended"))
		Expect(admittedRun.Status.Plan.Summary.FreedNodeCount).To(BeEquivalentTo(1))
		Expect(admittedRun.Status.Plan.Moves).To(HaveLen(1))
	})

	// G20: a finished run with a short TTL is GC-deleted by the controller.
	It("TTL GC deletes a finished run", func() {
		occupy(ctx, "ttl", nodes[0], 4) // clean -> Succeeded quickly

		run, err := newRun("ttl-gc", repackv1alpha1.RepackModeDryRun).goal(npuResource).ttl(10).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		waitTerminal(ctx, run.Name)

		err = wait.PollUntilContextTimeout(context.TODO(), repackPoll, repackTimeout, false,
			func(c context.Context) (bool, error) {
				_, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(c, run.Name, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				return false, nil
			})
		Expect(err).NotTo(HaveOccurred(), "run should be GC-deleted after its TTL")
	})
})

func deleteNativeWorkloads(ctx *e2eutil.TestContext, workloads ...*nativeWorkload) {
	for _, workload := range workloads {
		if workload == nil {
			continue
		}
		if workload.deployment != nil {
			_ = ctx.Kubeclient.AppsV1().Deployments(ctx.Namespace).Delete(context.TODO(), workload.deployment.Name, metav1.DeleteOptions{})
		}
		if workload.statefulSet != nil {
			_ = ctx.Kubeclient.AppsV1().StatefulSets(ctx.Namespace).Delete(context.TODO(), workload.statefulSet.Name, metav1.DeleteOptions{})
		}
	}
}

// Note on coverage gaps that are intentionally NOT e2e-tested here:
//   - K=1 concurrent (AnotherRunActive): the window is too small to observe
//     reliably (Execute is open-loop-fast); covered by state.EvaluateGate UT and
//     the engine's TestRequeueGatedRuns / gate concurrency UT.
//   - /metrics and /healthz endpoints: the engine has no Service, so scraping its
//     pod requires port-forward; covered by the cmd wiring + UT.
