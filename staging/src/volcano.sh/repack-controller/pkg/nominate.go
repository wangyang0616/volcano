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

package repackcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	repackinformers "volcano.sh/apis/pkg/client/informers/externalversions/repack/v1alpha1"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
)

// Nomination phases (mirror the CRD enum on PodNomination.Phase).
const (
	nomPending = "Pending"
	nomBound   = "Bound"
	nomExpired = "Expired"
)

// podGroupAnnotationKey is the pod annotation carrying its PodGroup name; the reconciler
// matches identityLabels generically, so it needs no per-workload identity key.
const podGroupAnnotationKey = "scheduling.k8s.io/group-name"

// Nominator is the landing-steering reconciler: it watches Pods and, for a not-
// yet-scheduled replacement pod, looks up a matching PodNomination in some
// RepackRun.status.nominations[] and writes pod.status.nominatedNodeName so the
// scheduler prefers the repack-recommended node. It is best-effort and soft — a
// nomination is only a hint; if the node has since filled, the scheduler is free
// to place the pod elsewhere (no reservation; §4.7.1.2).
//
// Why here and not the PodGroup controller: the injector must cover every gang
// kind (vcjob/Deployment/StatefulSet/RawPod), must own the status.nominations[]
// lifecycle, and is conceptually part of the repack control loop — so it lives
// with the RepackRun controller, decoupled from any single workload controller.
type Nominator struct {
	kubernetesClient kubernetes.Interface
	volcanoClient    vcclientset.Interface
	podLister        corelisters.PodLister
	repackRunLister  repacklisters.RepackRunLister
	informerSyncs    []cache.InformerSynced
	workQueue        workqueue.TypedRateLimitingInterface[string]
	now              func() time.Time
}

// NewNominator wires the reconciler to Pod and RepackRun informers. Watching
// both sides is important: a replacement Pod can be observed before the
// prepared nomination status reaches this controller's informer. A later
// RepackRun update must therefore wake the already-existing Pending Pod.
func NewNominator(kubernetesClient kubernetes.Interface, volcanoClient vcclientset.Interface, podInformer coreinformers.PodInformer, repackRunInformer repackinformers.RepackRunInformer) *Nominator {
	n := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    volcanoClient,
		podLister:        podInformer.Lister(),
		repackRunLister:  repackRunInformer.Lister(),
		informerSyncs:    []cache.InformerSynced{podInformer.Informer().HasSynced, repackRunInformer.Informer().HasSynced},
		workQueue:        workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		now:              time.Now,
	}
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    n.enqueue,
		UpdateFunc: func(_, newObj interface{}) { n.enqueue(newObj) },
		DeleteFunc: n.enqueueAfterVictimDeleted,
	})
	repackRunInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    n.enqueuePendingForRun,
		UpdateFunc: func(_, newObj interface{}) { n.enqueuePendingForRun(newObj) },
	})
	return n
}

func (n *Nominator) enqueue(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	if !needsNomination(pod) {
		return // already scheduled or already nominated — skip cheaply
	}
	key, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	n.workQueue.Add(key)
}

// enqueuePendingForRun closes the informer-ordering race: when nomination
// intents become visible, revisit Pending Pods that may already have emitted
// their Add event. RepackRun updates are rare, and the scan is restricted to the
// namespaces referenced by active nominations.
func (n *Nominator) enqueuePendingForRun(obj interface{}) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok {
		return
	}
	namespaces := map[string]bool{}
	for index := range run.Status.Nominations {
		nomination := &run.Status.Nominations[index]
		if nomination.Phase != nomBound && nomination.Phase != nomExpired {
			namespaces[nomination.Namespace] = true
		}
	}
	for namespace := range namespaces {
		pods, err := n.podLister.Pods(namespace).List(labels.Everything())
		if err != nil {
			utilruntime.HandleError(err)
			continue
		}
		for _, pod := range pods {
			n.enqueue(pod)
		}
	}
}

