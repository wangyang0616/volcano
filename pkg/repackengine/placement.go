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

package repackengine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	"volcano.sh/repack-controller/pkg/placement"
	state "volcano.sh/repack-controller/pkg/state"

	"volcano.sh/volcano/pkg/repackengine/adapter"
	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
)

// preparePlacementLeases marks every affected PodGroup before any victim is
// evicted. The Pod mutating webhook reads this lease and injects a scheduling
// gate into a subsequently-created replacement Pod. The lease value contains
// both name and UID so a stale PodGroup annotation cannot accidentally attach a
// new RepackRun with the same name.
func (e *Engine) preparePlacementLeases(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	if run == nil || run.Spec.Mode != repackv1alpha1.RepackModeExecute {
		return nil
	}
	lease := placement.OwnerValue(run.Name, run.UID)
	groups := placementPodGroups(run)
	klog.V(4).InfoS("repack: preparing PodGroup placement leases",
		"run", run.Name, "podGroupCount", len(groups), "lease", lease)
	for podGroupKey := range groups {
		namespace, podGroupName := podGroupKey.Namespace, podGroupKey.Name
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			podGroup, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Get(ctx, podGroupName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			current := podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation]
			if current != "" && current != lease {
				active, err := e.placementLeaseActive(ctx, current, namespace, podGroupName)
				if err != nil {
					return err
				}
				if active {
					return fmt.Errorf("PodGroup %s/%s already has active placement lease %q", namespace, podGroupName, current)
				}
				klog.InfoS("repack: replacing stale placement lease", "podGroup", namespace+"/"+podGroupName, "staleLease", current, "newLease", lease)
			}
			if current == lease {
				return nil
			}
			podGroup = podGroup.DeepCopy()
			if podGroup.Annotations == nil {
				podGroup.Annotations = map[string]string{}
			}
			podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] = lease
			_, err = e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Update(ctx, podGroup, metav1.UpdateOptions{})
			return err
		}); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("prepare placement lease: PodGroup %s/%s no longer exists", namespace, podGroupName)
			}
			return err
		}
		klog.V(4).InfoS("repack: PodGroup placement lease prepared",
			"run", run.Name, "podGroup", namespace+"/"+podGroupName)
	}
	return nil
}

// placementLeaseActive distinguishes a live Execute from an annotation left
// behind by a deleted Run, a terminal Run, or an interrupted cleanup. Lease
// ownership includes the UID, so reusing a Run name is never treated as live.
func (e *Engine) placementLeaseActive(ctx context.Context, lease, namespace, podGroupName string) (bool, error) {
	runName, runUID, ok := placement.ParseOwner(lease)
	if !ok {
		return false, nil
	}
	run, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil || run.UID != runUID || run.Spec.Mode != repackv1alpha1.RepackModeExecute {
		return false, err
	}
	return placement.ActiveForPodGroup(run, namespace, podGroupName), nil
}

func placementPodGroups(run *repackv1alpha1.RepackRun) map[types.NamespacedName]struct{} {
	groups := make(map[types.NamespacedName]struct{})
	if run == nil {
		return groups
	}
	for i := range run.Status.Nominations {
		nomination := &run.Status.Nominations[i]
		if nomination.Namespace != "" && nomination.PodGroupName != "" {
			groups[types.NamespacedName{Namespace: nomination.Namespace, Name: nomination.PodGroupName}] = struct{}{}
		}
		if nomination.Namespace != "" && nomination.ReplacementPodGroupName != "" {
			groups[types.NamespacedName{Namespace: nomination.Namespace, Name: nomination.ReplacementPodGroupName}] = struct{}{}
		}
	}
	return groups
}

func placementGroupsDifference(
	all, retain map[types.NamespacedName]struct{},
) map[types.NamespacedName]struct{} {
	result := make(map[types.NamespacedName]struct{})
	for key := range all {
		if _, stillNeeded := retain[key]; !stillNeeded {
			result[key] = struct{}{}
		}
	}
	return result
}

type placementLeaseReleaseOutcome int

const (
	placementLeaseReleased placementLeaseReleaseOutcome = iota
	placementLeaseAlreadyAbsent
	placementLeasePodGroupNotFound
	placementLeaseNotOwned
)

