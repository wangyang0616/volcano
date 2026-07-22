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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	repackinformers "volcano.sh/apis/pkg/client/informers/externalversions/repack/v1alpha1"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
	"volcano.sh/repack-controller/pkg/placement"
)

const (
	podGroupIndexName           = "repack.volcano.sh/pod-group"
	placementGateOwnerIndexName = "repack.volcano.sh/placement-gate-owner"
	nominationVictimIndexName   = "repack.volcano.sh/nomination-victim"
)

// Nominator is the placement-steering reconciler: it watches Pods and, for a not-
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
	kubernetesClient        kubernetes.Interface
	volcanoClient           vcclientset.Interface
	podLister               corelisters.PodLister
	repackRunLister         repacklisters.RepackRunLister
	podIndexer              cache.Indexer
	repackRunIndexer        cache.Indexer
	podGroupIndexAvailable  bool
	gateOwnerIndexAvailable bool
	victimIndexAvailable    bool
	informerSyncs           []cache.InformerSynced
	workQueue               workqueue.TypedRateLimitingInterface[string]
	recorder                record.EventRecorder
	now                     func() time.Time
}

const (
	eventReasonReplacementGated    = "RepackReplacementGated"
	eventReasonPlacementNominated  = "RepackPlacementNominated"
	eventReasonPlacementSucceeded  = "RepackPlacementSucceeded"
	eventReasonPlacementDrifted    = "RepackPlacementDrifted"
	eventReasonPlacementGateOpened = "RepackPlacementGateOpened"
	eventReasonPlacementReleased   = "RepackPlacementReleased"
)

// NewEventRecorder creates a Pod event recorder for the placement protocol.
// Pod events complement RepackRun events: operators can diagnose why a concrete
// replacement Pod is gated, nominated, drifted, or released from kubectl describe.
func NewEventRecorder(kubernetesClient kubernetes.Interface, component string) record.EventRecorder {
	if kubernetesClient == nil {
		return nil
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubernetesClient.CoreV1().Events("")})
	return broadcaster.NewRecorder(kubescheme.Scheme, corev1.EventSource{Component: component})
}

// SetEventRecorder enables Kubernetes events. It is optional so unit tests and
// embedders that do not need events retain a lightweight constructor.
func (n *Nominator) SetEventRecorder(recorder record.EventRecorder) {
	n.recorder = recorder
}

func (n *Nominator) recordPodEvent(pod *corev1.Pod, eventType, reason, message string) {
	if n == nil || n.recorder == nil || pod == nil {
		return
	}
	n.recorder.Event(pod, eventType, reason, message)
}

// NewNominator wires the reconciler to Pod and RepackRun informers. Watching
// both sides is important: a replacement Pod can be observed before the
// prepared nomination status reaches this controller's informer. A later
// RepackRun update must therefore wake the already-existing Pending Pod.
func NewNominator(kubernetesClient kubernetes.Interface, volcanoClient vcclientset.Interface, podInformer coreinformers.PodInformer, repackRunInformer repackinformers.RepackRunInformer) *Nominator {
	podGroupIndexAvailable := true
	gateOwnerIndexAvailable := true
	if err := podInformer.Informer().AddIndexers(cache.Indexers{
		podGroupIndexName:           podGroupIndex,
		placementGateOwnerIndexName: placementGateOwnerIndex,
	}); err != nil {
		klog.ErrorS(err, "repack nominator: add Pod indexes; falling back to namespace scan")
		podGroupIndexAvailable = false
		gateOwnerIndexAvailable = false
	}
	victimIndexAvailable := true
	if err := repackRunInformer.Informer().AddIndexers(cache.Indexers{nominationVictimIndexName: nominationVictimIndex}); err != nil {
		klog.ErrorS(err, "repack nominator: add nomination victim index; falling back to RepackRun scan")
		victimIndexAvailable = false
	}
	n := &Nominator{
		kubernetesClient:        kubernetesClient,
		volcanoClient:           volcanoClient,
		podLister:               podInformer.Lister(),
		repackRunLister:         repackRunInformer.Lister(),
		podIndexer:              podInformer.Informer().GetIndexer(),
		repackRunIndexer:        repackRunInformer.Informer().GetIndexer(),
		podGroupIndexAvailable:  podGroupIndexAvailable,
		gateOwnerIndexAvailable: gateOwnerIndexAvailable,
		victimIndexAvailable:    victimIndexAvailable,
		informerSyncs:           []cache.InformerSynced{podInformer.Informer().HasSynced, repackRunInformer.Informer().HasSynced},
		workQueue:               workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		now:                     time.Now,
	}
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    n.enqueue,
		UpdateFunc: func(_, newObj interface{}) { n.enqueue(newObj) },
		DeleteFunc: n.enqueueAfterVictimDeleted,
	})
	repackRunInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    n.enqueuePendingForRun,
		UpdateFunc: func(_, newObj interface{}) { n.enqueuePendingForRun(newObj) },
		DeleteFunc: n.enqueueGatedPodsForDeletedRun,
	})
	return n
}