// enqueueAfterVictimDeleted wakes a renamed/fungible replacement that was held
// back while the original victim still existed. Exact-name replacements are
// naturally handled by their own Add event.
func (n *Nominator) enqueueAfterVictimDeleted(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		if tombstone, tombstoneOK := obj.(cache.DeletedFinalStateUnknown); tombstoneOK {
			pod, ok = tombstone.Obj.(*corev1.Pod)
		}
	}
	if !ok || pod == nil {
		return
	}
	runs, err := n.repackRunLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	for _, run := range runs {
		for index := range run.Status.Nominations {
			nomination := &run.Status.Nominations[index]
			if nomination.Namespace == pod.Namespace && nomination.VictimPodName == pod.Name &&
				nomination.Phase != nomBound && nomination.Phase != nomExpired {
				n.enqueuePendingForRun(run)
				return
			}
		}
	}
}

// Run launches the reconciler until ctx is cancelled. The caller starts the
// informer factories.
func (n *Nominator) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer n.workQueue.ShutDown()
	if !cache.WaitForCacheSync(ctx.Done(), n.informerSyncs...) {
		return fmt.Errorf("nominator: cache failed to sync")
	}
	if workers < 1 {
		workers = 1
	}
	klog.V(3).InfoS("Starting repack nominator", "workers", workers)
	for i := 0; i < workers; i++ {
		go func() {
			for n.processNext(ctx) {
			}
		}()
	}
	<-ctx.Done()
	return nil
}

func (n *Nominator) processNext(ctx context.Context) bool {
	key, shutdown := n.workQueue.Get()
	if shutdown {
		return false
	}
	defer n.workQueue.Done(key)
	if err := n.reconcile(ctx, key); err != nil {
		utilruntime.HandleError(fmt.Errorf("nominate pod %q: %w", key, err))
		n.workQueue.AddRateLimited(key)
		return true
	}
	n.workQueue.Forget(key)
	return true
}

func (n *Nominator) reconcile(ctx context.Context, key string) error {
	namespace, podName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil
	}
	pod, err := n.podLister.Pods(namespace).Get(podName)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !needsNomination(pod) {
		return nil
	}

	nomination, owningRunName := n.matchNomination(pod)
	if nomination == nil {
		klog.V(5).InfoS("no pending nomination matches this pod", "pod", key)
		return nil // no pending nomination targets this pod
	}

	if err := n.patchNominatedNode(ctx, pod, nomination.NodeName); err != nil {
		return err
	}
	klog.V(3).InfoS("nominated replacement pod", "pod", key, "node", nomination.NodeName, "repackRun", owningRunName)
	// Best-effort: mark the record Bound so it is consumed once. A failure here
	// only risks a redundant (idempotent) re-nomination, so it is not fatal.
	if err := n.markBound(ctx, owningRunName, nomination); err != nil {
		klog.V(4).InfoS("could not mark nomination Bound (will retry on next event)", "err", err)
	}
	return nil
}

// needsNomination is true for a pod that is unscheduled, not yet nominated, and
// still Pending — the only pods a nomination can usefully steer.
func needsNomination(pod *corev1.Pod) bool {
	return pod.Spec.NodeName == "" &&
		pod.Status.NominatedNodeName == "" &&
		pod.Status.Phase == corev1.PodPending &&
		pod.DeletionTimestamp == nil
}