// releasePlacementLeases removes only annotations owned by this Run. It is safe
// to call on every terminal/error path: a newer Run's lease is never touched.
func (e *Engine) releasePlacementLeases(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	groups map[types.NamespacedName]struct{},
) error {
	if run == nil || len(groups) == 0 {
		return nil
	}
	lease := placement.OwnerValue(run.Name, run.UID)
	releasedCount, alreadyAbsentCount, notFoundCount, notOwnedCount := 0, 0, 0, 0
	for podGroupKey := range groups {
		namespace, podGroupName := podGroupKey.Namespace, podGroupKey.Name
		outcome := placementLeaseAlreadyAbsent
		observedLease := ""
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			outcome = placementLeaseAlreadyAbsent
			observedLease = ""
			podGroup, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Get(ctx, podGroupName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				outcome = placementLeasePodGroupNotFound
				return nil
			}
			if err != nil {
				return err
			}
			observedLease = podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation]
			if observedLease == "" {
				outcome = placementLeaseAlreadyAbsent
				return nil
			}
			if observedLease != lease {
				outcome = placementLeaseNotOwned
				return nil
			}
			podGroup = podGroup.DeepCopy()
			delete(podGroup.Annotations, repackv1alpha1.PlacementLeaseAnnotation)
			_, err = e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Update(ctx, podGroup, metav1.UpdateOptions{})
			if err == nil {
				outcome = placementLeaseReleased
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("release placement lease for PodGroup %s/%s: %w", namespace, podGroupName, err)
		}
		switch outcome {
		case placementLeaseReleased:
			releasedCount++
			klog.V(4).InfoS("repack: PodGroup placement lease released",
				"run", run.Name, "podGroup", namespace+"/"+podGroupName)
		case placementLeaseAlreadyAbsent:
			alreadyAbsentCount++
			klog.V(4).InfoS("repack: PodGroup placement lease was already absent",
				"run", run.Name, "podGroup", namespace+"/"+podGroupName)
		case placementLeasePodGroupNotFound:
			notFoundCount++
			klog.V(4).InfoS("repack: PodGroup already deleted during placement lease cleanup",
				"run", run.Name, "podGroup", namespace+"/"+podGroupName)
		case placementLeaseNotOwned:
			notOwnedCount++
			klog.V(3).InfoS("repack: PodGroup placement lease belongs to another owner; cleanup skipped",
				"run", run.Name, "podGroup", namespace+"/"+podGroupName, "observedLease", observedLease)
		}
	}
	klog.V(3).InfoS("repack: placement lease cleanup completed",
		"run", run.Name, "requestedPodGroupCount", len(groups),
		"releasedCount", releasedCount, "alreadyAbsentCount", alreadyAbsentCount,
		"notFoundCount", notFoundCount, "notOwnedCount", notOwnedCount)
	return nil
}

// cleanupPlacement removes engine-owned PodGroup leases. Pod gate lifecycle is
// owned exclusively by the nomination controller, which watches terminal and
// deleted Runs through the gate-owner Pod index.
func (e *Engine) cleanupPlacement(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	groups, err := e.ownedPlacementLeaseGroups(ctx, run)
	if err != nil {
		return err
	}
	if err := e.releasePlacementLeases(ctx, run, groups); err != nil {
		return err
	}
	return e.setPlacementActive(ctx, run, false)
}

// ownedPlacementLeaseGroups includes both status-recorded groups and admission-
// time candidate groups that were never claimed by a nomination (for example a
// concurrent scale-out). Terminal cleanup must not leave those Pods gated.
func (e *Engine) ownedPlacementLeaseGroups(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
) (map[types.NamespacedName]struct{}, error) {
	groups := placementPodGroups(run)
	if run == nil || e.volcanoClient == nil {
		return groups, nil
	}
	lease := placement.OwnerValue(run.Name, run.UID)
	namespaces := map[string]struct{}{}
	if run.Status.Plan != nil {
		for index := range run.Status.Plan.Moves {
			if namespace := run.Status.Plan.Moves[index].Namespace; namespace != "" {
				namespaces[namespace] = struct{}{}
			}
		}
	}
	for namespace := range namespaces {
		podGroups, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list PodGroups in namespace %s for placement cleanup: %w", namespace, err)
		}
		for index := range podGroups.Items {
			podGroup := &podGroups.Items[index]
			if podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] == lease {
				groups[types.NamespacedName{Namespace: namespace, Name: podGroup.Name}] = struct{}{}
			}
		}
	}
	return groups, nil
}

// setPlacementActive maintains the metadata index used by the PodGroup webhook.
// The label is not authoritative: webhooks still validate phase, Run UID, owner,
// creation time, and unfinished nominations.
func (e *Engine) setPlacementActive(ctx context.Context, run *repackv1alpha1.RepackRun, active bool) error {
	if run == nil || e.volcanoClient == nil {
		return nil
	}
	value := interface{}(nil)
	if active {
		value = "true"
	}
	body, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				repackv1alpha1.PlacementActiveLabel: value,
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = e.volcanoClient.RepackV1alpha1().RepackRuns().Patch(
		ctx, run.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("set placement-active=%t on RepackRun %s: %w", active, run.Name, err)
	}
	klog.V(4).InfoS("repack: placement discovery label reconciled",
		"run", run.Name, "active", active)
	return nil
}

