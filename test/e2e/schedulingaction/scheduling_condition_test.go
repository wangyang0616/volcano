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

package schedulingaction

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	resourcehelper "k8s.io/component-helpers/resource"
	"k8s.io/utils/ptr"

	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	e2eutil "volcano.sh/volcano/test/e2e/util"
)

const pipelineConditionSchedulerConfig = `actions: "allocate, preempt, backfill"
tiers:
- plugins:
  - name: priority
  - name: gang
    enablePreemptable: false
  - name: conformance
- plugins:
  - name: predicates
  - name: proportion
  - name: nodeorder
    arguments:
      nodeaffinity.weight: 100
  - name: network-topology-aware
`

var _ = Describe("Pending Pod scheduling conditions", Serial, Label("scheduling-condition"), func() {
	DescribeTable("remain stable across discarded Pipeline attempts", func(restrictSecondTask bool) {
		f := newPipelineConditionFixture(restrictSecondTask)

		By("Confirming repeated Pipeline and rollback before observing Pod status")
		f.waitForAttempts(metav1.Now(), f.nodes[1], true, nil)
		observations := []*pipelineStatusObservation{
			f.observeStatus("candidate-a"),
			f.observeStatus("candidate-b"),
		}
		for _, observation := range observations {
			DeferCleanup(observation.stop)
		}
		checkObservations := func() error {
			for _, observation := range observations {
				if err := observation.err(); err != nil {
					return err
				}
			}
			return nil
		}

		// Only a scoring label changes. Pod specs, requests, available resources,
		// and the HyperNode minResource failure stay unchanged. This exercises both
		// candidate choices without depending on random tie-breaking in CI.
		for _, preferred := range []string{f.nodes[0], f.nodes[1]} {
			By("Selecting the next trial node through a soft node affinity preference")
			since := metav1.Now()
			f.preferNode(preferred)
			secondPipelined := !restrictSecondTask || preferred == f.nodes[1]
			f.waitForAttempts(since, preferred, secondPipelined, checkObservations)
			Consistently(checkObservations, 5*time.Second, 200*time.Millisecond).Should(Succeed())
		}
		for _, observation := range observations {
			observation.stop()
		}
		Expect(checkObservations()).To(Succeed())

		By("Confirming trial victims were never evicted")
		for _, original := range f.reservations {
			pod, err := f.ctx.Kubeclient.CoreV1().Pods(f.ctx.Namespace).Get(context.TODO(), original.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.UID).To(Equal(original.UID))
			Expect(pod.DeletionTimestamp).To(BeNil())
			Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
		}

		By("Releasing resources and allowing the blocked gang member to run")
		f.setNodeLabel(f.nodes[1], f.unblockKey, ptr.To("true"))
		for _, pod := range f.reservations {
			Expect(f.ctx.Kubeclient.CoreV1().Pods(f.ctx.Namespace).Delete(context.TODO(), pod.Name,
				metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))})).To(Succeed())
		}
		for _, original := range f.tasks {
			Expect(e2eutil.WaitPodReady(f.ctx, original)).To(Succeed())
			pod, err := f.ctx.Kubeclient.CoreV1().Pods(f.ctx.Namespace).Get(context.TODO(), original.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.UID).To(Equal(original.UID), "the same pending Pod must recover")
			Expect(pod.Spec.NodeName).NotTo(BeEmpty())
			Expect(e2eutil.IsPodScheduled(pod)).To(BeTrue())
		}
	},
		Entry("when candidate nodes change", false),
		Entry("when a task alternates between Pipeline and no Pipeline", true),
	)

	It("retains a committed nomination while an evicted Pod is terminating", func() {
		f := newNominationConditionFixture()
		var baseline corev1.PodStatus
		Eventually(func() bool {
			victim, err := f.ctx.Kubeclient.CoreV1().Pods(f.ctx.Namespace).Get(context.TODO(), f.victim.Name, metav1.GetOptions{})
			if err != nil || victim.DeletionTimestamp == nil {
				return false
			}
			preemptor, err := f.ctx.Kubeclient.CoreV1().Pods(f.ctx.Namespace).Get(context.TODO(), f.preemptor.Name, metav1.GetOptions{})
			if err != nil || preemptor.Status.NominatedNodeName != f.node {
				return false
			}
			for _, condition := range preemptor.Status.Conditions {
				if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse &&
					strings.Contains(condition.Message, "assigned to "+f.node) {
					baseline = *preemptor.Status.DeepCopy()
					return true
				}
			}
			return false
		}, e2eutil.TwoMinute, 200*time.Millisecond).Should(BeTrue(), "expected an actual eviction and a published nomination")

		By("Checking repeated scheduling cycles while the victim is still terminating")
		since := metav1.Now()
		Consistently(func() error {
			victim, err := f.ctx.Kubeclient.CoreV1().Pods(f.ctx.Namespace).Get(context.TODO(), f.victim.Name, metav1.GetOptions{})
			if err != nil || victim.DeletionTimestamp == nil {
				return fmt.Errorf("victim stopped terminating during observation: %v", err)
			}
			preemptor, err := f.ctx.Kubeclient.CoreV1().Pods(f.ctx.Namespace).Get(context.TODO(), f.preemptor.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if preemptor.Spec.NodeName != "" || !equality.Semantic.DeepEqual(baseline, preemptor.Status) {
				return fmt.Errorf("preemptor status changed while waiting for victim: before=%+v after=%+v", baseline, preemptor.Status)
			}
			return nil
		}, 12*time.Second, 200*time.Millisecond).Should(Succeed())
		f.waitForSessions(since, 3)

		By("Checking the same Pod binds once the victim exits")
		Expect(e2eutil.WaitPodReady(f.ctx, f.preemptor)).To(Succeed())
		bound, err := f.ctx.Kubeclient.CoreV1().Pods(f.ctx.Namespace).Get(context.TODO(), f.preemptor.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(bound.UID).To(Equal(f.preemptor.UID))
		Expect(bound.Spec.NodeName).To(Equal(f.node))
	})
})