// matchNomination scans every RepackRun's pending, unexpired nominations for one
// that targets this pod, returning the record and the owning run's name.
//
// Match precedence — the landing-identity contract (§5.2.2):
//  1. victimPodName exact: nomination.VictimPodName equals the pod's name (same-name
//     rebuild — vcjob/StatefulSet/kthena ordinals);
//  2. identityLabels: same namespace+PodGroup and the pod's labels are a superset
//     of nomination.IdentityLabels (renamed replacement; the recorded label key+value
//     say exactly how to match — e.g. repack.volcano.sh/pod-identity=worker-3);
//  3. fungible: nomination.IdentityLabels empty — any pending pod in the same PodGroup
//     (single-role Deployment/ReplicaSet/Job).
//
// TODO(#46): when a native-kind pod exposes its identity only via env / ordinal
// name (not a label), have the engine record the equivalent label key here so the
// generic superset match still applies.
func (n *Nominator) matchNomination(pod *corev1.Pod) (*repackv1alpha1.PodNomination, string) {
	runs, err := n.repackRunLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(err)
		return nil, ""
	}
	now := n.now()
	podGroupName := pod.Annotations[podGroupAnnotationKey]
	var fungibleNomination *repackv1alpha1.PodNomination
	var fungibleRunName string
	for _, run := range runs {
		for index := range run.Status.Nominations {
			nomination := &run.Status.Nominations[index]
			if nomination.Phase == nomBound || nomination.Phase == nomExpired {
				continue
			}
			if nomination.ExpirationTime != nil && now.After(nomination.ExpirationTime.Time) {
				continue
			}
			// 1. exact victim name wins immediately (globally unique in namespace).
			if nomination.VictimPodName != "" && nomination.Namespace == pod.Namespace && nomination.VictimPodName == pod.Name {
				return nomination, run.Name
			}
			// identity / fungible require same namespace + PodGroup.
			if nomination.Namespace != pod.Namespace || nomination.PodGroupName == "" || nomination.PodGroupName != podGroupName {
				continue
			}
			if len(nomination.IdentityLabels) > 0 {
				// 2. label-superset identity match.
				if labelsMatch(pod.Labels, nomination.IdentityLabels) && n.victimGone(nomination) {
					return nomination, run.Name
				}
				continue
			}
			// 3. fungible: first pending record for this PodGroup.
			// Do not consume it while the original victim still exists: prepared
			// nominations are persisted before eviction, and a failed eviction must
			// not redirect an unrelated Pending gang member.
			if !n.victimGone(nomination) {
				continue
			}
			if fungibleNomination == nil {
				fungibleNomination, fungibleRunName = nomination, run.Name
			}
		}
	}
	return fungibleNomination, fungibleRunName
}

func (n *Nominator) victimGone(nomination *repackv1alpha1.PodNomination) bool {
	if nomination == nil || nomination.VictimPodName == "" || n.podLister == nil {
		return true
	}
	_, err := n.podLister.Pods(nomination.Namespace).Get(nomination.VictimPodName)
	return apierrors.IsNotFound(err)
}

// labelsMatch reports whether podLabels is a superset of want (all want entries
// present with equal values). Empty want matches anything.
func labelsMatch(podLabels, want map[string]string) bool {
	for k, v := range want {
		if podLabels[k] != v {
			return false
		}
	}
	return true
}

// patchNominatedNode writes pod.status.nominatedNodeName via the status
// subresource (a soft hint; the scheduler may still place elsewhere).
func (n *Nominator) patchNominatedNode(ctx context.Context, pod *corev1.Pod, node string) error {
	patch := map[string]interface{}{"status": map[string]interface{}{"nominatedNodeName": node}}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = n.kubernetesClient.CoreV1().Pods(pod.Namespace).Patch(
		ctx, pod.Name, types.StrategicMergePatchType, body, metav1.PatchOptions{}, "status")
	return ignoreNotFound(err)
}

// markBound flips the matched record to Bound on the owning RepackRun so it is
// consumed once. Re-reads the run to avoid clobbering a concurrent status write.
func (n *Nominator) markBound(ctx context.Context, owningRunName string, targetNomination *repackv1alpha1.PodNomination) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Use an API read, not the informer lister: a stale cached status could
		// otherwise overwrite the engine's just-written terminal result.
		updated, err := n.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, owningRunName, metav1.GetOptions{})
		if err != nil {
			return ignoreNotFound(err)
		}
		changed := false
		for index := range updated.Status.Nominations {
			nomination := &updated.Status.Nominations[index]
			if nomination.Namespace == targetNomination.Namespace && nomination.PodGroupName == targetNomination.PodGroupName &&
				nomination.VictimPodName == targetNomination.VictimPodName && nomination.NodeName == targetNomination.NodeName {
				if nomination.Phase != nomBound {
					nomination.Phase = nomBound
					changed = true
				}
			}
		}
		if !changed {
			return nil
		}
		_, err = n.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, updated, metav1.UpdateOptions{})
		return ignoreNotFound(err)
	})
}