const placementLeaseRepairInterval = 30 * time.Second

// repairRecreatedPodGroupLeasesIfDue is the eventual-consistency fallback for a
// PodGroup CREATE that bypassed or raced admission mutation. The webhook remains
// the primary barrier; this independently rate-limited repair protects later
// Pods without placing namespace-wide LIST load on every placement retry.
func (e *Engine) repairRecreatedPodGroupLeasesIfDue(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	if run == nil || run.Status.Plan == nil {
		return nil
	}
	pendingWorkload := false
	for workload := range placement.SourcePodGroupsByWorkload(run) {
		if placement.HasPendingPlacementsForWorkload(run, workload) {
			pendingWorkload = true
			break
		}
	}
	if !pendingWorkload || !e.placementLeaseRepairDue(run) {
		return nil
	}

	lease := placement.OwnerValue(run.Name, run.UID)
	namespaces := map[string]struct{}{}
	for index := range run.Status.Plan.Moves {
		namespaces[run.Status.Plan.Moves[index].Namespace] = struct{}{}
	}
	scannedPodGroupCount, repairedLeaseCount, conflictingLeaseCount := 0, 0, 0
	klog.V(4).InfoS("repack: scanning for recreated PodGroups that missed admission-time placement lease",
		"run", run.Name, "namespaceCount", len(namespaces))
	for namespace := range namespaces {
		podGroups, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		scannedPodGroupCount += len(podGroups.Items)
		for index := range podGroups.Items {
			podGroup := &podGroups.Items[index]
			if !placement.PlacementAppliesToPodGroup(run, podGroup) ||
				podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] == lease {
				continue
			}
			if current := podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation]; current != "" {
				conflictingLeaseCount++
				klog.V(3).InfoS("repack: recreated PodGroup has a different placement lease; repair skipped",
					"run", run.Name, "podGroup", namespace+"/"+podGroup.Name, "lease", current)
				continue
			}
			repaired := false
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Get(ctx, podGroup.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if latest.Annotations[repackv1alpha1.PlacementLeaseAnnotation] != "" {
					return nil
				}
				latest = latest.DeepCopy()
				if latest.Annotations == nil {
					latest.Annotations = map[string]string{}
				}
				latest.Annotations[repackv1alpha1.PlacementLeaseAnnotation] = lease
				_, err = e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Update(ctx, latest, metav1.UpdateOptions{})
				repaired = err == nil
				return err
			}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("repair placement lease for PodGroup %s/%s: %w", namespace, podGroup.Name, err)
			}
			if !repaired {
				continue
			}
			repairedLeaseCount++
			klog.V(3).InfoS("repack: repaired placement lease on recreated PodGroup",
				"run", run.Name, "podGroup", namespace+"/"+podGroup.Name, "lease", lease)
			e.recordRunEvent(run, v1.EventTypeNormal, eventReasonPlacementLeaseRepaired,
				fmt.Sprintf("Repaired placement lease on recreated PodGroup %s/%s.", namespace, podGroup.Name))
		}
	}
	klog.V(4).InfoS("repack: recreated PodGroup placement lease repair scan completed",
		"run", run.Name, "namespaceCount", len(namespaces),
		"scannedPodGroupCount", scannedPodGroupCount,
		"repairedLeaseCount", repairedLeaseCount,
		"conflictingLeaseCount", conflictingLeaseCount)
	return nil
}

func (e *Engine) placementLeaseRepairDue(run *repackv1alpha1.RepackRun) bool {
	if run == nil {
		return false
	}
	now := time.Now()
	if e.now != nil {
		now = e.now()
	}
	runIdentity := run.Name + "/" + string(run.UID)
	e.placementLeaseRepairMutex.Lock()
	defer e.placementLeaseRepairMutex.Unlock()
	if e.placementLeaseRepairRunIdentity == runIdentity &&
		now.Before(e.lastPlacementLeaseRepairTime.Add(placementLeaseRepairInterval)) {
		klog.V(5).InfoS("repack: recreated PodGroup lease repair scan rate-limited",
			"run", run.Name, "lastRepairTime", e.lastPlacementLeaseRepairTime,
			"nextRepairTime", e.lastPlacementLeaseRepairTime.Add(placementLeaseRepairInterval))
		return false
	}
	e.placementLeaseRepairRunIdentity = runIdentity
	e.lastPlacementLeaseRepairTime = now
	return true
}

const placementRetryInterval = 2 * time.Second