type nominationConditionFixture struct {
	ctx       *e2eutil.TestContext
	scheduler corev1.Pod
	node      string
	victim    *corev1.Pod
	preemptor *corev1.Pod
}

func newNominationConditionFixture() *nominationConditionFixture {
	ctx := e2eutil.InitTestContext(e2eutil.Options{})
	DeferCleanup(func() {
		Expect(ctx.Kubeclient.CoreV1().Namespaces().Delete(context.TODO(), ctx.Namespace, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() bool {
			_, err := ctx.Kubeclient.CoreV1().Namespaces().Get(context.TODO(), ctx.Namespace, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, e2eutil.TwoMinute, time.Second).Should(BeTrue())
	})
	f := &nominationConditionFixture{ctx: ctx, scheduler: configurePipelineConditionScheduler(ctx)}
	priorityName := "nomination-" + ctx.Namespace
	_, err := ctx.Kubeclient.SchedulingV1().PriorityClasses().Create(context.TODO(), &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: priorityName}, Value: 100,
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		Expect(ctx.Kubeclient.SchedulingV1().PriorityClasses().Delete(context.TODO(), priorityName, metav1.DeleteOptions{})).To(Succeed())
	})

	nodes, freeCPU := pipelineAvailableCPU(ctx)
	for _, node := range nodes {
		if node.Spec.Unschedulable || len(node.Spec.Taints) != 0 || freeCPU[node.Name] < 800 {
			continue
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				f.node = node.Name
				break
			}
		}
		if f.node != "" {
			break
		}
	}
	Expect(f.node).NotTo(BeEmpty(), "need one ready worker with at least 800m CPU free")
	filler := e2eutil.CreatePod(ctx, e2eutil.PodSpec{
		Name: "filler", Node: f.node,
		Req: corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(freeCPU[f.node]-300, resource.DecimalSI)},
	})
	Expect(e2eutil.WaitPodReady(ctx, filler)).To(Succeed())
	for _, name := range []string{"victims", "preemptors"} {
		pg := &schedulingv1beta1.PodGroup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ctx.Namespace},
			Spec:       schedulingv1beta1.PodGroupSpec{MinMember: 1, Queue: e2eutil.DefaultQueue},
		}
		if name == "preemptors" {
			pg.Spec.PriorityClassName = priorityName
		}
		_, err := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Create(context.TODO(), pg, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}
	victim := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "victim", Namespace: ctx.Namespace,
			Labels:      map[string]string{schedulingv1beta1.PodPreemptable: "true"},
			Annotations: map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: "victims"}},
		Spec: corev1.PodSpec{
			NodeName: f.node, SchedulerName: e2eutil.SchedulerName,
			TerminationGracePeriodSeconds: ptr.To(int64(75)),
			Containers: []corev1.Container{{Name: "nginx", Image: e2eutil.DefaultNginxImage, ImagePullPolicy: corev1.PullIfNotPresent,
				Resources: corev1.ResourceRequirements{Requests: e2eutil.CPUResource("200m")},
				Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "sleep 60"}}}},
			}},
		},
	}
	f.victim, err = ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Create(context.TODO(), victim, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(e2eutil.WaitPodReady(ctx, f.victim)).To(Succeed())

	preemptor := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "preemptor", Namespace: ctx.Namespace,
			Annotations: map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: "preemptors"}},
		Spec: corev1.PodSpec{
			SchedulerName: e2eutil.SchedulerName, PriorityClassName: priorityName,
			Containers: []corev1.Container{{Name: "nginx", Image: e2eutil.DefaultNginxImage, ImagePullPolicy: corev1.PullIfNotPresent,
				Resources: corev1.ResourceRequirements{Requests: e2eutil.CPUResource("200m")}}},
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{
					Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{f.node},
				}}}},
			}}},
		},
	}
	f.preemptor, err = ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Create(context.TODO(), preemptor, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	return f
}

