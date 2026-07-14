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

// Package repack holds the repack (runtime GPU/NPU defragmentation) e2e suite.
//
// Fixtures: real GPU/NPU hardware is not present in CI, so the tests advertise a
// FAKE fully-qualified extended resource (npuResource) on the worker nodes via the
// node status subresource. The scheduler accounts for it and kubelet admits pods
// requesting it (the container does not really use a device), which is enough to
// build a controllable fragmented layout of running pods.
package repack

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// npuResource is a fake, fully-qualified extended resource (contains "/", so it
// passes the goals.resource CEL rule). 8 units are advertised per worker node.
const (
	npuResource   = v1.ResourceName("volcano.sh/e2e-npu")
	npuPerNode    = 8
	repackTimeout = 3 * time.Minute
	repackPoll    = 2 * time.Second

	nativeWorkloadLabel  = "repack-e2e-workload"
	nativeScopeLabel     = "repack-e2e-scope"
	nativeSchedulingGate = "repack-e2e-hold"
)

// ---- node fixtures -------------------------------------------------------

// schedulableNodes returns worker node names (Ready, unschedulable=false, no
// control-plane taint) — the nodes the tests advertise fake NPUs on.
func schedulableNodes(ctx *e2eutil.TestContext) []string {
	nodes, err := ctx.Kubeclient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	var out []string
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Unschedulable {
			continue
		}
		control := false
		for _, taint := range n.Spec.Taints {
			if taint.Key == "node-role.kubernetes.io/control-plane" || taint.Key == "node-role.kubernetes.io/master" {
				control = true
			}
		}
		if control {
			continue
		}
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == v1.NodeReady && c.Status == v1.ConditionTrue {
				ready = true
			}
		}
		if ready {
			out = append(out, n.Name)
		}
	}
	return out
}

// advertiseNPU patches qty fake NPUs onto a node's capacity+allocatable.
func advertiseNPU(ctx *e2eutil.TestContext, node string, qty int) {
	patch := fmt.Sprintf(`{"status":{"capacity":{"%s":"%d"},"allocatable":{"%s":"%d"}}}`,
		npuResource, qty, npuResource, qty)
	_, err := ctx.Kubeclient.CoreV1().Nodes().Patch(
		context.TODO(), node, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}, "status")
	Expect(err).NotTo(HaveOccurred())
}

// clearNPU removes the fake NPU resource from a node (JSON-merge null deletes it).
func clearNPU(ctx *e2eutil.TestContext, node string) {
	patch := fmt.Sprintf(`{"status":{"capacity":{"%s":null},"allocatable":{"%s":null}}}`, npuResource, npuResource)
	_, _ = ctx.Kubeclient.CoreV1().Nodes().Patch(
		context.TODO(), node, types.MergePatchType, []byte(patch), metav1.PatchOptions{}, "status")
}

// npuFixture advertises fake NPUs on up to len(want) worker nodes and returns the
// node names it used. Registers cleanup that clears them again.
func npuFixture(ctx *e2eutil.TestContext, nodes int) []string {
	all := schedulableNodes(ctx)
	Expect(len(all)).To(BeNumerically(">=", nodes), "need at least %d schedulable worker nodes", nodes)
	used := all[:nodes]
	for _, n := range used {
		advertiseNPU(ctx, n, npuPerNode)
	}
	return used
}

// ---- occupying workloads -------------------------------------------------

// occupy creates a vcjob with one task requesting `cards` NPUs. If node != "" the
// task is pinned there (deterministic fragmented layout for DryRun); otherwise the
// scheduler places it (used for Execute, so the replacement can move). Waits Ready.
func occupy(ctx *e2eutil.TestContext, name, node string, cards int) *batchv1alpha1.Job {
	npuQty := resource.MustParse(fmt.Sprintf("%d", cards))
	npuList := v1.ResourceList{npuResource: npuQty}
	spec := &e2eutil.JobSpec{
		Name:      name,
		Namespace: ctx.Namespace,
		NodeName:  node,
		Tasks: []e2eutil.TaskSpec{{
			Name: "w", Min: 1, Rep: 1, Img: e2eutil.DefaultNginxImage,
			Req: npuList, Limit: npuList,
		}},
	}
	job := e2eutil.CreateJob(ctx, spec)
	Expect(e2eutil.WaitTasksReady(ctx, job, 1)).NotTo(HaveOccurred())
	return job
}