// reconcilePlacement is the engine-owned dynamic placement decision. The
// controller only reports gated replacement Pods; this method opens the same
// scheduler session used by planning and chooses a current, immediately-idle
// receiver while excluding the nodes this Run is trying to free.
func (e *Engine) reconcilePlacement(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	if run == nil {
		return nil
	}
	placed, drifted, expiredCount := placementOutcomeCounts(run)
	klog.V(4).InfoS("repack: reconciling replacement placement",
		"run", run.Name, "nominationCount", len(run.Status.Nominations),
		"placedCount", placed, "driftedCount", drifted, "expiredCount", expiredCount)
	if err := e.repairRecreatedPodGroupLeasesIfDue(ctx, run); err != nil {
		return fmt.Errorf("reconcile recreated PodGroup leases: %w", err)
	}
	if expired, err := e.expirePlacements(ctx, run); err != nil {
		return err
	} else if expired {
		e.workQueue.Add(run.Name)
		return nil
	}
	if placementsComplete(run) {
		return e.finishPlacement(ctx, run)
	}
	pending := placementCandidates(run)
	if len(pending) == 0 {
		// A replacement controller may need time to create the Pod. Keep polling
		// until the durable deadline so an absent replacement cannot bypass the
		// expiration escape hatch.
		e.workQueue.AddAfter(run.Name, placementRetryInterval)
		klog.V(4).InfoS("repack: no selectable replacement Pod observed yet; placement requeued",
			"run", run.Name, "retryAfter", placementRetryInterval)
		return nil
	}

	targetResource := e.resolveResource(run)
	schedulerSession := schedframework.OpenSession(e.schedulerCache, e.tiers, e.configurations)
	defer schedframework.CloseSessionReadOnly(schedulerSession)
	scope, err := engineframework.NewScopeMatcher(run.Spec.Scope, adapter.SessionGangScopeLookup(schedulerSession))
	if err != nil {
		return err
	}
	snapshot := adapter.NewSessionSnapshot(schedulerSession, targetResource, scope)
	excludedFreedNodes := realizedFreedNodeNames(run)
	klog.V(4).InfoS("repack: evaluating live placement receivers",
		"run", run.Name, "candidateCount", len(pending), "snapshotNodeCount", len(snapshot.Nodes()),
		"excludedFreedNodes", excludedFreedNodes)
	committed := make([]*engineapi.Move, 0, len(pending))
	selected := make(map[placementIdentity]string, len(pending))
	for _, nomination := range pending {
		pod, err := e.schedulerCache.Client().CoreV1().Pods(nomination.Namespace).Get(ctx, nomination.ReplacementPodName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if pod.UID != nomination.ReplacementPodUID || pod.Spec.NodeName != "" {
			continue
		}
		// The replacement is a live Pod and has not been bound yet; constructing a
		// scheduler TaskInfo from it preserves its current resource requests and
		// scheduling constraints for the full predicate simulation below.
		task := schedapi.NewTaskInfo(pod)
		receivers := placementReceivers(snapshot.Nodes(), excludedFreedNodes, nomination.NodeName, task)
		klog.V(4).InfoS("repack: replacement receiver candidates evaluated",
			"run", run.Name, "pod", nomination.Namespace+"/"+nomination.ReplacementPodName,
			"plannedNode", nomination.NodeName, "receiverCount", len(receivers))
		placements, fit := snapshot.FeasibleRelocation(committed, []*schedapi.TaskInfo{task}, receivers)
		if !fit || len(placements) != 1 {
			klog.V(3).InfoS("repack: replacement is awaiting receiver capacity",
				"run", run.Name, "pod", nomination.Namespace+"/"+nomination.ReplacementPodName,
				"plannedNode", nomination.NodeName, "receiverCount", len(receivers))
			return e.markAwaitingPlacement(ctx, run.Name, pending)
		}
		committed = append(committed, placements[0])
		selected[placementIdentityForNomination(nomination)] = placements[0].To
		klog.V(4).InfoS("repack: replacement receiver selected in scheduler simulation",
			"run", run.Name, "pod", nomination.Namespace+"/"+nomination.ReplacementPodName,
			"plannedNode", nomination.NodeName, "selectedNode", placements[0].To)
	}
	if len(selected) == 0 {
		e.workQueue.AddAfter(run.Name, placementRetryInterval)
		return nil
	}
	return e.writePlacementSelection(ctx, run.Name, selected)
}

func placementCandidates(run *repackv1alpha1.RepackRun) []*repackv1alpha1.PodNomination {
	result := make([]*repackv1alpha1.PodNomination, 0)
	for index := range run.Status.Nominations {
		nomination := &run.Status.Nominations[index]
		if nomination.ReplacementPodName == "" || nomination.ReplacementPodUID == "" || nomination.SelectedNodeName != "" {
			continue
		}
		if nomination.Phase == repackv1alpha1.PodPlacementGated || nomination.Phase == repackv1alpha1.PodPlacementAwaitingCapacity {
			result = append(result, nomination)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return placementIdentityForNomination(result[i]).less(placementIdentityForNomination(result[j]))
	})
	return result
}

func placementReceivers(nodes []*schedapi.NodeInfo, freedNodes []string, plannedNode string, task *schedapi.TaskInfo) []*schedapi.NodeInfo {
	freed := make(map[string]struct{}, len(freedNodes))
	for _, node := range freedNodes {
		freed[node] = struct{}{}
	}
	byName := make(map[string]*schedapi.NodeInfo, len(nodes))
	for _, node := range nodes {
		if node != nil {
			byName[node.Name] = node
		}
	}
	receivers := make([]*schedapi.NodeInfo, 0, len(nodes))
	appendIfImmediatelyIdle := func(node *schedapi.NodeInfo) {
		if node == nil {
			return
		}
		if _, excluded := freed[node.Name]; excluded || !task.InitResreq.LessEqual(node.Idle, schedapi.Zero) {
			return
		}
		receivers = append(receivers, node)
	}
	appendIfImmediatelyIdle(byName[plannedNode])
	names := make([]string, 0, len(byName))
	for name := range byName {
		if name != plannedNode {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		appendIfImmediatelyIdle(byName[name])
	}
	return receivers
}

func (e *Engine) writePlacementSelection(
	ctx context.Context,
	runName string,
	selected map[placementIdentity]string,
) error {
	var updatedRun *repackv1alpha1.RepackRun
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		run, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		for index := range run.Status.Nominations {
			nomination := &run.Status.Nominations[index]
			if node, found := selected[placementIdentityForNomination(nomination)]; found && nomination.SelectedNodeName == "" {
				nomination.SelectedNodeName = node
				changed = true
			}
		}
		if !changed {
			return nil
		}
		updatedRun, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
		return err
	})
	if err != nil || updatedRun == nil {
		return err
	}
	klog.V(3).InfoS("repack: live replacement receivers persisted",
		"run", runName, "selectionCount", len(selected))
	e.recordRunEvent(updatedRun, v1.EventTypeNormal, eventReasonPlacementSelected,
		fmt.Sprintf("Selected live receiver nodes for %d replacement Pods.", len(selected)))
	return nil
}

