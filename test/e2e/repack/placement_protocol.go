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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

const repackSystemNamespace = "volcano-system"

var _ = Describe("Repack placement protocol", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 3)
	})
	AfterEach(func() {
		recordSpecFailureDiagnostics(ctx)
		e2eutil.CleanupTestContext(ctx)
		for _, node := range nodes {
			clearNPU(ctx, node)
		}
	})

	// This test pauses the Engine after it has started its informers. It creates a
	// durable, in-flight placement record through the API, proving that webhook
	// gating is synchronous and that a restarted Engine resumes selection rather
	// than treating the Run as an interrupted eviction.
	It("gates a replacement, resumes after Engine restart, and records a selected placement", func() {
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		run, pgName, replacement := prepareGatedPlacement(ctx, "placement-recover", nodes[1], []string{nodes[0]}, 90*time.Second)
		defer deleteRun(ctx, run.Name)
		Expect(hasSchedulingGate(replacement, repackv1alpha1.PlacementGateName)).To(BeTrue(), "webhook must synchronously gate the replacement")

		Eventually(func() repackv1alpha1.PodNominationPhase {
			return getRun(ctx, run.Name).Status.Nominations[0].Phase
		}, repackTimeout, repackPoll).Should(Equal(repackv1alpha1.PodPlacementGated), "controller must report the concrete gated replacement")

		restoreEngine()

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("Executed"))
		nomination := got.Status.Nominations[0]
		Expect(nomination.Phase).To(Equal(repackv1alpha1.PodPlacementPlaced))
		Expect(nomination.SelectedNodeName).To(Equal(nodes[1]), "the planned receiver is preferred when it remains immediately idle")
		Expect(got.Status.Plan.FreedNodes).NotTo(ContainElement(nomination.SelectedNodeName), "freed nodes are never placement receivers")
		Expect(nomination.ActualNodeName).To(Equal(nodes[1]))
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MetricsVerified).To(BeTrue())
		Expect(got.Status.Result.FreedNodes).To(Equal(got.Status.Plan.FreedNodes),
			"successful Execute must verify the exact planned freed-node set")

		Eventually(func() bool {
			pod, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(context.TODO(), replacement.Name, metav1.GetOptions{})
			return err == nil && pod.Spec.NodeName == nodes[1] && !hasSchedulingGate(pod, repackv1alpha1.PlacementGateName)
		}, repackTimeout, repackPoll).Should(BeTrue(), "controller must open only the repack gate after persisting selection")
		assertPlacementLeaseReleased(ctx, pgName)
	})

	// The nomination protocol deliberately persists the selected receiver in the
	// RepackRun before it patches Pod status and opens the scheduling gate. Stop
	// the controller at exactly that durable checkpoint and verify that a fresh
	// controller process resumes the remaining idempotent operations.
	It("resumes a nominated replacement after controller-manager restart", func() {
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		run, pgName, replacement := prepareGatedPlacement(
			ctx, "placement-controller-recover", nodes[1], []string{nodes[0]}, 90*time.Second)
		defer deleteRun(ctx, run.Name)
		Eventually(func() repackv1alpha1.PodNominationPhase {
			return getRun(ctx, run.Name).Status.Nominations[0].Phase
		}, repackTimeout, repackPoll).Should(Equal(repackv1alpha1.PodPlacementGated))

		restoreController := pauseRepackControllerManager(ctx)
		defer restoreController()
		setPlacementSelection(ctx, run.Name, nodes[1])

		Consistently(func() bool {
			pod, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(
				context.TODO(), replacement.Name, metav1.GetOptions{})
			return err == nil && pod.Status.NominatedNodeName == "" &&
				hasSchedulingGate(pod, repackv1alpha1.PlacementGateName)
		}, 5*time.Second, repackPoll).Should(BeTrue(),
			"while the controller is stopped, the durable selection must not mutate the Pod")

		restoreController()
		Eventually(func() bool {
			pod, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(
				context.TODO(), replacement.Name, metav1.GetOptions{})
			return err == nil && pod.Spec.NodeName == nodes[1] &&
				!hasSchedulingGate(pod, repackv1alpha1.PlacementGateName)
		}, repackTimeout, repackPoll).Should(BeTrue(),
			"a restarted nominator must patch nominatedNodeName, open the gate, and observe binding")
		Eventually(func() repackv1alpha1.PodNominationPhase {
			return getRun(ctx, run.Name).Status.Nominations[0].Phase
		}, repackTimeout, repackPoll).Should(Equal(repackv1alpha1.PodPlacementPlaced))

		restoreEngine()
		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("Executed"))
		Expect(got.Status.Nominations[0].SelectedNodeName).To(Equal(nodes[1]))
		Expect(got.Status.Nominations[0].ActualNodeName).To(Equal(nodes[1]))
		assertPlacementLeaseReleased(ctx, pgName)
	})

	// These are real controller replacement paths: Repack's drain operation is
	// modeled with the Eviction API against an already-running native workload.
	// The replacement is therefore created by its Deployment/StatefulSet only
	// after the original Pod has disappeared, which is the race that automatic
	// PodGroup derivation must close.
	It("gates and places a Deployment replacement through its ReplicaSet PodGroup", func() {
		workload := occupyNativeDeployment(ctx, "placement-deployment", nodes[0], "move", 2)
		defer deleteNativeWorkloads(ctx, workload)
		verifyNativeReplacementPlacement(ctx, nodes, workload, "placement-deployment")
	})

	It("gates and places a StatefulSet replacement through its StatefulSet PodGroup", func() {
		workload := occupyNativeStatefulSet(ctx, "placement-statefulset", nodes[0], "move", 2)
		defer deleteNativeWorkloads(ctx, workload)
		verifyNativeReplacementPlacement(ctx, nodes, workload, "placement-statefulset")
	})

	// Exercise the complete Execute path without importing a workload-specific
	// controller. The test creates an ordinary owner and models only the generic
	// cascade behavior after Repack has issued its real evictions: the workload
	// removes the old unit and reconstructs an equivalent PodGroup under a new
	// name. Planning, lease activation, eviction, admission gating, nomination,
	// scheduling, and benefit verification all remain production code paths.
	It("executes through workload-level cascading PodGroup recreation", func() {
		controller := true
		owner, err := ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(
			context.TODO(), &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "placement-cascade-owner", Namespace: ctx.Namespace},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ownerReference := metav1.OwnerReference{
			APIVersion: "v1", Kind: "ConfigMap", Name: owner.Name, UID: owner.UID, Controller: &controller,
		}

		originalPodGroupName := "placement-cascade-original"
		_, err = ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Create(
			context.TODO(), &schedulingv1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: originalPodGroupName, Namespace: ctx.Namespace,
					OwnerReferences: []metav1.OwnerReference{ownerReference},
				},
				Spec: schedulingv1beta1.PodGroupSpec{MinMember: 2},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		oneCard := resource.MustParse("1")
		createPod := func(name, podGroupName, nodeName string) *v1.Pod {
			pod, createErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Create(
				context.TODO(), &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: name, Namespace: ctx.Namespace,
						Annotations: map[string]string{
							schedulingv1beta1.KubeGroupNameAnnotationKey: podGroupName,
						},
					},
					Spec: v1.PodSpec{
						NodeName: nodeName, SchedulerName: e2eutil.SchedulerName,
						RestartPolicy: v1.RestartPolicyNever,
						Containers: []v1.Container{{
							Name: name, Image: e2eutil.DefaultNginxImage,
							ImagePullPolicy: v1.PullIfNotPresent,
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{npuResource: oneCard},
								Limits:   v1.ResourceList{npuResource: oneCard},
							},
						}},
					},
				}, metav1.CreateOptions{})
			Expect(createErr).NotTo(HaveOccurred())
			return pod
		}
		originalPods := []*v1.Pod{
			createPod("placement-cascade-original-0", originalPodGroupName, nodes[0]),
			createPod("placement-cascade-original-1", originalPodGroupName, nodes[0]),
		}
		for _, pod := range originalPods {
			Expect(e2eutil.WaitPodReady(ctx, pod)).To(Succeed())
		}

		// Leave exactly two cards on the receiver. This makes moving the two-card
		// source group both feasible and beneficial while scope prevents any
		// unrelated workload from becoming a drain target.
		sixCards := resource.MustParse("6")
		receiver, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Create(
			context.TODO(), &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "placement-cascade-receiver", Namespace: ctx.Namespace},
				Spec: v1.PodSpec{
					NodeName: nodes[1], RestartPolicy: v1.RestartPolicyNever,
					Containers: []v1.Container{{
						Name: "receiver", Image: e2eutil.DefaultNginxImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{npuResource: sixCards},
							Limits:   v1.ResourceList{npuResource: sixCards},
						},
					}},
				},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(e2eutil.WaitPodReady(ctx, receiver)).To(Succeed())

		scope := &repackv1alpha1.RepackScope{
			PodGroups: &repackv1alpha1.RepackSelectorTerm{
				Include: &repackv1alpha1.RepackSelector{
					Names: []string{ctx.Namespace + "/" + originalPodGroupName},
				},
			},
		}
		run, err := newRun("placement-cascade", repackv1alpha1.RepackModeExecute).
			goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		Eventually(func() bool {
			latest := getRun(ctx, run.Name)
			return latest.Status.Phase == repackv1alpha1.RepackRunning &&
				latest.Status.Plan != nil && len(latest.Status.Plan.Moves) == 1 &&
				latest.Status.Plan.Moves[0].PodGroupName == originalPodGroupName &&
				len(latest.Status.Nominations) == 2
		}, repackTimeout, repackPoll).Should(BeTrue(),
			"the real Engine must plan and prepare both original Pod evictions")

		Eventually(func() int {
			remaining := 0
			for _, pod := range originalPods {
				_, getErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(
					context.TODO(), pod.Name, metav1.GetOptions{})
				if getErr == nil {
					remaining++
				}
			}
			return remaining
		}, repackTimeout, repackPoll).Should(Equal(0), "Engine must evict the original PodGroup")

		// Model a generic workload controller's group-level recovery after the
		// first eviction has cascaded through its old serving unit.
		Expect(ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Delete(
			context.TODO(), originalPodGroupName, metav1.DeleteOptions{})).To(Succeed())
		waitPodGroupDeleted(ctx, ctx.Namespace, originalPodGroupName)
		replacementPodGroupName := "placement-cascade-replacement"
		replacementPodGroup, err := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Create(
			context.TODO(), &schedulingv1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: replacementPodGroupName, Namespace: ctx.Namespace,
					OwnerReferences: []metav1.OwnerReference{ownerReference},
				},
				Spec: schedulingv1beta1.PodGroupSpec{MinMember: 2},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(replacementPodGroup.Annotations).To(HaveKeyWithValue(
			repackv1alpha1.PlacementLeaseAnnotation, run.Name+"/"+string(run.UID)),
			"the PodGroup CREATE webhook must carry the active placement lease onto the new generation")

		replacementPods := []*v1.Pod{
			createPod("placement-cascade-replacement-0", replacementPodGroupName, ""),
			createPod("placement-cascade-replacement-1", replacementPodGroupName, ""),
		}
		for _, pod := range replacementPods {
			Expect(hasSchedulingGate(pod, repackv1alpha1.PlacementGateName)).To(BeTrue(),
				"every Pod in the reconstructed unit must be gated before scheduling")
		}

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("Executed"))
		Expect(got.Status.Plan.FreedNodes).To(Equal([]string{nodes[0]}))
		Expect(got.Status.Result.FreedNodes).To(Equal(got.Status.Plan.FreedNodes))
		Expect(got.Status.Nominations).To(HaveLen(2))
		for index := range got.Status.Nominations {
			nomination := &got.Status.Nominations[index]
			Expect(nomination.PodGroupName).To(Equal(originalPodGroupName))
			Expect(nomination.ReplacementPodGroupName).To(Equal(replacementPodGroupName))
			Expect(nomination.ReplacementPodName).To(HavePrefix("placement-cascade-replacement-"))
			Expect(nomination.Phase).To(Equal(repackv1alpha1.PodPlacementPlaced))
			Expect(nomination.ActualNodeName).To(Equal(nodes[1]))
		}
		assertPlacementLeaseReleased(ctx, replacementPodGroupName)
	})

	// Models a controller such as Kthena ServingGroupRecreate without importing
	// that project: one eviction has caused the workload to delete its original
	// PodGroup and recreate both the group and Pod under different names.
	It("gates and places a replacement whose workload recreates the entire PodGroup", func() {
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		controller := true
		owner, err := ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "placement-pg-recreate-owner", Namespace: ctx.Namespace},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ownerReference := metav1.OwnerReference{
			APIVersion: "v1", Kind: "ConfigMap", Name: owner.Name, UID: owner.UID, Controller: &controller,
		}

		run, err := newRun("placement-pg-recreate", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)
		oldPodGroupName := run.Name + "-old"
		newPodGroupName := run.Name + "-new"
		victimPodName := run.Name + "-victim"
		replacementPodName := run.Name + "-replacement"
		expires := metav1.NewTime(time.Now().Add(90 * time.Second))
		started := metav1.Now()
		run.Status = repackv1alpha1.RepackRunStatus{
			Phase:     repackv1alpha1.RepackRunning,
			StartTime: &started,
			Plan: &repackv1alpha1.RepackPlan{
				Summary:    &repackv1alpha1.RepackSummary{FreedNodeCount: 1, MovedCardCount: 2},
				FreedNodes: []string{nodes[0]},
				Moves: []repackv1alpha1.RepackMove{{
					Namespace: ctx.Namespace, PodGroupName: oldPodGroupName, Cards: 2,
					Owner: &repackv1alpha1.WorkloadRef{APIVersion: "v1", Kind: "ConfigMap", Name: owner.Name},
					Pods: []repackv1alpha1.PodMove{{
						Name: victimPodName, FromNode: nodes[0], ToNode: nodes[1], Cards: 2,
					}},
				}},
			},
			Result: &repackv1alpha1.RepackResult{MovedCardCount: 2},
			Nominations: []repackv1alpha1.PodNomination{{
				Namespace: ctx.Namespace, PodGroupName: oldPodGroupName, VictimPodName: victimPodName,
				NodeName: nodes[1], ExpirationTime: &expires, Phase: repackv1alpha1.PodPlacementPrepared,
			}},
		}
		run, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().UpdateStatus(context.TODO(), run, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().Patch(
			context.TODO(), run.Name, types.MergePatchType,
			[]byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"true"}}}`,
				repackv1alpha1.PlacementActiveLabel)),
			metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred())

		_, err = ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Create(context.TODO(), &schedulingv1beta1.PodGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name: oldPodGroupName, Namespace: ctx.Namespace, OwnerReferences: []metav1.OwnerReference{ownerReference},
			},
			Spec: schedulingv1beta1.PodGroupSpec{MinMember: 1},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Delete(
			context.TODO(), oldPodGroupName, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() bool {
			_, getErr := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Get(
				context.TODO(), oldPodGroupName, metav1.GetOptions{})
			return apierrors.IsNotFound(getErr)
		}, fixtureTimeout, repackPoll).Should(BeTrue())

		newPodGroup, err := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Create(
			context.TODO(), &schedulingv1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: newPodGroupName, Namespace: ctx.Namespace, OwnerReferences: []metav1.OwnerReference{ownerReference},
				},
				Spec: schedulingv1beta1.PodGroupSpec{MinMember: 1},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(newPodGroup.Annotations).To(HaveKeyWithValue(
			repackv1alpha1.PlacementLeaseAnnotation, run.Name+"/"+string(run.UID)),
			"PodGroup CREATE webhook must atomically inject the active placement lease")

		quantity := resource.MustParse("2")
		replacement := e2eutil.CreatePod(ctx, e2eutil.PodSpec{
			Name: replacementPodName, SchedulerName: e2eutil.SchedulerName, RestartPolicy: v1.RestartPolicyNever,
			Req: v1.ResourceList{npuResource: quantity}, Limit: v1.ResourceList{npuResource: quantity},
			Annotations: map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: newPodGroupName},
		})
		Expect(hasSchedulingGate(replacement, repackv1alpha1.PlacementGateName)).To(BeTrue())

		Eventually(func() repackv1alpha1.PodNomination {
			return getRun(ctx, run.Name).Status.Nominations[0]
		}, repackTimeout, repackPoll).Should(And(
			HaveField("ReplacementPodGroupName", newPodGroupName),
			HaveField("Phase", repackv1alpha1.PodPlacementGated),
		))

		restoreEngine()
		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(got.Status.Nominations[0].ReplacementPodGroupName).To(Equal(newPodGroupName))
		Expect(got.Status.Nominations[0].Phase).To(Equal(repackv1alpha1.PodPlacementPlaced))
		Expect(got.Status.Message).To(ContainSubstring(
			ctx.Namespace + "/" + oldPodGroupName + " -> " + ctx.Namespace + "/" + newPodGroupName))
		assertPlacementLeaseReleased(ctx, newPodGroupName)
	})

	// Some serving controllers can reconstruct the same logical workload more
	// than once while recovery is in progress. The original PodGroup remains the
	// audit identity, while ReplacementPodGroupName must advance from v1 to v2
	// and forget the deleted v1 Pod before v2 claims the nomination.
	It("advances placement mapping across repeated PodGroup recreation", func() {
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		controller := true
		owner, err := ctx.Kubeclient.CoreV1().ConfigMaps(ctx.Namespace).Create(context.TODO(), &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "placement-pg-recreate-twice-owner", Namespace: ctx.Namespace},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ownerReference := metav1.OwnerReference{
			APIVersion: "v1", Kind: "ConfigMap", Name: owner.Name, UID: owner.UID, Controller: &controller,
		}

		run, err := newRun("placement-pg-recreate-twice", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)
		originalPodGroupName := run.Name + "-original"
		victimPodName := run.Name + "-victim"
		expires := metav1.NewTime(time.Now().Add(90 * time.Second))
		run.Status = repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Plan: &repackv1alpha1.RepackPlan{
				Summary:    &repackv1alpha1.RepackSummary{FreedNodeCount: 1, MovedCardCount: 2},
				FreedNodes: []string{nodes[0]},
				Moves: []repackv1alpha1.RepackMove{{
					Namespace: ctx.Namespace, PodGroupName: originalPodGroupName, Cards: 2,
					Owner: &repackv1alpha1.WorkloadRef{APIVersion: "v1", Kind: "ConfigMap", Name: owner.Name},
					Pods: []repackv1alpha1.PodMove{{
						Name: victimPodName, FromNode: nodes[0], ToNode: nodes[1], Cards: 2,
					}},
				}},
			},
			Result: &repackv1alpha1.RepackResult{MovedCardCount: 2},
			Nominations: []repackv1alpha1.PodNomination{{
				Namespace: ctx.Namespace, PodGroupName: originalPodGroupName, VictimPodName: victimPodName,
				NodeName: nodes[1], ExpirationTime: &expires, Phase: repackv1alpha1.PodPlacementPrepared,
			}},
		}
		run, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().UpdateStatus(
			context.TODO(), run, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().Patch(
			context.TODO(), run.Name, types.MergePatchType,
			[]byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"true"}}}`,
				repackv1alpha1.PlacementActiveLabel)),
			metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred())

		_, err = ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Create(
			context.TODO(), &schedulingv1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: originalPodGroupName, Namespace: ctx.Namespace,
					OwnerReferences: []metav1.OwnerReference{ownerReference},
				},
				Spec: schedulingv1beta1.PodGroupSpec{MinMember: 1},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Delete(
			context.TODO(), originalPodGroupName, metav1.DeleteOptions{})).To(Succeed())
		waitPodGroupDeleted(ctx, ctx.Namespace, originalPodGroupName)

		createGeneration := func(generation string) (string, *v1.Pod) {
			podGroupName := run.Name + "-" + generation
			podGroup, createErr := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Create(
				context.TODO(), &schedulingv1beta1.PodGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: podGroupName, Namespace: ctx.Namespace,
						OwnerReferences: []metav1.OwnerReference{ownerReference},
					},
					Spec: schedulingv1beta1.PodGroupSpec{MinMember: 1},
				}, metav1.CreateOptions{})
			Expect(createErr).NotTo(HaveOccurred())
			Expect(podGroup.Annotations).To(HaveKeyWithValue(
				repackv1alpha1.PlacementLeaseAnnotation, run.Name+"/"+string(run.UID)))

			quantity := resource.MustParse("2")
			pod := e2eutil.CreatePod(ctx, e2eutil.PodSpec{
				Name: run.Name + "-" + generation + "-pod", SchedulerName: e2eutil.SchedulerName,
				RestartPolicy: v1.RestartPolicyNever,
				Req:           v1.ResourceList{npuResource: quantity},
				Limit:         v1.ResourceList{npuResource: quantity},
				Annotations: map[string]string{
					schedulingv1beta1.KubeGroupNameAnnotationKey: podGroupName,
				},
			})
			Expect(hasSchedulingGate(pod, repackv1alpha1.PlacementGateName)).To(BeTrue())
			return podGroupName, pod
		}

		firstPodGroupName, firstPod := createGeneration("v1")
		Eventually(func() repackv1alpha1.PodNomination {
			return getRun(ctx, run.Name).Status.Nominations[0]
		}, repackTimeout, repackPoll).Should(And(
			HaveField("ReplacementPodGroupName", firstPodGroupName),
			HaveField("ReplacementPodName", firstPod.Name),
			HaveField("Phase", repackv1alpha1.PodPlacementGated),
		))

		Expect(ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Delete(
			context.TODO(), firstPod.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() bool {
			_, getErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(
				context.TODO(), firstPod.Name, metav1.GetOptions{})
			return apierrors.IsNotFound(getErr)
		}, fixtureTimeout, repackPoll).Should(BeTrue())
		Expect(ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Delete(
			context.TODO(), firstPodGroupName, metav1.DeleteOptions{})).To(Succeed())
		waitPodGroupDeleted(ctx, ctx.Namespace, firstPodGroupName)

		secondPodGroupName, secondPod := createGeneration("v2")
		Eventually(func() repackv1alpha1.PodNomination {
			return getRun(ctx, run.Name).Status.Nominations[0]
		}, repackTimeout, repackPoll).Should(And(
			HaveField("PodGroupName", originalPodGroupName),
			HaveField("ReplacementPodGroupName", secondPodGroupName),
			HaveField("ReplacementPodName", secondPod.Name),
			HaveField("ReplacementPodUID", secondPod.UID),
			HaveField("Phase", repackv1alpha1.PodPlacementGated),
		))

		restoreEngine()
		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(got.Status.Nominations[0].ReplacementPodGroupName).To(Equal(secondPodGroupName))
		Expect(got.Status.Nominations[0].Phase).To(Equal(repackv1alpha1.PodPlacementPlaced))
		Expect(got.Status.Message).To(ContainSubstring(
			ctx.Namespace + "/" + originalPodGroupName + " -> " + ctx.Namespace + "/" + secondPodGroupName))
		assertPlacementLeaseReleased(ctx, secondPodGroupName)
	})

	// Names are only unique inside a namespace. Use deliberately identical
	// workload, PodGroup, victim, and replacement Pod names in two namespaces to
	// prove that lease discovery, recreation mapping, and concrete Pod claiming
	// all key their state by namespace as well as name.
	It("keeps identical PodGroup recreation identities isolated by namespace", func() {
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		peerNamespace := ctx.Namespace + "-peer"
		_, err := ctx.Kubeclient.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: peerNamespace},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			foreground := metav1.DeletePropagationForeground
			_ = ctx.Kubeclient.CoreV1().Namespaces().Delete(
				context.TODO(), peerNamespace, metav1.DeleteOptions{PropagationPolicy: &foreground})
			Eventually(func() bool {
				_, getErr := ctx.Kubeclient.CoreV1().Namespaces().Get(
					context.TODO(), peerNamespace, metav1.GetOptions{})
				return apierrors.IsNotFound(getErr)
			}, repackTimeout, repackPoll).Should(BeTrue(), "peer namespace must be deleted")
		}()

		const (
			ownerName               = "same-workload"
			originalPodGroupName    = "same-original-pg"
			replacementPodGroupName = "same-replacement-pg"
			victimPodName           = "same-victim"
			replacementPodName      = "same-replacement"
		)
		controller := true
		ownerReferences := map[string]metav1.OwnerReference{}
		for _, namespace := range []string{ctx.Namespace, peerNamespace} {
			owner, createErr := ctx.Kubeclient.CoreV1().ConfigMaps(namespace).Create(
				context.TODO(), &v1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: ownerName, Namespace: namespace},
				}, metav1.CreateOptions{})
			Expect(createErr).NotTo(HaveOccurred())
			ownerReferences[namespace] = metav1.OwnerReference{
				APIVersion: "v1", Kind: "ConfigMap", Name: owner.Name, UID: owner.UID, Controller: &controller,
			}
		}

		run, err := newRun("placement-namespace-isolation", repackv1alpha1.RepackModeExecute).
			goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)
		expires := metav1.NewTime(time.Now().Add(90 * time.Second))
		run.Status = repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Plan: &repackv1alpha1.RepackPlan{
				Summary:    &repackv1alpha1.RepackSummary{FreedNodeCount: 1, MovedCardCount: 4},
				FreedNodes: []string{nodes[0]},
				Moves: []repackv1alpha1.RepackMove{
					{
						Namespace: ctx.Namespace, PodGroupName: originalPodGroupName, Cards: 2,
						Owner: &repackv1alpha1.WorkloadRef{APIVersion: "v1", Kind: "ConfigMap", Name: ownerName},
						Pods: []repackv1alpha1.PodMove{{
							Name: victimPodName, FromNode: nodes[0], ToNode: nodes[1], Cards: 2,
						}},
					},
					{
						Namespace: peerNamespace, PodGroupName: originalPodGroupName, Cards: 2,
						Owner: &repackv1alpha1.WorkloadRef{APIVersion: "v1", Kind: "ConfigMap", Name: ownerName},
						Pods: []repackv1alpha1.PodMove{{
							Name: victimPodName, FromNode: nodes[0], ToNode: nodes[2], Cards: 2,
						}},
					},
				},
			},
			Result: &repackv1alpha1.RepackResult{MovedCardCount: 4},
			Nominations: []repackv1alpha1.PodNomination{
				{
					Namespace: ctx.Namespace, PodGroupName: originalPodGroupName, VictimPodName: victimPodName,
					NodeName: nodes[1], ExpirationTime: &expires, Phase: repackv1alpha1.PodPlacementPrepared,
				},
				{
					Namespace: peerNamespace, PodGroupName: originalPodGroupName, VictimPodName: victimPodName,
					NodeName: nodes[2], ExpirationTime: &expires, Phase: repackv1alpha1.PodPlacementPrepared,
				},
			},
		}
		run, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().UpdateStatus(
			context.TODO(), run, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().Patch(
			context.TODO(), run.Name, types.MergePatchType,
			[]byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"true"}}}`,
				repackv1alpha1.PlacementActiveLabel)),
			metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred())

		for _, namespace := range []string{ctx.Namespace, peerNamespace} {
			_, createErr := ctx.Vcclient.SchedulingV1beta1().PodGroups(namespace).Create(
				context.TODO(), &schedulingv1beta1.PodGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: originalPodGroupName, Namespace: namespace,
						OwnerReferences: []metav1.OwnerReference{ownerReferences[namespace]},
					},
					Spec: schedulingv1beta1.PodGroupSpec{MinMember: 1},
				}, metav1.CreateOptions{})
			Expect(createErr).NotTo(HaveOccurred())
			Expect(ctx.Vcclient.SchedulingV1beta1().PodGroups(namespace).Delete(
				context.TODO(), originalPodGroupName, metav1.DeleteOptions{})).To(Succeed())
			waitPodGroupDeleted(ctx, namespace, originalPodGroupName)
		}

		replacements := map[string]*v1.Pod{}
		quantity := resource.MustParse("2")
		for _, namespace := range []string{ctx.Namespace, peerNamespace} {
			podGroup, createErr := ctx.Vcclient.SchedulingV1beta1().PodGroups(namespace).Create(
				context.TODO(), &schedulingv1beta1.PodGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: replacementPodGroupName, Namespace: namespace,
						OwnerReferences: []metav1.OwnerReference{ownerReferences[namespace]},
					},
					Spec: schedulingv1beta1.PodGroupSpec{MinMember: 1},
				}, metav1.CreateOptions{})
			Expect(createErr).NotTo(HaveOccurred())
			Expect(podGroup.Annotations).To(HaveKeyWithValue(
				repackv1alpha1.PlacementLeaseAnnotation, run.Name+"/"+string(run.UID)))

			pod, createErr := ctx.Kubeclient.CoreV1().Pods(namespace).Create(
				context.TODO(), &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: replacementPodName, Namespace: namespace,
						Annotations: map[string]string{
							schedulingv1beta1.KubeGroupNameAnnotationKey: replacementPodGroupName,
						},
					},
					Spec: v1.PodSpec{
						SchedulerName: e2eutil.SchedulerName,
						RestartPolicy: v1.RestartPolicyNever,
						Containers: []v1.Container{{
							Name: replacementPodName, Image: e2eutil.DefaultNginxImage,
							ImagePullPolicy: v1.PullIfNotPresent,
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{npuResource: quantity},
								Limits:   v1.ResourceList{npuResource: quantity},
							},
						}},
					},
				}, metav1.CreateOptions{})
			Expect(createErr).NotTo(HaveOccurred())
			Expect(hasSchedulingGate(pod, repackv1alpha1.PlacementGateName)).To(BeTrue())
			replacements[namespace] = pod
		}

		Eventually(func() bool {
			latest := getRun(ctx, run.Name)
			if len(latest.Status.Nominations) != 2 {
				return false
			}
			seenNamespaces := map[string]bool{}
			for index := range latest.Status.Nominations {
				nomination := &latest.Status.Nominations[index]
				replacement := replacements[nomination.Namespace]
				if replacement == nil ||
					nomination.PodGroupName != originalPodGroupName ||
					nomination.ReplacementPodGroupName != replacementPodGroupName ||
					nomination.ReplacementPodName != replacement.Name ||
					nomination.ReplacementPodUID != replacement.UID ||
					nomination.Phase != repackv1alpha1.PodPlacementGated {
					return false
				}
				seenNamespaces[nomination.Namespace] = true
			}
			return seenNamespaces[ctx.Namespace] && seenNamespaces[peerNamespace]
		}, repackTimeout, repackPoll).Should(BeTrue(),
			"same-named PodGroups and Pods must claim only nominations from their own namespaces")

		restoreEngine()
		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(got.Status.Nominations).To(HaveLen(2))
		for index := range got.Status.Nominations {
			nomination := &got.Status.Nominations[index]
			Expect(nomination.ReplacementPodGroupName).To(Equal(replacementPodGroupName))
			Expect(nomination.ReplacementPodName).To(Equal(replacementPodName))
			Expect(nomination.Phase).To(Equal(repackv1alpha1.PodPlacementPlaced))
			assertPlacementLeaseReleasedInNamespace(ctx, nomination.Namespace, replacementPodGroupName)
		}
	})

	// A PodGroup lease intentionally closes admission for every new Pod in the
	// unit. For a Deployment, a concurrent scale-out Pod is indistinguishable
	// from a replacement until the victim is gone. It remains held during that
	// ambiguity, then terminal cleanup releases its gate and owner marker.
	It("holds a concurrent Deployment scale-out Pod, then releases it when placement terminates", func() {
		workload := occupyNativeDeployment(ctx, "placement-scale-out", nodes[0], "move", 2)
		defer deleteNativeWorkloads(ctx, workload)
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()
		run := prepareGatedNativeReplacement(ctx, "placement-scale-out", workload, nodes[1], []string{nodes[0]}, 90*time.Second)
		defer deleteRun(ctx, run.Name)

		deployment, err := ctx.Kubeclient.AppsV1().Deployments(ctx.Namespace).Get(context.TODO(), workload.deployment.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		replicas := int32(2)
		deployment = deployment.DeepCopy()
		deployment.Spec.Replicas = &replicas
		_, err = ctx.Kubeclient.AppsV1().Deployments(ctx.Namespace).Update(context.TODO(), deployment, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(), "scale Deployment while a placement lease is active")

		var scaleOut *v1.Pod
		Eventually(func() bool {
			pods, listErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: nativeWorkloadLabel + "=" + workload.deployment.Name})
			if listErr != nil || len(pods.Items) != 2 {
				return false
			}
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.UID == workload.podUID {
					continue
				}
				scaleOut = pod.DeepCopy()
				return hasSchedulingGate(scaleOut, repackv1alpha1.PlacementGateName) &&
					scaleOut.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] == run.Name+"/"+string(run.UID)
			}
			return false
		}, repackTimeout, repackPoll).Should(BeTrue(), "ambiguous scale-out Pod must remain protected by the active placement lease")

		run = getRun(ctx, run.Name)
		run.Status.Phase = repackv1alpha1.RepackFailed
		_, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().UpdateStatus(context.TODO(), run, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(), "mark placement Run terminal to trigger owner cleanup")
		restoreEngine()

		Eventually(func() bool {
			pod, getErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(context.TODO(), scaleOut.Name, metav1.GetOptions{})
			return getErr == nil && pod.Spec.NodeName != "" && !hasSchedulingGate(pod, repackv1alpha1.PlacementGateName) &&
				pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] == ""
		}, repackTimeout, repackPoll).Should(BeTrue(), "terminal cleanup must release the held scale-out Pod")
	})

	// The Engine sees no immediately idle receiver: the planned receiver and the
	// only fallback receiver are both occupied, while the source node is excluded
	// by the accepted plan. The deadline must release the gate and fail the Run
	// instead of stranding the workload indefinitely.
	It("keeps the gate while capacity is unavailable, then expires and releases it at the deadline", func() {
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		occupy(ctx, "placement-planned-receiver-blocker", nodes[1], npuPerNode)
		occupy(ctx, "placement-fallback-receiver-blocker", nodes[2], npuPerNode)
		// Allow an Engine restart to finish informer cache synchronization before the
		// deadline. The assertion below is specifically about the observable
		// AwaitingCapacity state, not merely the terminal expiration.
		run, pgName, replacement := prepareGatedPlacement(ctx, "placement-expire", nodes[1], []string{nodes[0]}, time.Minute)
		defer deleteRun(ctx, run.Name)
		Expect(hasSchedulingGate(replacement, repackv1alpha1.PlacementGateName)).To(BeTrue())

		Eventually(func() repackv1alpha1.PodNominationPhase {
			return getRun(ctx, run.Name).Status.Nominations[0].Phase
		}, repackTimeout, repackPoll).Should(Equal(repackv1alpha1.PodPlacementGated))

		restoreEngine()
		Eventually(func() repackv1alpha1.PodNominationPhase {
			return getRun(ctx, run.Name).Status.Nominations[0].Phase
		}, repackTimeout, repackPoll).Should(Equal(repackv1alpha1.PodPlacementAwaitingCapacity), "gate must remain while no immediately idle receiver exists")
		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackFailed))
		Expect(completeReason(got)).To(Equal("PlacementExpired"))
		Expect(got.Status.Nominations[0].Phase).To(Equal(repackv1alpha1.PodPlacementExpired))
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MetricsVerified).To(BeFalse())
		Expect(got.Status.Result.FreedNodes).To(BeEmpty())

		Eventually(func() bool {
			pod, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(context.TODO(), replacement.Name, metav1.GetOptions{})
			return err == nil && !hasSchedulingGate(pod, repackv1alpha1.PlacementGateName)
		}, repackTimeout, repackPoll).Should(BeTrue(), "deadline must release only the repack placement gate")
		assertPlacementLeaseReleased(ctx, pgName)
	})

	// A selected receiver can disappear after selection but before the scheduler
	// binds the Pod. The controller records the actual node as Degraded, but the
	// Run still succeeds when the replacement binds and the exact planned source
	// node is verified free. The explicit selection here models that short
	// concurrent-change window while the Engine is paused at the durable checkpoint.
	It("reports placement drift without failing a realized node-freeing plan", func() {
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		occupy(ctx, "placement-drift-blocker", nodes[1], npuPerNode)
		run, pgName, replacement := prepareGatedPlacement(ctx, "placement-drift", nodes[1], []string{nodes[0]}, 90*time.Second)
		defer deleteRun(ctx, run.Name)
		Eventually(func() repackv1alpha1.PodNominationPhase {
			return getRun(ctx, run.Name).Status.Nominations[0].Phase
		}, repackTimeout, repackPoll).Should(Equal(repackv1alpha1.PodPlacementGated))

		setPlacementSelection(ctx, run.Name, nodes[1])
		Eventually(func() string {
			latest := getRun(ctx, run.Name)
			if len(latest.Status.Nominations) != 1 {
				return ""
			}
			nomination := latest.Status.Nominations[0]
			if nomination.Phase != repackv1alpha1.PodPlacementDegraded {
				return ""
			}
			return nomination.ActualNodeName
		}, repackTimeout, repackPoll).ShouldNot(BeEmpty(), "scheduler must bind on a feasible node and controller must expose the drift")

		restoreEngine()
		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("ExecutedWithPlacementDrift"))
		Expect(got.Status.Nominations[0].SelectedNodeName).To(Equal(nodes[1]))
		Expect(got.Status.Nominations[0].ActualNodeName).NotTo(BeEmpty())
		Expect(got.Status.Nominations[0].ActualNodeName).NotTo(Equal(nodes[1]))
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MetricsVerified).To(BeTrue())
		Expect(got.Status.Result.FreedNodes).To(Equal(got.Status.Plan.FreedNodes),
			"placement drift is non-fatal only when the exact planned node set is free")
		assertPlacementLeaseReleased(ctx, pgName)
		Eventually(func() bool {
			pod, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(context.TODO(), replacement.Name, metav1.GetOptions{})
			return err == nil && !hasSchedulingGate(pod, repackv1alpha1.PlacementGateName)
		}, repackTimeout, repackPoll).Should(BeTrue())
	})

	// A replacement can bind successfully while unrelated concurrent work lands
	// on the planned source node. Success is defined by the exact freed-node set,
	// not only by replacement health, so this must be an operator-visible failure.
	It("fails when a replacement is placed but the exact planned node is not freed", func() {
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		run, pgName, replacement := prepareGatedPlacement(
			ctx, "placement-benefit-missed", nodes[1], []string{nodes[0]}, 90*time.Second)
		defer deleteRun(ctx, run.Name)
		Eventually(func() repackv1alpha1.PodNominationPhase {
			return getRun(ctx, run.Name).Status.Nominations[0].Phase
		}, repackTimeout, repackPoll).Should(Equal(repackv1alpha1.PodPlacementGated))

		occupy(ctx, "placement-benefit-blocker", nodes[0], 1)
		restoreEngine()

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackFailed))
		Expect(completeReason(got)).To(Equal("BenefitNotRealized"))
		Expect(got.Status.Nominations[0].Phase).To(Equal(repackv1alpha1.PodPlacementPlaced),
			"replacement health alone must not turn an unrealized plan into success")
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MetricsVerified).To(BeTrue())
		Expect(got.Status.Result.FreedNodes).NotTo(ContainElement(nodes[0]))
		Expect(got.Status.Message).To(ContainSubstring(nodes[0]),
			"status.message must identify the planned node that remained occupied")
		Eventually(func() bool {
			pod, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(
				context.TODO(), replacement.Name, metav1.GetOptions{})
			return err == nil && pod.Spec.NodeName == nodes[1]
		}, repackTimeout, repackPoll).Should(BeTrue())
		assertPlacementLeaseReleased(ctx, pgName)
	})
})

// pauseRepackEngine gives the test a stable protocol checkpoint. The return
// function is idempotent so it is safe in both normal and deferred cleanup paths.
func pauseRepackEngine(ctx *e2eutil.TestContext) func() {
	return pauseSystemDeployment(ctx, "app=volcano-repack-engine", "repack Engine")
}

func pauseRepackControllerManager(ctx *e2eutil.TestContext) func() {
	return pauseSystemDeployment(ctx, "app=volcano-controller", "Volcano controller-manager")
}

func pauseSystemDeployment(ctx *e2eutil.TestContext, labelSelector, component string) func() {
	deployments, err := ctx.Kubeclient.AppsV1().Deployments(repackSystemNamespace).List(
		context.TODO(), metav1.ListOptions{LabelSelector: labelSelector})
	Expect(err).NotTo(HaveOccurred(), "list %s deployments", component)
	Expect(deployments.Items).To(HaveLen(1), "exactly one %s deployment must exist", component)
	deployment := deployments.Items[0].DeepCopy()
	deploymentName := deployment.Name
	original := int32(1)
	if deployment.Spec.Replicas != nil {
		original = *deployment.Spec.Replicas
	}
	zero := int32(0)
	deployment.Spec.Replicas = &zero
	_, err = ctx.Kubeclient.AppsV1().Deployments(repackSystemNamespace).Update(context.TODO(), deployment, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "scale down %s", component)
	Eventually(func() int {
		pods, listErr := ctx.Kubeclient.CoreV1().Pods(repackSystemNamespace).List(
			context.TODO(), metav1.ListOptions{LabelSelector: labelSelector})
		if listErr != nil {
			return -1
		}
		return len(pods.Items)
	}, repackTimeout, repackPoll).Should(Equal(0), "%s must be stopped before creating checkpoint state", component)

	restored := false
	return func() {
		if restored {
			return
		}
		restored = true
		current, getErr := ctx.Kubeclient.AppsV1().Deployments(repackSystemNamespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
		Expect(getErr).NotTo(HaveOccurred())
		current.Spec.Replicas = &original
		_, updateErr := ctx.Kubeclient.AppsV1().Deployments(repackSystemNamespace).Update(context.TODO(), current, metav1.UpdateOptions{})
		Expect(updateErr).NotTo(HaveOccurred(), "restore %s", component)
		Eventually(func() int32 {
			latest, latestErr := ctx.Kubeclient.AppsV1().Deployments(repackSystemNamespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
			if latestErr != nil {
				return -1
			}
			return latest.Status.AvailableReplicas
		}, repackTimeout, repackPoll).Should(BeNumerically(">=", original), "%s must become available", component)
	}
}

func waitPodGroupDeleted(ctx *e2eutil.TestContext, namespace, podGroupName string) {
	Eventually(func() bool {
		_, err := ctx.Vcclient.SchedulingV1beta1().PodGroups(namespace).Get(
			context.TODO(), podGroupName, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, fixtureTimeout, repackPoll).Should(BeTrue(), "PodGroup %s/%s must be deleted", namespace, podGroupName)
}

func prepareGatedPlacement(ctx *e2eutil.TestContext, name, plannedNode string, freedNodes []string, deadline time.Duration) (*repackv1alpha1.RepackRun, string, *v1.Pod) {
	run, err := newRun(name, repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
	Expect(err).NotTo(HaveOccurred())
	pgName := fmt.Sprintf("%s-pg", run.Name)
	podName := fmt.Sprintf("%s-replacement", run.Name)
	expires := metav1.NewTime(time.Now().Add(deadline))
	fromNode := ""
	if len(freedNodes) > 0 {
		fromNode = freedNodes[0]
	}
	run.Status = repackv1alpha1.RepackRunStatus{
		Phase: repackv1alpha1.RepackRunning,
		Plan: &repackv1alpha1.RepackPlan{
			Summary: &repackv1alpha1.RepackSummary{
				FreedNodeCount: int32(len(freedNodes)),
				MovedCardCount: 2,
			},
			FreedNodes: freedNodes,
			Moves: []repackv1alpha1.RepackMove{{
				Namespace: ctx.Namespace, PodGroupName: pgName, Cards: 2,
				Pods: []repackv1alpha1.PodMove{{Name: podName, FromNode: fromNode, ToNode: plannedNode, Cards: 2}},
			}},
		},
		Result: &repackv1alpha1.RepackResult{MovedCardCount: 2},
		Nominations: []repackv1alpha1.PodNomination{{
			Namespace: ctx.Namespace, PodGroupName: pgName, VictimPodName: podName,
			NodeName: plannedNode, ExpirationTime: &expires, Phase: repackv1alpha1.PodPlacementPrepared,
		}},
	}
	run, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().UpdateStatus(context.TODO(), run, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "persist in-flight placement checkpoint")

	_, err = ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Create(context.TODO(), &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: pgName, Namespace: ctx.Namespace,
			Annotations: map[string]string{repackv1alpha1.PlacementLeaseAnnotation: run.Name + "/" + string(run.UID)},
		},
		Spec: schedulingv1beta1.PodGroupSpec{MinMember: 1},
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "create leased PodGroup")

	quantity := resource.MustParse("2")
	resources := v1.ResourceList{npuResource: quantity}
	pod := e2eutil.CreatePod(ctx, e2eutil.PodSpec{
		Name: podName, SchedulerName: e2eutil.SchedulerName, RestartPolicy: v1.RestartPolicyNever,
		Req:         resources,
		Limit:       resources,
		Annotations: map[string]string{"scheduling.k8s.io/group-name": pgName},
	})
	return run, pgName, pod
}

// verifyNativeReplacementPlacement exercises the same lifecycle as Execute:
// an already-running Pod has a leased PodGroup, drain evicts it, and the native
// controller creates a fresh replacement. The Engine is paused only while the
// replacement reaches admission, so the test can reliably observe the gate.
func verifyNativeReplacementPlacement(ctx *e2eutil.TestContext, nodes []string, workload *nativeWorkload, runPrefix string) {
	Expect(workload).NotTo(BeNil())
	restoreEngine := pauseRepackEngine(ctx)
	defer restoreEngine()

	run := prepareGatedNativeReplacement(ctx, runPrefix, workload, nodes[1], []string{nodes[0]}, 90*time.Second)
	defer deleteRun(ctx, run.Name)
	Expect(ctx.Kubeclient.PolicyV1().Evictions(ctx.Namespace).Evict(context.TODO(), &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: workload.podName, Namespace: ctx.Namespace},
	})).To(Succeed(), "evict original native Pod")

	var replacement *v1.Pod
	Eventually(func() bool {
		pods, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: nativeWorkloadLabel + "=" + workloadName(workload),
		})
		if err != nil || len(pods.Items) != 1 {
			return false
		}
		replacement = pods.Items[0].DeepCopy()
		return replacement.UID != workload.podUID &&
			hasSchedulingGate(replacement, repackv1alpha1.PlacementGateName) &&
			replacement.Annotations[schedulingv1beta1.KubeGroupNameAnnotationKey] == workload.podGroup
	}, repackTimeout, repackPoll).Should(BeTrue(), "native controller replacement must remain gated after pg-controller associates its automatic PodGroup")

	Eventually(func() repackv1alpha1.PodNominationPhase {
		return getRun(ctx, run.Name).Status.Nominations[0].Phase
	}, repackTimeout, repackPoll).Should(Equal(repackv1alpha1.PodPlacementGated), "controller must report the concrete gated replacement")

	restoreEngine()
	got := waitTerminal(ctx, run.Name)
	Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
	Expect(got.Status.Nominations).To(HaveLen(1))
	Expect(got.Status.Nominations[0].SelectedNodeName).NotTo(BeEmpty())
	Expect(got.Status.Plan.FreedNodes).NotTo(ContainElement(got.Status.Nominations[0].SelectedNodeName))
	Expect(got.Status.Nominations[0].ActualNodeName).To(Equal(got.Status.Nominations[0].SelectedNodeName))
	Expect(got.Status.Result).NotTo(BeNil())
	Expect(got.Status.Result.MetricsVerified).To(BeTrue())
	waitPodEventReasons(ctx, replacement,
		"RepackReplacementGated", "RepackPlacementNominated", "RepackPlacementSucceeded")
	assertPlacementLeaseReleased(ctx, workload.podGroup)
}

func prepareGatedNativeReplacement(ctx *e2eutil.TestContext, name string, workload *nativeWorkload, plannedNode string, freedNodes []string, deadline time.Duration) *repackv1alpha1.RepackRun {
	run, err := newRun(name, repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
	Expect(err).NotTo(HaveOccurred())
	expires := metav1.NewTime(time.Now().Add(deadline))
	fromNode := ""
	if len(freedNodes) > 0 {
		fromNode = freedNodes[0]
	}
	run.Status = repackv1alpha1.RepackRunStatus{
		Phase: repackv1alpha1.RepackRunning,
		Plan: &repackv1alpha1.RepackPlan{
			Summary: &repackv1alpha1.RepackSummary{
				FreedNodeCount: int32(len(freedNodes)),
				MovedCardCount: 2,
			},
			FreedNodes: freedNodes,
			Moves: []repackv1alpha1.RepackMove{{
				Namespace: ctx.Namespace, PodGroupName: workload.podGroup, Cards: 2,
				Pods: []repackv1alpha1.PodMove{{
					Name: workload.podName, FromNode: fromNode, ToNode: plannedNode, Cards: 2,
				}},
			}},
		},
		Result: &repackv1alpha1.RepackResult{MovedCardCount: 2},
		Nominations: []repackv1alpha1.PodNomination{{
			Namespace: ctx.Namespace, PodGroupName: workload.podGroup, VictimPodName: workload.podName,
			NodeName: plannedNode, ExpirationTime: &expires, Phase: repackv1alpha1.PodPlacementPrepared,
		}},
	}
	run, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().UpdateStatus(context.TODO(), run, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "persist native in-flight placement checkpoint")

	podGroup, err := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Get(context.TODO(), workload.podGroup, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "get automatic PodGroup before eviction")
	podGroup = podGroup.DeepCopy()
	if podGroup.Annotations == nil {
		podGroup.Annotations = map[string]string{}
	}
	podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] = run.Name + "/" + string(run.UID)
	_, err = ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Update(context.TODO(), podGroup, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "lease automatic PodGroup before eviction")
	return run
}

func workloadName(workload *nativeWorkload) string {
	if workload != nil && workload.deployment != nil {
		return workload.deployment.Name
	}
	if workload != nil && workload.statefulSet != nil {
		return workload.statefulSet.Name
	}
	return ""
}

func assertPlacementLeaseReleased(ctx *e2eutil.TestContext, podGroupName string) {
	assertPlacementLeaseReleasedInNamespace(ctx, ctx.Namespace, podGroupName)
}

func assertPlacementLeaseReleasedInNamespace(ctx *e2eutil.TestContext, namespace, podGroupName string) {
	Eventually(func() string {
		pg, err := ctx.Vcclient.SchedulingV1beta1().PodGroups(namespace).Get(
			context.TODO(), podGroupName, metav1.GetOptions{})
		if err != nil {
			return "unexpected-error"
		}
		return pg.Annotations[repackv1alpha1.PlacementLeaseAnnotation]
	}, repackTimeout, repackPoll).Should(BeEmpty(),
		"terminal Run must release PodGroup %s/%s placement lease", namespace, podGroupName)
}

func setPlacementSelection(ctx *e2eutil.TestContext, runName, selectedNode string) {
	run := getRun(ctx, runName)
	Expect(run.Status.Nominations).To(HaveLen(1))
	run.Status.Nominations[0].SelectedNodeName = selectedNode
	_, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().UpdateStatus(context.TODO(), run, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "persist selected receiver")
}

func hasSchedulingGate(pod *v1.Pod, gateName string) bool {
	if pod == nil {
		return false
	}
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == gateName {
			return true
		}
	}
	return false
}