// nativeWorkload is a scheduler-placed Deployment replica that starts at a
// deterministic node only for fixture setup. Its PodGroup is created by the
// generic pg-controller path (ReplicaSet owner); no workload-specific Repack
// integration is involved.
type nativeWorkload struct {
	deployment *appsv1.Deployment
	podName    string
	podGroup   string
}

// occupyNativeDeployment creates one gated Deployment pod, binds it to node to
// form a deterministic fragmented layout, then removes the gate from the
// ReplicaSet template. A later Repack eviction therefore produces an ordinary
// Volcano-scheduled replacement, which can consume a nomination.
func occupyNativeDeployment(ctx *e2eutil.TestContext, name, node, scopeValue string, cards int) *nativeWorkload {
	replicas := int32(1)
	labels := map[string]string{
		nativeWorkloadLabel: name,
		nativeScopeLabel:    scopeValue,
	}
	quantity := resource.MustParse(fmt.Sprintf("%d", cards))
	deployment, err := ctx.Kubeclient.AppsV1().Deployments(ctx.Namespace).Create(context.TODO(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ctx.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nativeWorkloadLabel: name}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: v1.PodSpec{
					SchedulerName:   e2eutil.SchedulerName,
					RestartPolicy:   v1.RestartPolicyAlways,
					SchedulingGates: []v1.PodSchedulingGate{{Name: nativeSchedulingGate}},
					Containers: []v1.Container{{
						Name:            name,
						Image:           e2eutil.DefaultNginxImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Resources:       v1.ResourceRequirements{Requests: v1.ResourceList{npuResource: quantity}, Limits: v1.ResourceList{npuResource: quantity}},
					}},
				},
			},
		},
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "create native deployment")

	var initialPod *v1.Pod
	Eventually(func() bool {
		pods, listErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: nativeWorkloadLabel + "=" + name,
		})
		if listErr != nil || len(pods.Items) != 1 {
			return false
		}
		pod := &pods.Items[0]
		if pod.Annotations["scheduling.k8s.io/group-name"] == "" {
			return false
		}
		initialPod = pod.DeepCopy()
		return true
	}, repackTimeout, repackPoll).Should(BeTrue(), "automatic PodGroup must be associated with native pod")

	err = ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Bind(context.TODO(), &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: initialPod.Name, Namespace: ctx.Namespace},
		Target:     v1.ObjectReference{Kind: "Node", Name: node},
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "bind gated native pod")
	Eventually(func() v1.PodPhase {
		pod, getErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(context.TODO(), initialPod.Name, metav1.GetOptions{})
		if getErr != nil {
			return v1.PodUnknown
		}
		return pod.Status.Phase
	}, repackTimeout, repackPoll).Should(Equal(v1.PodRunning), "manually bound native pod should run")

	owner := metav1.GetControllerOf(initialPod)
	Expect(owner).NotTo(BeNil(), "Deployment pod must have a ReplicaSet controller")
	Expect(owner.Kind).To(Equal("ReplicaSet"))
	replicaSet, err := ctx.Kubeclient.AppsV1().ReplicaSets(ctx.Namespace).Get(context.TODO(), owner.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "get native deployment ReplicaSet")
	replicaSet.Spec.Template.Spec.SchedulingGates = nil
	_, err = ctx.Kubeclient.AppsV1().ReplicaSets(ctx.Namespace).Update(context.TODO(), replicaSet, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "remove fixture scheduling gate from replacement template")

	return &nativeWorkload{
		deployment: deployment,
		podName:    initialPod.Name,
		podGroup:   initialPod.Annotations["scheduling.k8s.io/group-name"],
	}
}

// ---- RepackRun helpers ---------------------------------------------------

type runBuilder struct{ run *repackv1alpha1.RepackRun }