func (e *Engine) markAwaitingPlacement(ctx context.Context, runName string, nominations []*repackv1alpha1.PodNomination) error {
	keys := make(map[placementIdentity]struct{}, len(nominations))
	for _, nomination := range nominations {
		keys[placementIdentityForNomination(nomination)] = struct{}{}
	}
	var updatedRun *repackv1alpha1.RepackRun
	placementStateChanged := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		run, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		for index := range run.Status.Nominations {
			nomination := &run.Status.Nominations[index]
			if _, found := keys[placementIdentityForNomination(nomination)]; found && nomination.SelectedNodeName == "" && nomination.Phase != repackv1alpha1.PodPlacementAwaitingCapacity {
				nomination.Phase = repackv1alpha1.PodPlacementAwaitingCapacity
				changed = true
				placementStateChanged = true
			}
		}
		if state.SetCondition(
			&run.Status.Conditions,
			state.CondProgressing,
			metav1.ConditionTrue,
			state.ReasonAwaitingPlacement,
			placementProgressMessage(run, e.resolveResource(run)),
			run.Generation,
		) {
			changed = true
		}
		if !changed {
			return nil
		}
		updatedRun, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
		return err
	})
	if err == nil {
		e.workQueue.AddAfter(runName, placementRetryInterval)
	}
	if err == nil && placementStateChanged && updatedRun != nil {
		message := placementProgressMessage(updatedRun, e.resolveResource(updatedRun))
		klog.V(3).InfoS("repack: replacement placement waiting for capacity",
			"run", runName, "pendingCount", len(nominations), "retryAfter", placementRetryInterval)
		e.recordRunEvent(updatedRun, v1.EventTypeWarning, eventReasonPlacementAwaitingCapacity, message)
	}
	return err
}

func placementsComplete(run *repackv1alpha1.RepackRun) bool {
	if len(run.Status.Nominations) == 0 {
		return false
	}
	for index := range run.Status.Nominations {
		switch run.Status.Nominations[index].Phase {
		case repackv1alpha1.PodPlacementPlaced, repackv1alpha1.PodPlacementDegraded, repackv1alpha1.PodPlacementExpired:
		default:
			return false
		}
	}
	return true
}