func (f *nominationConditionFixture) waitForSessions(since metav1.Time, minimum int) {
	Eventually(func() (int, error) {
		logs, err := f.ctx.Kubeclient.CoreV1().Pods(f.scheduler.Namespace).GetLogs(f.scheduler.Name,
			&corev1.PodLogOptions{SinceTime: &since, LimitBytes: ptr.To(int64(4 << 20))}).DoRaw(context.TODO())
		if err != nil {
			return 0, err
		}
		return strings.Count(string(logs), "Close Session "), nil
	}, e2eutil.TwoMinute, time.Second).Should(BeNumerically(">=", minimum))
}

type pipelineConditionFixture struct {
	ctx           *e2eutil.TestContext
	scheduler     corev1.Pod
	nodes         []string
	preferenceKey string
	unblockKey    string
	tasks         []*corev1.Pod
	reservations  []*corev1.Pod
}

func newPipelineConditionFixture(restrictSecondTask bool) *pipelineConditionFixture {
	ctx := e2eutil.InitTestContext(e2eutil.Options{})
	// CleanupTestContext deletes every HyperNode in the cluster. This fixture
	// creates none, so clean up only its namespace and explicitly owned resources.
	DeferCleanup(func() {
		Expect(ctx.Kubeclient.CoreV1().Namespaces().Delete(context.TODO(), ctx.Namespace, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() bool {
			_, err := ctx.Kubeclient.CoreV1().Namespaces().Get(context.TODO(), ctx.Namespace, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, e2eutil.TwoMinute, time.Second).Should(BeTrue())
	})
	f := &pipelineConditionFixture{
		ctx:           ctx,
		preferenceKey: "e2e.volcano.sh/prefer-" + ctx.Namespace,
		unblockKey:    "e2e.volcano.sh/unblock-" + ctx.Namespace,
	}
	priorityName := "pipeline-" + ctx.Namespace
	_, err := ctx.Kubeclient.SchedulingV1().PriorityClasses().Create(context.TODO(), &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: priorityName}, Value: 100,
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		Expect(ctx.Kubeclient.SchedulingV1().PriorityClasses().Delete(context.TODO(), priorityName, metav1.DeleteOptions{})).To(Succeed())
	})

	f.scheduler = configurePipelineConditionScheduler(ctx)

	nodes, freeCPU := pipelineAvailableCPU(ctx)
	for _, node := range nodes {
		if node.Spec.Unschedulable || len(node.Spec.Taints) != 0 || freeCPU[node.Name] < 800 {
			continue
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				f.nodes = append(f.nodes, node.Name)
			}
		}
	}
	sort.Strings(f.nodes)
	Expect(len(f.nodes)).To(BeNumerically(">=", 2), "need two ready workers with at least 800m CPU free")
	f.nodes = f.nodes[:2]
	DeferCleanup(func() {
		for _, node := range f.nodes {
			f.setNodeLabel(node, f.preferenceKey, nil)
			f.setNodeLabel(node, f.unblockKey, nil)
		}
	})
	f.preferNode(f.nodes[1])
	f.createPodGroup("victims", "", nil, false)
	for i, node := range f.nodes {
		// Each worker has 100m idle and 200m reclaimable. A 200m task fits
		// after preemption, but two such tasks cannot fit on the same worker.
		for _, reservation := range []struct {
			name  string
			cpu   int64
			group string
		}{
			{fmt.Sprintf("filler-%d", i), freeCPU[node] - 300, ""},
			{fmt.Sprintf("victim-%d", i), 200, "victims"},
		} {
			annotations := map[string]string{}
			if reservation.group != "" {
				annotations[schedulingv1beta1.KubeGroupNameAnnotationKey] = reservation.group
			}
			pod := e2eutil.CreatePod(ctx, e2eutil.PodSpec{
				Name: reservation.name, Node: node,
				Req:                           corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(reservation.cpu, resource.DecimalSI)},
				Annotations:                   annotations,
				Labels:                        map[string]string{schedulingv1beta1.PodPreemptable: fmt.Sprint(reservation.group != "")},
				TerminationGracePeriodSeconds: ptr.To(int64(0)),
			})
			f.reservations = append(f.reservations, pod)
			Expect(e2eutil.WaitPodReady(ctx, pod)).To(Succeed())
		}
	}
	_, freeCPU = pipelineAvailableCPU(ctx)
	var totalFree int64
	for _, free := range freeCPU {
		totalFree += max(free, 0)
	}
	// Force allocate to report one stable HyperNode minResource failure while
	// preempt can still trial individual tasks. Releasing the reservations later
	// satisfies this minimum without changing the PodGroup or Pod requests.
	minimum := corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(totalFree+400, resource.DecimalSI)}
	f.createPodGroup("targets", priorityName, &minimum, true)
	for i, name := range []string{"candidate-a", "candidate-b", "blocked"} {
		allowed := f.nodes
		if restrictSecondTask && i == 1 {
			allowed = f.nodes[:1]
		}
		var terms []corev1.NodeSelectorTerm
		for _, node := range allowed {
			terms = append(terms, corev1.NodeSelectorTerm{MatchFields: []corev1.NodeSelectorRequirement{{
				Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node},
			}}})
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ctx.Namespace,
				Labels: map[string]string{"e2e.volcano.sh/pipeline-target": ctx.Namespace},
				Annotations: map[string]string{
					schedulingv1beta1.KubeGroupNameAnnotationKey: "targets",
					"volcano.sh/task-priority":                   fmt.Sprint(103 - i),
				},
			},
			Spec: corev1.PodSpec{
				SchedulerName: e2eutil.SchedulerName, PriorityClassName: priorityName,
				TerminationGracePeriodSeconds: ptr.To(int64(0)),
				Containers: []corev1.Container{{Name: "nginx", Image: e2eutil.DefaultNginxImage, ImagePullPolicy: corev1.PullIfNotPresent,
					Resources: corev1.ResourceRequirements{Requests: e2eutil.CPUResource("200m")}}},
				Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: terms},
				}},
			},
		}
		if i == 0 {
			pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.PreferredSchedulingTerm{{
				Weight: 100, Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: f.preferenceKey, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"},
				}}},
			}}
		}
		if i == 2 {
			pod.Spec.NodeSelector = map[string]string{f.unblockKey: "true"}
			pod.Spec.Containers[0].Resources.Requests = e2eutil.CPUResource("100m")
		}
		created, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		f.tasks = append(f.tasks, created)
	}
	return f
}

