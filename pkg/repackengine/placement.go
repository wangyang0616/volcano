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
	"fmt"
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	for key := range groups {
		namespace, podGroupName := splitPlacementPodGroupKey(key)
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

func placementPodGroups(run *repackv1alpha1.RepackRun) map[string]struct{} {
	groups := make(map[string]struct{})
	if run == nil {
		return groups
	}
	for i := range run.Status.Nominations {
		nomination := &run.Status.Nominations[i]
		if nomination.Namespace != "" && nomination.PodGroupName != "" {
			groups[nomination.Namespace+"\x00"+nomination.PodGroupName] = struct{}{}
		}
	}
	return groups
}

func placementGroupsDifference(all, retain map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{})
	for key := range all {
		if _, stillNeeded := retain[key]; !stillNeeded {
			result[key] = struct{}{}
		}
	}
	return result
}

// releasePlacementLeases removes only annotations owned by this Run. It is safe
// to call on every terminal/error path: a newer Run's lease is never touched.
func (e *Engine) releasePlacementLeases(ctx context.Context, run *repackv1alpha1.RepackRun, groups map[string]struct{}) error {
	if run == nil || len(groups) == 0 {
		return nil
	}
	lease := placement.OwnerValue(run.Name, run.UID)
	for key := range groups {
		namespace, podGroupName := splitPlacementPodGroupKey(key)
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			podGroup, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Get(ctx, podGroupName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil || podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] != lease {
				return err
			}
			podGroup = podGroup.DeepCopy()
			delete(podGroup.Annotations, repackv1alpha1.PlacementLeaseAnnotation)
			_, err = e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Update(ctx, podGroup, metav1.UpdateOptions{})
			return err
		})
		if err != nil {
			return fmt.Errorf("release placement lease for PodGroup %s/%s: %w", namespace, podGroupName, err)
		}
	}
	return nil
}

// cleanupPlacement removes engine-owned PodGroup leases. Pod gate lifecycle is
// owned exclusively by the nomination controller, which watches terminal and
// deleted Runs through the gate-owner Pod index.
func (e *Engine) cleanupPlacement(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	return e.releasePlacementLeases(ctx, run, placementPodGroups(run))
}

func splitPlacementPodGroupKey(key string) (string, string) {
	for i := range key {
		if key[i] == '\x00' {
			return key[:i], key[i+1:]
		}
	}
	return "", ""
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
	committed := make([]*engineapi.Move, 0, len(pending))
	selected := make(map[string]string, len(pending))
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
		receivers := placementReceivers(snapshot.Nodes(), acceptedFreedNodeNames(run), nomination.NodeName, task)
		placements, fit := snapshot.FeasibleRelocation(committed, []*schedapi.TaskInfo{task}, receivers)
		if !fit || len(placements) != 1 {
			return e.markAwaitingPlacement(ctx, run.Name, pending)
		}
		committed = append(committed, placements[0])
		selected[placementStatusKey(nomination)] = placements[0].To
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
	sort.Slice(result, func(i, j int) bool { return placementStatusKey(result[i]) < placementStatusKey(result[j]) })
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

func (e *Engine) writePlacementSelection(ctx context.Context, runName string, selected map[string]string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		run, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		for index := range run.Status.Nominations {
			nomination := &run.Status.Nominations[index]
			if node, found := selected[placementStatusKey(nomination)]; found && nomination.SelectedNodeName == "" {
				nomination.SelectedNodeName = node
				changed = true
			}
		}
		if !changed {
			return nil
		}
		_, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
		return err
	})
}

func (e *Engine) markAwaitingPlacement(ctx context.Context, runName string, nominations []*repackv1alpha1.PodNomination) error {
	keys := make(map[string]struct{}, len(nominations))
	for _, nomination := range nominations {
		keys[placementStatusKey(nomination)] = struct{}{}
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		run, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		for index := range run.Status.Nominations {
			nomination := &run.Status.Nominations[index]
			if _, found := keys[placementStatusKey(nomination)]; found && nomination.SelectedNodeName == "" && nomination.Phase != repackv1alpha1.PodPlacementAwaitingCapacity {
				nomination.Phase = repackv1alpha1.PodPlacementAwaitingCapacity
				changed = true
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
		_, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
		return err
	})
	if err == nil {
		e.workQueue.AddAfter(runName, placementRetryInterval)
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
	degraded := false
	expired := false
	metricsUnverified := false
	for index := range run.Status.Nominations {
		if run.Status.Nominations[index].Phase != repackv1alpha1.PodPlacementPlaced {
			degraded = true
		}
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
			degraded = true
			metricsUnverified = true
			markExecuteBenefitUnverified(run)
		} else {
			updateActualExecuteResult(run, nodes, targetResource)
			schedframework.CloseSessionReadOnly(schedulerSession)
		}
	}

	reason := state.ReasonExecuted
	if degraded {
		reason = state.ReasonPlacementDegraded
	}
	message := placementStatusMessage(run, targetResource, degraded, metricsUnverified)
	run.Status.Message = message
	state.SetCondition(&run.Status.Conditions, state.CondProgressing, metav1.ConditionFalse, reason, message, run.Generation)
	if degraded {
		state.SetCondition(&run.Status.Conditions, state.CondFailed, metav1.ConditionTrue, reason, message, run.Generation)
	} else {
		state.SetCondition(&run.Status.Conditions, state.CondComplete, metav1.ConditionTrue, reason, message, run.Generation)
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
	var actuallyFreed int32
	for _, nodeName := range acceptedFreedNodeNames(run) {
		node := nodesByName[nodeName]
		if node == nil || engineapi.Scalar(node.Allocatable, targetResource) <= 0 {
			continue
		}
		if engineapi.Scalar(node.Used, targetResource) == 0 {
			actuallyFreed++
		}
	}
	run.Status.Result.FreedNodeCount = actuallyFreed
	run.Status.Result.MetricsVerified = true
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
	run.Status.Result.MetricsVerified = false
}

func placementStatusKey(nomination *repackv1alpha1.PodNomination) string {
	if nomination == nil {
		return ""
	}
	return nomination.Namespace + "\x00" + nomination.PodGroupName + "\x00" + nomination.VictimPodName + "\x00" + nomination.NodeName
}

// expirePlacements is the liveness escape hatch. A scheduling gate deliberately
// fails closed while the engine is deciding a receiver, but it must never leave
// a workload unavailable forever when concurrent work consumed every viable
// receiver. At the durable nomination deadline, release only our gate and let
// normal scheduling restore the Pod; the Run then ends Failed with explicit
// placement status instead of silently claiming defragmentation success.
func (e *Engine) expirePlacements(ctx context.Context, run *repackv1alpha1.RepackRun) (bool, error) {
	keys := map[string]struct{}{}
	for index := range run.Status.Nominations {
		nomination := &run.Status.Nominations[index]
		if nomination.ExpirationTime == nil || e.now().Before(nomination.ExpirationTime.Time) {
			continue
		}
		switch nomination.Phase {
		case repackv1alpha1.PodPlacementPrepared, repackv1alpha1.PodPlacementGated, repackv1alpha1.PodPlacementAwaitingCapacity:
			keys[placementStatusKey(nomination)] = struct{}{}
		}
	}
	if len(keys) == 0 {
		return false, nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, run.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		for index := range latest.Status.Nominations {
			nomination := &latest.Status.Nominations[index]
			if _, found := keys[placementStatusKey(nomination)]; !found {
				continue
			}
			nomination.Phase = repackv1alpha1.PodPlacementExpired
			changed = true
		}
		if !changed {
			return nil
		}
		_, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, latest, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return false, err
	}
	return true, nil
}