func (e *Engine) finishPlacement(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	expired := false
	metricsUnverified := false
	for index := range run.Status.Nominations {
		if run.Status.Nominations[index].Phase == repackv1alpha1.PodPlacementExpired {
			expired = true
		}
	}
	targetResource := e.resolveResource(run)
	if expired {
		// An expired replacement has been released to normal scheduling but has
		// not produced a trustworthy terminal binding. Do not claim the optimistic
		// plan benefit while workload demand may be temporarily absent.
		markExecuteBenefitUnverified(run)
	} else {
		schedulerSession := schedframework.OpenSession(e.schedulerCache, e.tiers, e.configurations)
		nodes := adapter.NewSessionSnapshot(schedulerSession, targetResource, nil).Nodes()
		visible := placementBindingsVisible(nodes, run.Status.Nominations)
		if !visible {
			schedframework.CloseSessionReadOnly(schedulerSession)
			// The nomination controller may observe Pod binding just before the
			// scheduler cache applies the same Pod update. Wait for one coherent
			// snapshot before publishing cluster-wide actual metrics.
			if !placementObservationDeadlinePassed(run, e.now()) {
				e.workQueue.AddAfter(run.Name, placementRetryInterval)
				return nil
			}
			metricsUnverified = true
			markExecuteBenefitUnverified(run)
		} else {
			updateActualExecuteResult(run, nodes, targetResource)
			schedframework.CloseSessionReadOnly(schedulerSession)
		}
	}

	decision := evaluatePlacementTerminal(run, metricsUnverified)
	message := placementStatusMessage(run, targetResource, decision)
	result := run.Status.Result
	resultMetrics := runResult(run)
	placedCount, driftedCount, expiredPlacementCount := placementOutcomeCounts(run)
	klog.V(3).InfoS("repack: replacement placement terminal result evaluated",
		"run", run.Name, "succeeded", decision.Succeeded, "reason", decision.Reason,
		"metricsUnverified", metricsUnverified,
		"placedCount", placedCount, "driftedCount", driftedCount, "expiredCount", expiredPlacementCount,
		"plannedFreedNodeCount", len(decision.Nodes.Planned), "actualFreedNodeCount", len(decision.Nodes.Actual),
		"missingFreedNodeCount", len(decision.Nodes.Missing), "missingFreedNodes", formatNodeNames(decision.Nodes.Missing),
		"unexpectedFreedNodeCount", len(decision.Nodes.Unexpected),
		"fragAfterPercent", resultMetrics.fragAfter, "movedCardCount", resultMetrics.movedCards)
	klog.V(4).InfoS("repack: terminal node-freeing set comparison",
		"run", run.Name, "plannedFreedNodes", decision.Nodes.Planned,
		"actualFreedNodes", decision.Nodes.Actual, "missingFreedNodes", decision.Nodes.Missing,
		"unexpectedFreedNodes", decision.Nodes.Unexpected, "setsEqual", decision.Nodes.Equal,
		"result", result)
	run.Status.Message = message
	state.SetCondition(&run.Status.Conditions, state.CondProgressing, metav1.ConditionFalse, decision.Reason, message, run.Generation)
	if decision.Succeeded {
		state.SetCondition(&run.Status.Conditions, state.CondComplete, metav1.ConditionTrue, decision.Reason, message, run.Generation)
	} else {
		state.SetCondition(&run.Status.Conditions, state.CondFailed, metav1.ConditionTrue, decision.Reason, message, run.Generation)
	}
	run.Status.Phase = state.DerivePhase(run.Status.Conditions)
	if err := e.updateStatusTerminal(ctx, run); err != nil {
		return err
	}
	// Placement is terminal even if API cleanup needs a retry. Do not hold the
	// global Execute slot while only removing our own metadata and gates.
	e.markExecuteDone(run.Name)
	e.requeueGatedRuns()
	// The terminal result is durable before cleanup. Returning an error makes the
	// workqueue retry the idempotent cleanup without ever repeating eviction.
	if err := e.cleanupPlacement(ctx, run); err != nil {
		return fmt.Errorf("cleanup placement after terminal result: %w", err)
	}
	return nil
}

type freedNodeSetComparison struct {
	Planned    []string
	Actual     []string
	Missing    []string
	Unexpected []string
	Equal      bool
}

type placementTerminalDecision struct {
	Succeeded bool
	Reason    string
	Nodes     freedNodeSetComparison
}

func evaluatePlacementTerminal(run *repackv1alpha1.RepackRun, metricsUnverified bool) placementTerminalDecision {
	_, drifted, expired := placementOutcomeCounts(run)
	nodes := compareFreedNodeSets(run)
	switch {
	case expired > 0:
		return placementTerminalDecision{Reason: state.ReasonPlacementExpired, Nodes: nodes}
	case metricsUnverified || run == nil || run.Status.Result == nil || !run.Status.Result.MetricsVerified:
		return placementTerminalDecision{Reason: state.ReasonMetricsUnverified, Nodes: nodes}
	case !nodes.Equal:
		return placementTerminalDecision{Reason: state.ReasonBenefitNotRealized, Nodes: nodes}
	case drifted > 0:
		return placementTerminalDecision{Succeeded: true, Reason: state.ReasonExecutedWithPlacementDrift, Nodes: nodes}
	default:
		return placementTerminalDecision{Succeeded: true, Reason: state.ReasonExecuted, Nodes: nodes}
	}
}