func configurePipelineConditionScheduler(ctx *e2eutil.TestContext) corev1.Pod {
	namespace := os.Getenv("VOLCANO_E2E_NAMESPACE")
	if namespace == "" {
		namespace = "volcano-system"
	}
	schedulers, err := ctx.Kubeclient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: "app=volcano-scheduler"})
	Expect(err).NotTo(HaveOccurred())
	var active []corev1.Pod
	for _, pod := range schedulers.Items {
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning {
			active = append(active, pod)
		}
	}
	Expect(active).To(HaveLen(1), "the scheduling-condition fixture requires one scheduler with log verbosity >= 3")
	scheduler := active[0]
	var configMapName, configKey string
	for _, volume := range scheduler.Spec.Volumes {
		if volume.ConfigMap == nil {
			continue
		}
		cm, err := ctx.Kubeclient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), volume.ConfigMap.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		for key := range cm.Data {
			if strings.HasPrefix(key, "volcano-scheduler") && strings.HasSuffix(key, ".conf") {
				configMapName, configKey = cm.Name, key
			}
		}
	}
	Expect(configMapName).NotTo(BeEmpty(), "scheduler configuration must be mounted from a ConfigMap")
	config := e2eutil.NewConfigMapCase(namespace, configMapName)
	Expect(config.ChangeBy(func(data map[string]string) (bool, map[string]string) {
		old := data[configKey]
		data[configKey] = pipelineConditionSchedulerConfig
		return true, map[string]string{configKey: old}
	})).To(Succeed())
	DeferCleanup(func() { Expect(config.UndoChanged()).To(Succeed()) })

	return scheduler
}