func (n *Nominator) enqueue(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	if pod.Spec.NodeName != "" {
		if pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != "" {
			key, err := cache.MetaNamespaceKeyFunc(pod)
			if err == nil {
				n.workQueue.Add(key)
			}
		}
		return
	}
	// A Repack-owned gate/owner marker represents an in-flight protocol that must
	// be reconciled to completion even after nominatedNodeName has been written.
	// In particular, a conflict or restart during status persistence, Pod status
	// patching, or gate removal must not make the Pod disappear from this controller.
	if hasPlacementGate(pod) || pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != "" {
		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		n.workQueue.Add(key)
		return
	}
	if !needsNomination(pod) {
		return // already nominated, terminating, or not Pending
	}
	key, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	n.workQueue.Add(key)
}

func podGroupIndexKey(namespace, podGroupName string) string {
	return namespace + "\x00" + podGroupName
}

func nominationVictimIndexKey(namespace, victimPodName string) string {
	return namespace + "\x00" + victimPodName
}

func podGroupIndex(obj interface{}) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	podGroupName := placement.PodGroupName(pod)
	if podGroupName == "" {
		return nil, nil
	}
	return []string{podGroupIndexKey(pod.Namespace, podGroupName)}, nil
}

func placementGateOwnerIndex(obj interface{}) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	owner := pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation]
	if owner == "" {
		return nil, nil
	}
	return []string{owner}, nil
}

func nominationVictimIndex(obj interface{}) ([]string, error) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok {
		return nil, nil
	}
	keys := make([]string, 0, len(run.Status.Nominations))
	for index := range run.Status.Nominations {
		nomination := &run.Status.Nominations[index]
		if placementConsumed(nomination) || nomination.Phase == repackv1alpha1.PodPlacementExpired || nomination.VictimPodName == "" {
			continue
		}
		keys = append(keys, nominationVictimIndexKey(nomination.Namespace, nomination.VictimPodName))
	}
	return keys, nil
}

func (n *Nominator) enqueuePodByName(namespace, podName string) {
	if namespace == "" || podName == "" {
		return
	}
	pod, err := n.podLister.Pods(namespace).Get(podName)
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	n.enqueue(pod)
}

// enqueuePendingForRun closes the informer-ordering race: when nomination
// intents become visible, revisit Pending Pods that may already have emitted
// their Add event. The PodGroup index limits this to pods that can actually
// match an active nomination, rather than scanning an entire namespace on every
// status update.
func (n *Nominator) enqueuePendingForRun(obj interface{}) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok {
		return
	}
	n.enqueueGatedPodsForOwner(placement.OwnerValue(run.Name, run.UID))
	if !placementRunActive(run) {
		return
	}
	podGroups := map[string]bool{}
	namespaces := map[string]bool{}
	for index := range run.Status.Nominations {
		nomination := &run.Status.Nominations[index]
		if !placementConsumed(nomination) && nomination.Phase != repackv1alpha1.PodPlacementExpired {
			namespaces[nomination.Namespace] = true
			if nomination.PodGroupName != "" {
				podGroups[podGroupIndexKey(nomination.Namespace, nomination.PodGroupName)] = true
			}
			n.enqueuePodByName(nomination.Namespace, nomination.VictimPodName)
		}
	}
	for podGroupKey := range podGroups {
		if !n.podGroupIndexAvailable || n.podIndexer == nil {
			continue
		}
		objects, err := n.podIndexer.ByIndex(podGroupIndexName, podGroupKey)
		if err != nil {
			utilruntime.HandleError(err)
			continue
		}
		for _, object := range objects {
			n.enqueue(object)
		}
	}
	klog.V(4).InfoS("repack nominator: nomination update enqueued candidate Pods", "run", run.Name,
		"activeNominationPodGroups", len(podGroups), "podGroupIndexEnabled", n.podGroupIndexAvailable,
		"namespaceFallbackCount", len(namespaces))
	if !n.podGroupIndexAvailable {
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
}