func compareFreedNodeSets(run *repackv1alpha1.RepackRun) freedNodeSetComparison {
	var planned, actual []string
	if run != nil && run.Status.Plan != nil {
		planned = run.Status.Plan.FreedNodes
	}
	if run != nil && run.Status.Result != nil {
		actual = run.Status.Result.FreedNodes
	}
	result := freedNodeSetComparison{
		Planned: sortedUniqueNodeNames(planned),
		Actual:  sortedUniqueNodeNames(actual),
	}
	plannedSet := make(map[string]struct{}, len(result.Planned))
	actualSet := make(map[string]struct{}, len(result.Actual))
	for _, nodeName := range result.Planned {
		plannedSet[nodeName] = struct{}{}
	}
	for _, nodeName := range result.Actual {
		actualSet[nodeName] = struct{}{}
	}
	for _, nodeName := range result.Planned {
		if _, found := actualSet[nodeName]; !found {
			result.Missing = append(result.Missing, nodeName)
		}
	}
	for _, nodeName := range result.Actual {
		if _, found := plannedSet[nodeName]; !found {
			result.Unexpected = append(result.Unexpected, nodeName)
		}
	}
	result.Equal = len(result.Missing) == 0 && len(result.Unexpected) == 0
	return result
}

func sortedUniqueNodeNames(nodeNames []string) []string {
	unique := make(map[string]struct{}, len(nodeNames))
	for _, nodeName := range nodeNames {
		if nodeName != "" {
			unique[nodeName] = struct{}{}
		}
	}
	result := make([]string, 0, len(unique))
	for nodeName := range unique {
		result = append(result, nodeName)
	}
	sort.Strings(result)
	return result
}

func placementObservationDeadlinePassed(run *repackv1alpha1.RepackRun, now time.Time) bool {
	if run == nil || len(run.Status.Nominations) == 0 {
		return false
	}
	var latest time.Time
	for index := range run.Status.Nominations {
		expirationTime := run.Status.Nominations[index].ExpirationTime
		if expirationTime == nil {
			return false
		}
		if expirationTime.Time.After(latest) {
			latest = expirationTime.Time
		}
	}
	return !latest.IsZero() && !now.Before(latest)
}

func placementBindingsVisible(nodes []*schedapi.NodeInfo, nominations []repackv1alpha1.PodNomination) bool {
	expected := make(map[string]string)
	for index := range nominations {
		nomination := &nominations[index]
		switch nomination.Phase {
		case repackv1alpha1.PodPlacementPlaced, repackv1alpha1.PodPlacementDegraded:
			if nomination.ReplacementPodUID == "" || nomination.ActualNodeName == "" {
				return false
			}
			expected[string(nomination.ReplacementPodUID)] = nomination.ActualNodeName
		}
	}
	if len(expected) == 0 {
		return true
	}
	for _, node := range nodes {
		if node == nil {
			continue
		}
		for _, task := range node.Tasks {
			if task == nil {
				continue
			}
			expectedNode, found := expected[string(task.UID)]
			if found && expectedNode == node.Name {
				delete(expected, string(task.UID))
			}
		}
	}
	return len(expected) == 0
}