func newRun(name string, mode repackv1alpha1.RepackMode) *runBuilder {
	return &runBuilder{run: &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{GenerateName: name + "-"},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: mode},
	}}
}
func (b *runBuilder) goal(res v1.ResourceName) *runBuilder {
	b.run.Spec.Goals = []repackv1alpha1.RepackGoal{{Resource: res}}
	return b
}
func (b *runBuilder) ttl(sec int64) *runBuilder { b.run.Spec.TTLSecondsAfterFinished = &sec; return b }
func (b *runBuilder) scope(s *repackv1alpha1.RepackScope) *runBuilder {
	b.run.Spec.Scope = s
	return b
}
func (b *runBuilder) maxPerRun(m *repackv1alpha1.MaxPerRun) *runBuilder {
	b.run.Spec.MaxPerRun = m
	return b
}

// create submits the RepackRun; the caller defers deleteRun(created.Name).
func (b *runBuilder) create(ctx *e2eutil.TestContext) (*repackv1alpha1.RepackRun, error) {
	return ctx.Vcclient.RepackV1alpha1().RepackRuns().Create(context.TODO(), b.run, metav1.CreateOptions{})
}

func getRun(ctx *e2eutil.TestContext, name string) *repackv1alpha1.RepackRun {
	r, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(context.TODO(), name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return r
}

func deleteRun(ctx *e2eutil.TestContext, name string) {
	_ = ctx.Vcclient.RepackV1alpha1().RepackRuns().Delete(context.TODO(), name, metav1.DeleteOptions{})
}

// waitTerminal blocks until the run reaches a terminal phase and returns it.
func waitTerminal(ctx *e2eutil.TestContext, name string) *repackv1alpha1.RepackRun {
	var last *repackv1alpha1.RepackRun
	err := wait.PollUntilContextTimeout(context.TODO(), repackPoll, repackTimeout, false,
		func(c context.Context) (bool, error) {
			r, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(c, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			last = r
			switch r.Status.Phase {
			case repackv1alpha1.RepackSucceeded, repackv1alpha1.RepackFailed, repackv1alpha1.RepackCancelled:
				return true, nil
			}
			return false, nil
		})
	Expect(err).NotTo(HaveOccurred(), "run %s did not reach a terminal phase (is the repack engine running?)", name)
	return last
}

// waitCondition blocks until the run has a True condition of condType, returning
// its reason (used for the Queued gate, which is not terminal).
func waitCondition(ctx *e2eutil.TestContext, name, condType string) string {
	var reason string
	err := wait.PollUntilContextTimeout(context.TODO(), repackPoll, repackTimeout, false,
		func(c context.Context) (bool, error) {
			r, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(c, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			for _, cond := range r.Status.Conditions {
				if cond.Type == condType && cond.Status == metav1.ConditionTrue {
					reason = cond.Reason
					return true, nil
				}
			}
			return false, nil
		})
	Expect(err).NotTo(HaveOccurred(), "run %s never got a True %s condition", name, condType)
	return reason
}

// completeReason returns the reason of the True Complete/Failed condition.
func completeReason(run *repackv1alpha1.RepackRun) string {
	for _, c := range run.Status.Conditions {
		if c.Status == metav1.ConditionTrue && (c.Type == "Complete" || c.Type == "Failed") {
			return c.Reason
		}
	}
	return ""
}

// podGroupNames returns the "namespace/name" of every PodGroup in the test
// namespace (used to build scope.podGroups.include/exclude by exact name without
// guessing the vcjob's PodGroup naming).
func podGroupNames(ctx *e2eutil.TestContext) []string {
	pgs, err := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).List(context.TODO(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	var out []string
	for i := range pgs.Items {
		out = append(out, ctx.Namespace+"/"+pgs.Items[i].Name)
	}
	return out
}

// runningPodCount is the number of Running pods in the test namespace (to assert
// DryRun evicts nothing).
func runningPodCount(ctx *e2eutil.TestContext) int {
	pods, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).List(context.TODO(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	n := 0
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == v1.PodRunning {
			n++
		}
	}
	return n
}