func (n *Nominator) enqueueGatedPodsForDeletedRun(obj interface{}) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok {
		if tombstone, tombstoneOK := obj.(cache.DeletedFinalStateUnknown); tombstoneOK {
			run, ok = tombstone.Obj.(*repackv1alpha1.RepackRun)
		}
	}
	if !ok || run == nil {
		return
	}
	n.enqueueGatedPodsForOwner(placement.OwnerValue(run.Name, run.UID))
}

func (n *Nominator) enqueueGatedPodsForOwner(owner string) {
	if owner == "" {
		return
	}
	if n.gateOwnerIndexAvailable && n.podIndexer != nil {
		objects, err := n.podIndexer.ByIndex(placementGateOwnerIndexName, owner)
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		for _, object := range objects {
			n.enqueue(object)
		}
		return
	}
	if n.podLister == nil {
		return
	}
	pods, err := n.podLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	for _, pod := range pods {
		if pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] == owner {
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
	if n.victimIndexAvailable && n.repackRunIndexer != nil {
		objects, err := n.repackRunIndexer.ByIndex(nominationVictimIndexName, nominationVictimIndexKey(pod.Namespace, pod.Name))
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		for _, object := range objects {
			n.enqueuePendingForRun(object)
		}
		klog.V(4).InfoS("repack nominator: victim deletion matched nomination runs by index",
			"namespace", pod.Namespace, "victimPod", pod.Name, "matchedRunCount", len(objects))
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
				!placementConsumed(nomination) && nomination.Phase != repackv1alpha1.PodPlacementExpired {
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
	klog.V(3).InfoS("Starting repack nominator", "workers", workers,
		"podGroupIndexEnabled", n.podGroupIndexAvailable, "gateOwnerIndexEnabled", n.gateOwnerIndexAvailable,
		"victimIndexEnabled", n.victimIndexAvailable)
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
		klog.V(4).InfoS("repack nominator: reconcile failed; requeueing with rate limit",
			"pod", key, "retryCount", n.workQueue.NumRequeues(key)+1, "error", err)
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
	klog.V(4).InfoS("repack nominator: reconciling Pod",
		"pod", key, "phase", pod.Status.Phase, "nodeName", pod.Spec.NodeName,
		"nominatedNodeName", pod.Status.NominatedNodeName,
		"podGroup", placement.PodGroupName(pod),
		"placementGate", hasPlacementGate(pod),
		"gateOwner", pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation])
	if pod.Spec.NodeName != "" {
		if err := n.observePlacement(ctx, pod); err != nil {
			return err
		}
		return n.clearPlacementGate(ctx, pod)
	}
	hasGate := hasPlacementGate(pod)
	gateOwner := pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation]
	if !hasGate && gateOwner == "" {
		// Pods outside the placement protocol have no durable owner from which to
		// recover a nomination. Ordinary already-nominated Pods are not ours.
		return nil
	}

	run, err := n.placementRunForPod(ctx, pod)
	if err != nil {
		return err
	}
	if run == nil || !placementRunActive(run) {
		klog.V(4).InfoS("repack nominator: releasing placement metadata because owning Run is absent or inactive",
			"pod", key, "gateOwner", gateOwner)
		return n.clearPlacementGate(ctx, pod)
	}

	// Prefer the durable concrete association even for Nominated records. Generic
	// matching deliberately treats Nominated as consumed, but an interrupted
	// Nominated -> gate-open transition must be resumed rather than forgotten.
	nomination := nominationForReplacement(run, pod)
	if nomination != nil {
		switch nomination.Phase {
		case repackv1alpha1.PodPlacementPlaced,
			repackv1alpha1.PodPlacementDegraded,
			repackv1alpha1.PodPlacementExpired,
			repackv1alpha1.PodNominationBound:
			return n.clearPlacementGate(ctx, pod)
		}
	}

	if !hasGate {
		if placementAwaitingBinding(run, pod) {
			return nil
		}
		return n.clearPlacementGate(ctx, pod)
	}

	owningRunName := run.Name
	if nomination == nil {
		nomination, owningRunName = n.matchNominationInRuns(pod, []*repackv1alpha1.RepackRun{run})
	}
	if nomination == nil {
		if pendingPlacementForPodGroup(run, pod, n.now()) {
			// Deployment/ReplicaSet can create its replacement while the victim
			// remains Terminating. At this point it is indistinguishable from a
			// concurrent scale-out Pod, so retain the owner-marked gate until
			// victim deletion makes matching authoritative.
			klog.V(4).InfoS("repack nominator: retaining ambiguous PodGroup gate until victim deletion",
				"pod", key, "run", run.Name, "podGroup", placement.PodGroupName(pod))
			return nil
		}
		klog.V(4).InfoS("repack nominator: no active nomination matches Pod; releasing placement gate",
			"pod", key, "run", run.Name, "podGroup", placement.PodGroupName(pod))
		return n.clearPlacementGate(ctx, pod)
	}
	klog.V(4).InfoS("repack nominator: matched replacement Pod to placement intent",
		"pod", key, "run", owningRunName, "podGroup", nomination.PodGroupName,
		"victimPod", nomination.VictimPodName, "plannedNode", nomination.NodeName,
		"selectedNode", nomination.SelectedNodeName, "identityLabels", nomination.IdentityLabels)
	if nomination.SelectedNodeName == "" {
		// The admission webhook has stopped the scheduler. Hand the placement
		// decision to the engine, which owns the scheduler session and can choose
		// a current receiver without duplicating predicate logic here.
		return n.markPlacementGated(ctx, owningRunName, pod)
	}

	selectedNode := nomination.SelectedNodeName
	// Persist the concrete association and selected receiver before mutating the
	// Pod. Every following operation is then recoverable from Run status after a
	// conflict, process restart, or partial API failure.
	if err := n.markPlacementNominated(ctx, owningRunName, nomination, pod, selectedNode); err != nil {
		return err
	}
	if pod.Status.NominatedNodeName != selectedNode {
		if err := n.patchNominatedNode(ctx, pod, selectedNode); err != nil {
			return err
		}
	}
	if err := n.openPlacementGate(ctx, pod); err != nil {
		return err
	}
	klog.V(3).InfoS("repack placement nominated replacement pod", "pod", key, "node", selectedNode, "repackRun", owningRunName)
	n.recordPodEvent(pod, corev1.EventTypeNormal, eventReasonPlacementNominated,
		fmt.Sprintf("RepackRun %s selected node %s for this replacement Pod.", owningRunName, selectedNode))
	return nil
}

// nominationForReplacement returns the durable placement record already claimed
// by this concrete replacement. Unlike generic matching, it intentionally sees
// Nominated records so an interrupted status-patch/gate-open sequence can resume.
func nominationForReplacement(run *repackv1alpha1.RepackRun, pod *corev1.Pod) *repackv1alpha1.PodNomination {
	if run == nil || pod == nil || pod.UID == "" {
		return nil
	}
	for index := range run.Status.Nominations {
		nomination := &run.Status.Nominations[index]
		if nomination.Namespace == pod.Namespace && nomination.ReplacementPodName == pod.Name &&
			nomination.ReplacementPodUID == pod.UID {
			return nomination
		}
	}
	return nil
}

func pendingPlacementForPodGroup(run *repackv1alpha1.RepackRun, pod *corev1.Pod, now time.Time) bool {
	if !placementRunActive(run) || pod == nil {
		return false
	}
	podGroupName := placement.PodGroupName(pod)
	for i := range run.Status.Nominations {
		nomination := &run.Status.Nominations[i]
		if nomination.Namespace != pod.Namespace || nomination.PodGroupName != podGroupName || placementConsumed(nomination) || nomination.Phase == repackv1alpha1.PodPlacementExpired {
			continue
		}
		if nomination.ExpirationTime == nil || !now.After(nomination.ExpirationTime.Time) {
			return true
		}
	}
	return false
}

func placementRunActive(run *repackv1alpha1.RepackRun) bool {
	return run != nil && run.Spec.Mode == repackv1alpha1.RepackModeExecute &&
		run.Status.Phase == repackv1alpha1.RepackRunning
}

// needsNomination is true for a pod that is unscheduled, not yet nominated, and
// still Pending — the only pods a nomination can usefully steer.
func needsNomination(pod *corev1.Pod) bool {
	return pod.Spec.NodeName == "" &&
		pod.Status.NominatedNodeName == "" &&
		pod.Status.Phase == corev1.PodPending &&
		pod.DeletionTimestamp == nil
}

// Match precedence — the placement identity contract (§5.2.2):
//  1. victimPodName exact: nomination.VictimPodName equals the pod's name (same-name
//     rebuild — vcjob/StatefulSet/kthena ordinals);
//  2. identityLabels: same namespace+PodGroup and the pod's labels are a superset
//     of nomination.IdentityLabels (renamed replacement; the recorded label key+value
//     say exactly how to match — e.g. repack.volcano.sh/pod-identity=worker-3);
//  3. fungible: nomination.IdentityLabels empty — any pending pod in the same PodGroup
//     (single-role Deployment/ReplicaSet/Job).
func (n *Nominator) matchNominationInRuns(pod *corev1.Pod, runs []*repackv1alpha1.RepackRun) (*repackv1alpha1.PodNomination, string) {
	if pod == nil {
		return nil, ""
	}
	now := n.now()
	podGroupName := placement.PodGroupName(pod)
	var fungibleNomination *repackv1alpha1.PodNomination
	var fungibleRunName string
	for _, run := range runs {
		if !placementRunActive(run) {
			continue
		}
		for index := range run.Status.Nominations {
			nomination := &run.Status.Nominations[index]
			if placementConsumed(nomination) || nomination.Phase == repackv1alpha1.PodPlacementExpired {
				continue
			}
			if nomination.ReplacementPodName != "" &&
				(nomination.ReplacementPodName != pod.Name || nomination.ReplacementPodUID != pod.UID) {
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

// placementRunForPod reads only the Run recorded by the gate owner annotation.
// The direct read closes informer ordering windows without scanning or matching
// unrelated Runs.
func (n *Nominator) placementRunForPod(ctx context.Context, pod *corev1.Pod) (*repackv1alpha1.RepackRun, error) {
	if n.volcanoClient == nil || pod == nil {
		return nil, nil
	}
	runName, runUID, ok := placement.ParseOwner(
		pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation])
	if !ok {
		return nil, nil
	}
	run, err := n.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if run.UID != runUID {
		return nil, nil
	}
	return run, nil
}

func placementAwaitingBinding(run *repackv1alpha1.RepackRun, pod *corev1.Pod) bool {
	if run == nil || pod == nil {
		return false
	}
	for i := range run.Status.Nominations {
		nomination := &run.Status.Nominations[i]
		if nomination.Namespace == pod.Namespace && nomination.ReplacementPodName == pod.Name &&
			nomination.ReplacementPodUID == pod.UID && nomination.Phase == repackv1alpha1.PodPlacementNominated {
			return true
		}
	}
	return false
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

// openPlacementGate removes only Repack's scheduling gate after the selected
// receiver is durable. The owner marker remains until actual binding is
// observed, so scheduled Pod updates can find their Run without global scans.
func (n *Nominator) openPlacementGate(ctx context.Context, pod *corev1.Pod) error {
	return n.patchPlacementGate(ctx, pod, false)
}

// clearPlacementGate removes both Repack's gate and owner marker on terminal,
// stale, unrelated, or already-observed paths.
func (n *Nominator) clearPlacementGate(ctx context.Context, pod *corev1.Pod) error {
	return n.patchPlacementGate(ctx, pod, true)
}

func (n *Nominator) patchPlacementGate(ctx context.Context, pod *corev1.Pod, removeOwner bool) error {
	if pod == nil || (!hasPlacementGate(pod) &&
		(!removeOwner || pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] == "")) {
		return nil
	}
	expectedOwner := pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation]
	patched := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := n.kubernetesClient.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if latest.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != expectedOwner {
			return nil
		}
		operations := make([]string, 0, 2)
		for index := range latest.Spec.SchedulingGates {
			if latest.Spec.SchedulingGates[index].Name != repackv1alpha1.PlacementGateName {
				continue
			}
			operations = append(operations, fmt.Sprintf(`{"op":"remove","path":"/spec/schedulingGates/%d"}`, index))
			break
		}
		if removeOwner && latest.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != "" {
			operations = append(operations, `{"op":"remove","path":"/metadata/annotations/repack.volcano.sh~1placement-gate-owner"}`)
		}
		if len(operations) == 0 {
			return nil
		}
		body := []byte("[" + strings.Join(operations, ",") + "]")
		_, err = n.kubernetesClient.CoreV1().Pods(latest.Namespace).Patch(ctx, latest.Name, types.JSONPatchType, body, metav1.PatchOptions{})
		patched = err == nil
		return ignoreNotFound(err)
	})
	if err != nil || !patched {
		return err
	}
	if removeOwner {
		klog.V(3).InfoS("repack nominator: released placement gate and owner marker",
			"pod", pod.Namespace+"/"+pod.Name, "gateOwner", expectedOwner)
		n.recordPodEvent(pod, corev1.EventTypeNormal, eventReasonPlacementReleased,
			fmt.Sprintf("Released the Repack placement gate owned by %s.", expectedOwner))
	} else {
		klog.V(4).InfoS("repack nominator: opened placement gate after nomination became durable",
			"pod", pod.Namespace+"/"+pod.Name, "gateOwner", expectedOwner)
		n.recordPodEvent(pod, corev1.EventTypeNormal, eventReasonPlacementGateOpened,
			"Opened the Repack placement gate after persisting the selected receiver.")
	}
	return nil
}

func hasPlacementGate(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == repackv1alpha1.PlacementGateName {
			return true
		}
	}
	return false
}