func updateActualExecuteResult(run *repackv1alpha1.RepackRun, nodes []*schedapi.NodeInfo, targetResource v1.ResourceName) {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil || run.Status.Result == nil {
		return
	}
	run.Status.Result.FragAfterPercent = percentagePoints(engineapi.MeasureResourceFragmentation(nodes, targetResource).FragmentationRate())
	nodesByName := make(map[string]*schedapi.NodeInfo, len(nodes))
	for _, node := range nodes {
		if node != nil {
			nodesByName[node.Name] = node
		}
	}
	realizedCandidates := sortedUniqueNodeNames(realizedFreedNodeNames(run))
	realizedCandidateSet := make(map[string]struct{}, len(realizedCandidates))
	for _, nodeName := range realizedCandidates {
		realizedCandidateSet[nodeName] = struct{}{}
	}
	actuallyFreedNodes := make([]string, 0, len(realizedCandidates))
	for _, nodeName := range sortedUniqueNodeNames(run.Status.Plan.FreedNodes) {
		if _, realized := realizedCandidateSet[nodeName]; !realized {
			klog.V(4).InfoS("repack: planned node is not an actual-free candidate because its complete victim set was not removed",
				"run", run.Name, "node", nodeName, "resource", targetResource)
			continue
		}
		node := nodesByName[nodeName]
		if node == nil {
			klog.V(4).InfoS("repack: planned node not present in terminal scheduler snapshot",
				"run", run.Name, "node", nodeName, "resource", targetResource)
			continue
		}
		allocatable := engineapi.Scalar(node.Allocatable, targetResource)
		used := engineapi.Scalar(node.Used, targetResource)
		if allocatable <= 0 {
			klog.V(4).InfoS("repack: planned node no longer provides the target resource",
				"run", run.Name, "node", nodeName, "resource", targetResource,
				"allocatable", allocatable, "used", used)
			continue
		}
		if used == 0 {
			actuallyFreedNodes = append(actuallyFreedNodes, nodeName)
			klog.V(4).InfoS("repack: planned node verified free of the target resource",
				"run", run.Name, "node", nodeName, "resource", targetResource,
				"allocatable", allocatable, "used", used)
			continue
		}
		klog.V(4).InfoS("repack: planned node remains occupied by the target resource",
			"run", run.Name, "node", nodeName, "resource", targetResource,
			"allocatable", allocatable, "used", used)
	}
	run.Status.Result.FreedNodes = actuallyFreedNodes
	run.Status.Result.FreedNodeCount = int32(len(actuallyFreedNodes))
	run.Status.Result.MetricsVerified = true
	comparison := compareFreedNodeSets(run)
	klog.V(3).InfoS("repack: actual Execute benefit measured from scheduler snapshot",
		"run", run.Name, "resource", targetResource,
		"fragAfterPercent", run.Status.Result.FragAfterPercent,
		"freedNodeCount", run.Status.Result.FreedNodeCount,
		"movedCardCount", run.Status.Result.MovedCardCount,
		"plannedFreedNodeCount", len(comparison.Planned),
		"missingFreedNodeCount", len(comparison.Missing), "missingFreedNodes", formatNodeNames(comparison.Missing),
		"unexpectedFreedNodeCount", len(comparison.Unexpected))
	klog.V(4).InfoS("repack: actual Execute benefit node sets",
		"run", run.Name, "resource", targetResource,
		"plannedFreedNodes", comparison.Planned, "actualFreedNodes", comparison.Actual,
		"missingFreedNodes", comparison.Missing, "unexpectedFreedNodes", comparison.Unexpected)
}

func markExecuteBenefitUnverified(run *repackv1alpha1.RepackRun) {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil {
		return
	}
	if run.Status.Result == nil {
		run.Status.Result = &repackv1alpha1.RepackResult{}
	}
	run.Status.Result.FragAfterPercent = run.Status.Plan.Summary.FragBeforePercent
	run.Status.Result.FreedNodeCount = 0
	run.Status.Result.FreedNodes = nil
	run.Status.Result.MetricsVerified = false
}

// expirePlacements is the liveness escape hatch. A scheduling gate deliberately
// fails closed while the engine is deciding a receiver, but it must never leave
// a workload unavailable forever when concurrent work consumed every viable
// receiver. At the durable nomination deadline, release only our gate and let
// normal scheduling restore the Pod; the Run then ends Failed with explicit
// placement status instead of silently claiming defragmentation success.
func (e *Engine) expirePlacements(ctx context.Context, run *repackv1alpha1.RepackRun) (bool, error) {
	keys := map[placementIdentity]struct{}{}
	for index := range run.Status.Nominations {
		nomination := &run.Status.Nominations[index]
		if placementCanExpire(nomination, e.now()) {
			keys[placementIdentityForNomination(nomination)] = struct{}{}
		}
	}
	if len(keys) == 0 {
		return false, nil
	}
	klog.V(3).InfoS("repack: replacement placement deadline reached",
		"run", run.Name, "expiringNominationCount", len(keys))
	var updatedRun *repackv1alpha1.RepackRun
	expiredCount := 0
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, run.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		expiredCount = 0
		for index := range latest.Status.Nominations {
			nomination := &latest.Status.Nominations[index]
			if _, found := keys[placementIdentityForNomination(nomination)]; !found || !placementCanExpire(nomination, e.now()) {
				continue
			}
			nomination.Phase = repackv1alpha1.PodPlacementExpired
			changed = true
			expiredCount++
		}
		if !changed {
			return nil
		}
		updatedRun, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, latest, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return false, err
	}
	if updatedRun != nil {
		e.recordRunEvent(updatedRun, v1.EventTypeWarning, eventReasonPlacementExpired,
			fmt.Sprintf("%d replacement placement intents expired; scheduling gates will be released.", expiredCount))
		return true, nil
	}
	return false, nil
}

func placementCanExpire(nomination *repackv1alpha1.PodNomination, now time.Time) bool {
	if nomination == nil || nomination.ExpirationTime == nil || now.Before(nomination.ExpirationTime.Time) {
		return false
	}
	switch nomination.Phase {
	case repackv1alpha1.PodPlacementPrepared, repackv1alpha1.PodPlacementGated,
		repackv1alpha1.PodPlacementAwaitingCapacity, repackv1alpha1.PodPlacementNominated:
		return true
	default:
		return false
	}
}