func (f *pipelineConditionFixture) createPodGroup(name, priority string, minimum *corev1.ResourceList, subgroup bool) {
	pg := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ctx.Namespace},
		Spec:       schedulingv1beta1.PodGroupSpec{MinMember: 1, Queue: e2eutil.DefaultQueue, PriorityClassName: priority, MinResources: minimum},
	}
	if subgroup {
		pg.Spec.MinMember = 3
		pg.Spec.SubGroupPolicy = []schedulingv1beta1.SubGroupPolicySpec{{
			Name: "replica", SubGroupSize: ptr.To(int32(3)), MinSubGroups: ptr.To(int32(1)),
			MatchLabelKeys: []string{"e2e.volcano.sh/pipeline-target"},
		}}
	}
	_, err := f.ctx.Vcclient.SchedulingV1beta1().PodGroups(f.ctx.Namespace).Create(context.TODO(), pg, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func pipelineAvailableCPU(ctx *e2eutil.TestContext) ([]corev1.Node, map[string]int64) {
	nodes, err := ctx.Kubeclient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	free := make(map[string]int64, len(nodes.Items))
	for _, node := range nodes.Items {
		free[node.Name] = node.Status.Allocatable.Cpu().MilliValue()
	}
	pods, err := ctx.Kubeclient.CoreV1().Pods(metav1.NamespaceAll).List(context.TODO(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		requests := resourcehelper.PodRequests(&pod, resourcehelper.PodResourcesOptions{})
		free[pod.Spec.NodeName] -= requests.Cpu().MilliValue()
	}
	return nodes.Items, free
}

func (f *pipelineConditionFixture) setNodeLabel(node, key string, value *string) {
	patch, err := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]*string{key: value}}})
	Expect(err).NotTo(HaveOccurred())
	_, err = f.ctx.Kubeclient.CoreV1().Nodes().Patch(context.TODO(), node, types.MergePatchType, patch, metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func (f *pipelineConditionFixture) preferNode(preferred string) {
	for _, node := range f.nodes {
		f.setNodeLabel(node, f.preferenceKey, ptr.To(fmt.Sprint(node == preferred)))
	}
}

// Count complete sessions using existing V(3) transaction logs, so the same
// test also runs against the unfixed scheduler. Requiring both Pipeline and
// UnPipeline prevents an idle or misconfigured scheduler from passing.
func (f *pipelineConditionFixture) waitForAttempts(since metav1.Time, firstNode string, secondPipelined bool, observe func() error) {
	Eventually(func() (int, error) {
		if observe != nil {
			Expect(observe()).To(Succeed())
		}
		logs, err := f.ctx.Kubeclient.CoreV1().Pods(f.scheduler.Namespace).GetLogs(f.scheduler.Name,
			&corev1.PodLogOptions{SinceTime: &since, LimitBytes: ptr.To(int64(4 << 20))}).DoRaw(context.TODO())
		if err != nil {
			return 0, err
		}
		completed := 0
		var session string
		pipelined, rolledBack := map[string]string{}, map[string]string{}
		for _, line := range strings.Split(string(logs), "\n") {
			if _, rest, ok := strings.Cut(line, "Open Session "); ok {
				session = strings.Fields(rest)[0]
				pipelined, rolledBack = map[string]string{}, map[string]string{}
			}
			for _, operation := range []struct {
				marker string
				nodes  map[string]string
			}{
				{"After pipelined Task <", pipelined}, {"After unpipelined Task <", rolledBack},
			} {
				if _, rest, ok := strings.Cut(line, operation.marker+f.ctx.Namespace+"/"); ok {
					name, rest, ok := strings.Cut(rest, "> to Node <")
					if ok {
						node, _, _ := strings.Cut(rest, ">")
						operation.nodes[name] = node
					}
				}
			}
			if session != "" && strings.Contains(line, "Close Session "+session) {
				second := pipelined["candidate-b"] != ""
				if pipelined["candidate-a"] == firstNode && rolledBack["candidate-a"] == firstNode &&
					second == secondPipelined && (!second || rolledBack["candidate-b"] == pipelined["candidate-b"]) {
					completed++
				}
				session = ""
			}
		}
		return completed, nil
	}, e2eutil.TwoMinute, time.Second).Should(BeNumerically(">=", 3), "expected repeated discarded Pipeline attempts on %s, second task pipelined=%v (scheduler verbosity must be >= 3)", firstNode, secondPipelined)
}

type pipelineStatusObservation struct {
	watch    watch.Interface
	done     chan struct{}
	errors   chan error
	stopping chan struct{}
	stopOnce sync.Once
}

func (f *pipelineConditionFixture) observeStatus(taskName string) *pipelineStatusObservation {
	options := metav1.ListOptions{
		LabelSelector: "e2e.volcano.sh/pipeline-target=" + f.ctx.Namespace,
		FieldSelector: "metadata.name=" + taskName,
	}
	pods, err := f.ctx.Kubeclient.CoreV1().Pods(f.ctx.Namespace).List(context.TODO(), options)
	Expect(err).NotTo(HaveOccurred())
	Expect(pods.Items).To(HaveLen(1))
	baseline := make(map[types.UID]corev1.PodStatus, len(pods.Items))
	for _, pod := range pods.Items {
		Expect(pod.Spec.NodeName).To(BeEmpty())
		Expect(pod.Status.NominatedNodeName).To(BeEmpty())
		Expect(pod.Status.Phase).To(Equal(corev1.PodPending))
		found := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled {
				found = true
				Expect(condition.Status).To(Equal(corev1.ConditionFalse))
				Expect(condition.Reason).To(Equal(corev1.PodReasonUnschedulable))
				Expect(condition.Message).To(ContainSubstring("last observed candidate node"))
				Expect(condition.Message).To(ContainSubstring("HyperNode: 0/"))
				Expect(condition.Message).To(ContainSubstring("minResource"))
			}
		}
		Expect(found).To(BeTrue(), "missing scheduling condition on %s", pod.Name)
		baseline[pod.UID] = *pod.Status.DeepCopy()
	}
	options.ResourceVersion = pods.ResourceVersion
	stream, err := f.ctx.Kubeclient.CoreV1().Pods(f.ctx.Namespace).Watch(context.TODO(), options)
	Expect(err).NotTo(HaveOccurred())
	o := &pipelineStatusObservation{
		watch: stream, done: make(chan struct{}), errors: make(chan error, 1), stopping: make(chan struct{}),
	}
	go func() {
		defer close(o.done)
		for event := range stream.ResultChan() {
			if event.Type == watch.Error {
				// Stop closes the HTTP response body and can produce a final decode
				// error. Actual watch errors during observation must still fail.
				select {
				case <-o.stopping:
					return
				default:
				}
			}
			if event.Type == watch.Bookmark {
				continue
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok || event.Type == watch.Deleted {
				o.errors <- fmt.Errorf("unexpected Pod watch event %s: %v", event.Type, event.Object)
				return
			}
			previous, exists := baseline[pod.UID]
			if !exists || pod.Spec.NodeName != "" || !equality.Semantic.DeepEqual(previous, pod.Status) {
				o.errors <- fmt.Errorf("Pod %s status changed during discarded attempts: before=%+v after=%+v", pod.Name, previous, pod.Status)
				return
			}
		}
		select {
		case <-o.stopping:
		default:
			o.errors <- fmt.Errorf("Pod watch closed before observation completed")
		}
	}()
	return o
}

func (o *pipelineStatusObservation) err() error {
	select {
	case err := <-o.errors:
		o.errors <- err
		return err
	default:
	}
	return nil
}

func (o *pipelineStatusObservation) stop() {
	o.stopOnce.Do(func() {
		close(o.stopping)
		o.watch.Stop()
	})
	<-o.done
}