// markPlacementNominated records the concrete replacement before opening its
// gate. This makes the later bound-node observation unambiguous even for
// fungible native workloads.
func (n *Nominator) markPlacementNominated(ctx context.Context, runName string, nomination *repackv1alpha1.PodNomination, pod *corev1.Pod, selectedNode string) error {
	if runName == "" || nomination == nil || pod == nil {
		return fmt.Errorf("cannot persist replacement nomination without Run, placement, and Pod identity")
	}
	key := nominationStatusKey(nomination)
	durable := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		run, err := n.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			// Do not open the gate when the owning Run vanished between lookup and
			// status persistence. A fresh reconcile will release stale metadata.
			return err
		}
		for index := range run.Status.Nominations {
			current := &run.Status.Nominations[index]
			if nominationStatusKey(current) != key {
				continue
			}
			if placementConsumed(current) {
				durable = current.Phase == repackv1alpha1.PodPlacementNominated &&
					current.SelectedNodeName == selectedNode &&
					current.ReplacementPodName == pod.Name && current.ReplacementPodUID == pod.UID
				return nil
			}
			current.SelectedNodeName = selectedNode
			current.ReplacementPodName = pod.Name
			current.ReplacementPodUID = pod.UID
			current.Phase = repackv1alpha1.PodPlacementNominated
			_, err = n.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
			durable = err == nil
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !durable {
		return fmt.Errorf("replacement placement %q in RepackRun %q changed before nomination became durable", key, runName)
	}
	return nil
}

func (n *Nominator) markPlacementGated(ctx context.Context, runName string, pod *corev1.Pod) error {
	if runName == "" || pod == nil {
		return nil
	}
	updated := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		run, err := n.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			return ignoreNotFound(err)
		}
		current, _ := n.matchNominationInRuns(pod, []*repackv1alpha1.RepackRun{run})
		if current == nil {
			return nil
		}
		current.ReplacementPodName = pod.Name
		current.ReplacementPodUID = pod.UID
		current.Phase = repackv1alpha1.PodPlacementGated
		_, err = n.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
		updated = err == nil
		return err
	})
	if err != nil || !updated {
		return err
	}
	klog.V(3).InfoS("repack nominator: replacement Pod associated with placement intent and held by gate",
		"run", runName, "pod", pod.Namespace+"/"+pod.Name,
		"podGroup", placement.PodGroupName(pod), "podUID", pod.UID)
	n.recordPodEvent(pod, corev1.EventTypeNormal, eventReasonReplacementGated,
		fmt.Sprintf("RepackRun %s is holding this replacement Pod while selecting live receiver capacity.", runName))
	return nil
}

// observePlacement records the scheduler's actual binding. A selected node is a
// soft preference by design; if the scheduler has to choose elsewhere the run
// remains live and the drift is surfaced as Degraded rather than pinning the Pod.
func (n *Nominator) observePlacement(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil || pod.Spec.NodeName == "" {
		return nil
	}
	cachedRun, err := n.placementRunForPod(ctx, pod)
	if err != nil {
		return err
	}
	if cachedRun == nil {
		return nil
	}
	observedPhase := repackv1alpha1.PodNominationPhase("")
	selectedNode := ""
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		run, err := n.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, cachedRun.Name, metav1.GetOptions{})
		if err != nil {
			return ignoreNotFound(err)
		}
		for index := range run.Status.Nominations {
			nomination := &run.Status.Nominations[index]
			if nomination.Namespace != pod.Namespace || nomination.ReplacementPodName != pod.Name || nomination.ReplacementPodUID != pod.UID {
				continue
			}
			nomination.ActualNodeName = pod.Spec.NodeName
			selectedNode = nomination.SelectedNodeName
			switch nomination.Phase {
			case repackv1alpha1.PodPlacementNominated:
				if nomination.SelectedNodeName == pod.Spec.NodeName {
					nomination.Phase = repackv1alpha1.PodPlacementPlaced
				} else {
					nomination.Phase = repackv1alpha1.PodPlacementDegraded
				}
			case repackv1alpha1.PodPlacementGated, repackv1alpha1.PodPlacementAwaitingCapacity:
				// Another actor bypassed the gate before the engine selected a
				// receiver. Record the actual node but never claim controlled
				// placement success.
				nomination.Phase = repackv1alpha1.PodPlacementDegraded
			default:
				return nil
			}
			observedPhase = nomination.Phase
			_, err = n.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
			return err
		}
		return nil
	})
	if err != nil || observedPhase == "" {
		return err
	}
	if observedPhase == repackv1alpha1.PodPlacementPlaced {
		klog.V(3).InfoS("repack nominator: replacement Pod reached selected receiver",
			"run", cachedRun.Name, "pod", pod.Namespace+"/"+pod.Name, "node", pod.Spec.NodeName)
		n.recordPodEvent(pod, corev1.EventTypeNormal, eventReasonPlacementSucceeded,
			fmt.Sprintf("Replacement Pod reached Repack-selected node %s.", pod.Spec.NodeName))
	} else {
		klog.V(3).InfoS("repack nominator: replacement Pod placement drift detected",
			"run", cachedRun.Name, "pod", pod.Namespace+"/"+pod.Name,
			"selectedNode", selectedNode, "actualNode", pod.Spec.NodeName, "phase", observedPhase)
		n.recordPodEvent(pod, corev1.EventTypeWarning, eventReasonPlacementDrifted,
			fmt.Sprintf("Replacement Pod bound to %s instead of Repack-selected node %s.", pod.Spec.NodeName, selectedNode))
	}
	return nil
}

func placementConsumed(nomination *repackv1alpha1.PodNomination) bool {
	if nomination == nil {
		return true
	}
	switch nomination.Phase {
	case repackv1alpha1.PodPlacementNominated, repackv1alpha1.PodPlacementPlaced,
		repackv1alpha1.PodPlacementDegraded, repackv1alpha1.PodPlacementExpired,
		repackv1alpha1.PodNominationBound:
		return true
	default:
		return false
	}
}

func nominationStatusKey(nomination *repackv1alpha1.PodNomination) string {
	if nomination == nil {
		return ""
	}
	return nomination.Namespace + "\x00" + nomination.PodGroupName + "\x00" + nomination.VictimPodName + "\x00" + nomination.NodeName
}
