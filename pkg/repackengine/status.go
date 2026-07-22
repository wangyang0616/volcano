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
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

func (e *Engine) fail(ctx context.Context, run *repackv1alpha1.RepackRun, generation int64, reason string, err error) error {
	klog.ErrorS(err, "repack: run failed", "run", run.Name, "reason", reason)
	message := failureStatusMessage(e.resolveResource(run), reason, err)
	run.Status.Message = message
	state.SetCondition(&run.Status.Conditions, state.CondProgressing, metav1.ConditionFalse, reason, message, generation)
	state.SetCondition(&run.Status.Conditions, state.CondFailed, metav1.ConditionTrue, reason, message, generation)
	run.Status.Phase = state.DerivePhase(run.Status.Conditions)
	if err := e.updateStatusTerminal(ctx, run); err != nil {
		return err
	}
	if run.Spec.Mode != repackv1alpha1.RepackModeExecute {
		return nil
	}
	// A failure after the prepare barrier must release its PodGroup lease.
	// Pod-level gate cleanup is driven by the nomination controller from this
	// terminal status. Return lease cleanup failures so the terminal-only
	// reconcile path can retry without replaying an eviction.
	e.markExecuteDone(run.Name)
	e.requeueGatedRuns()
	if err := e.cleanupPlacement(ctx, run); err != nil {
		return fmt.Errorf("cleanup placement after failure: %w", err)
	}
	return nil
}

func (e *Engine) updateStatus(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	stampLifecycle(run, time.Now())
	desired := run.Status.DeepCopy()
	err := e.writeStatus(ctx, run.Name, desired)
	if err != nil {
		klog.ErrorS(err, "repack: update status", "run", run.Name)
	}
	return err
}

func (e *Engine) writeStatus(ctx context.Context, name string, desired *repackv1alpha1.RepackRunStatus) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		// Re-apply the intended status onto the freshest object. Placement progress is
		// controller-owned; preserve a concurrently observed replacement association or
		// terminal placement result instead of resetting it during an engine write.
		merged := desired.DeepCopy()
		mergeNominationPhases(merged.Nominations, latest.Status.Nominations)
		merged.DeepCopyInto(&latest.Status)
		_, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, latest, metav1.UpdateOptions{})
		return err
	})
}

func mergeNominationPhases(desired, latest []repackv1alpha1.PodNomination) {
	placements := make(map[string]repackv1alpha1.PodNomination, len(latest))
	for i := range latest {
		r := &latest[i]
		if r.Phase == repackv1alpha1.PodPlacementGated || r.Phase == repackv1alpha1.PodPlacementAwaitingCapacity ||
			r.Phase == repackv1alpha1.PodPlacementNominated || r.Phase == repackv1alpha1.PodPlacementPlaced ||
			r.Phase == repackv1alpha1.PodPlacementDegraded || r.Phase == repackv1alpha1.PodPlacementExpired ||
			r.Phase == repackv1alpha1.PodNominationBound || r.Phase == repackv1alpha1.PodNominationExpired {
			placements[nominationKey(r)] = *r
		}
	}
	for i := range desired {
		if placement, found := placements[nominationKey(&desired[i])]; found {
			desired[i].Phase = placement.Phase
			desired[i].SelectedNodeName = placement.SelectedNodeName
			desired[i].ReplacementPodName = placement.ReplacementPodName
			desired[i].ReplacementPodUID = placement.ReplacementPodUID
			desired[i].ActualNodeName = placement.ActualNodeName
		}
	}
}

func nominationKey(r *repackv1alpha1.PodNomination) string {
	if r == nil {
		return ""
	}
	return r.Namespace + "\x00" + r.PodGroupName + "\x00" + r.VictimPodName + "\x00" + r.NodeName
}

// updateStatusTerminal keeps retrying until the terminal result is durable or
// leadership/context is lost. After Execute side effects have started, returning
// success without this write would leave an ambiguous, non-replayable Run.
func (e *Engine) updateStatusTerminal(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	stampLifecycle(run, time.Now())
	desired := run.Status.DeepCopy()
	name := run.Name
	err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		if err := e.writeStatus(ctx, name, desired); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil // explicitly deleted; no terminal object remains to persist
			}
			klog.ErrorS(err, "repack: terminal status persistence failed; retrying", "run", name)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("persist terminal status for %s: %w", name, err)
	}

	outcome := terminalOutcome(run)
	metrics.ObserveRun(string(run.Spec.Mode), outcome)
	klog.V(4).InfoS("repack: terminal status persisted", "run", run.Name, "mode", run.Spec.Mode,
		"phase", run.Status.Phase, "outcome", outcome, "nominationCount", len(run.Status.Nominations))
	if e.recorder != nil {
		etype := v1.EventTypeNormal
		if run.Status.Phase == repackv1alpha1.RepackFailed {
			etype = v1.EventTypeWarning
		}
		message := run.Status.Message
		if message == "" {
			message = "RepackRun reached a terminal state."
		}
		e.recorder.Event(run, etype, outcome, message)
	}
	return nil
}

// terminalOutcome is the reason of the True Complete/Failed/Cancelled condition
// (the run's terminal verdict) for the runs_total metric; "Unknown" if none.
func terminalOutcome(run *repackv1alpha1.RepackRun) string {
	for _, c := range run.Status.Conditions {
		if c.Status == metav1.ConditionTrue &&
			(c.Type == state.CondComplete || c.Type == state.CondFailed || c.Type == state.CondCancelled) {
			return c.Reason
		}
	}
	return "Unknown"
}

// stampLifecycle records StartTime on first Running and CompletionTime on first
// terminal — the anchors the controller's TTL-GC and Execute cooldown (K=1) key
// off. Both stamps are nil-guarded so the engine and the controller (which guards
// the same fields) never clobber each other: whoever reaches the state first wins.
func stampLifecycle(run *repackv1alpha1.RepackRun, now time.Time) {
	if run.Status.Phase == repackv1alpha1.RepackRunning && run.Status.StartTime == nil {
		t := metav1.NewTime(now)
		run.Status.StartTime = &t
	}
	if state.IsTerminal(run.Status.Phase) && run.Status.CompletionTime == nil {
		t := metav1.NewTime(now)
		run.Status.CompletionTime = &t
	}
}

// applyPlan maps the search outcome onto the immutable status.plan. DryRun and
// Execute expose the same complete pre-eviction decision, including predicted
// benefit. Execute acceptance and observed cluster metrics are deliberately
// reported through status.nominations and status.result instead of rewriting
// this audit record.
func applyPlan(
	run *repackv1alpha1.RepackRun,
	report engineframework.Report,
	plan *engineapi.RepackPlan,
	targetResource v1.ResourceName,
	owners map[string]*repackv1alpha1.WorkloadRef,
	resolvedScope *repackv1alpha1.ResolvedScope,
) {
	moves := buildStatusMoves(plan, targetResource, owners)
	summary := buildRepackSummary(report)
	if summary != nil {
		var cards int64
		for _, m := range moves {
			cards += m.Cards
		}
		summary.MovedCardCount = cards
		if resolvedScope != nil {
			summary.ResolvedScope = resolvedScope.DeepCopy()
		}
	}
	run.Status.Plan = &repackv1alpha1.RepackPlan{
		Summary:    summary,
		Moves:      moves,
		FreedNodes: sortedFreedNodeNames(plan),
	}
}

// buildResolvedScope summarizes the two independent action-scope axes. The
// fragmentation report remains cluster-wide: node scope limits drain targets,
// while PodGroup scope limits which accelerator consumers may be moved.
func buildResolvedScope(nodes []*schedapi.NodeInfo, scope *engineframework.ScopeMatcher, targetResource v1.ResourceName) *repackv1alpha1.ResolvedScope {
	resolved := &repackv1alpha1.ResolvedScope{}
	podGroups := make(map[schedapi.JobID]struct{})
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if engineapi.Scalar(node.Allocatable, targetResource) > 0 &&
			(scope == nil || scope.NodeInScope(node)) {
			resolved.NodeCount++
		}
		for _, task := range node.Tasks {
			if task == nil || task.Job == "" || engineapi.Scalar(task.Resreq, targetResource) <= 0 {
				continue
			}
			if scope == nil || scope.InScope(task.Job) {
				podGroups[task.Job] = struct{}{}
			}
		}
	}
	resolved.PodGroupCount = int32(len(podGroups))
	return resolved
}

func markExecuteNotPerformed(run *repackv1alpha1.RepackRun) {
	if run == nil {
		return
	}
	run.Status.Result = nil
	run.Status.Nominations = nil
}

// prepareExecuteNominations records the complete set of placement intents before
// the eviction barrier. A later commit filters this set to accepted evictions.
func prepareExecuteNominations(run *repackv1alpha1.RepackRun, plan *engineapi.RepackPlan, nominationTTL time.Duration) {
	if run == nil {
		return
	}
	run.Status.Nominations = buildPodNominations(plan, nominationTTL)
}

// retainAcceptedNominations keeps only intents whose evictions were accepted.
// Existing records are retained verbatim so their durable expiration deadline
// and any concurrently observed replacement association are never reset.
func retainAcceptedNominations(
	existing []repackv1alpha1.PodNomination,
	acceptedPlan *engineapi.RepackPlan,
	nominationTTL time.Duration,
) []repackv1alpha1.PodNomination {
	accepted := buildPodNominations(acceptedPlan, nominationTTL)
	if len(accepted) == 0 {
		return nil
	}
	existingByKey := make(map[string]repackv1alpha1.PodNomination, len(existing))
	for index := range existing {
		existingByKey[nominationKey(&existing[index])] = existing[index]
	}
	for index := range accepted {
		if record, found := existingByKey[nominationKey(&accepted[index])]; found {
			accepted[index] = record
		}
	}
	return accepted
}

// initializeExecuteResult publishes the accepted disruption amount immediately
// after CommitPlan. Cluster-wide benefit remains conservative until replacement
// bindings are visible in one coherent scheduler snapshot.
func initializeExecuteResult(run *repackv1alpha1.RepackRun, acceptedPlan *engineapi.RepackPlan, targetResource v1.ResourceName) {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil {
		return
	}
	run.Status.Result = &repackv1alpha1.RepackResult{
		FragAfterPercent: run.Status.Plan.Summary.FragBeforePercent,
		MovedCardCount:   movedCardCount(acceptedPlan, targetResource),
		MetricsVerified:  false,
	}
}

func initializeNoopExecuteResult(run *repackv1alpha1.RepackRun) {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil {
		return
	}
	run.Status.Result = &repackv1alpha1.RepackResult{
		FragAfterPercent: run.Status.Plan.Summary.FragBeforePercent,
		MetricsVerified:  true,
	}
}

func movedCardCount(plan *engineapi.RepackPlan, targetResource v1.ResourceName) int64 {
	var cards int64
	for _, move := range buildStatusMoves(plan, targetResource, nil) {
		cards += move.Cards
	}
	return cards
}

// acceptedFreedNodeNames derives the source nodes whose complete planned victim
// set was accepted. status.plan remains the complete proposal, so placement must
// not exclude a source node whose eviction set was only partially accepted.
func acceptedFreedNodeNames(run *repackv1alpha1.RepackRun) []string {
	if run == nil || run.Status.Plan == nil {
		return nil
	}
	accepted := make(map[string]struct{}, len(run.Status.Nominations))
	for index := range run.Status.Nominations {
		nomination := &run.Status.Nominations[index]
		accepted[nomination.Namespace+"\x00"+nomination.PodGroupName+"\x00"+nomination.VictimPodName+"\x00"+nomination.NodeName] = struct{}{}
	}
	var result []string
	for _, nodeName := range run.Status.Plan.FreedNodes {
		hasPlannedVictim := false
		complete := true
		for moveIndex := range run.Status.Plan.Moves {
			move := &run.Status.Plan.Moves[moveIndex]
			for podIndex := range move.Pods {
				pod := &move.Pods[podIndex]
				if pod.FromNode != nodeName {
					continue
				}
				hasPlannedVictim = true
				key := move.Namespace + "\x00" + move.PodGroupName + "\x00" + pod.Name + "\x00" + pod.ToNode
				if _, found := accepted[key]; !found {
					complete = false
					break
				}
			}
			if !complete {
				break
			}
		}
		if hasPlannedVictim && complete {
			result = append(result, nodeName)
		}
	}
	return result
}

// buildStatusMoves groups the plan's per-task relocations into per-PodGroup status moves;
// fromNode/toNode live per-pod in pods[] (a gang's pods may spread across nodes).
// moves is a pure plan (identical in DryRun/Execute). Deterministic order.
func buildStatusMoves(plan *engineapi.RepackPlan, targetResource v1.ResourceName, owners map[string]*repackv1alpha1.WorkloadRef) []repackv1alpha1.RepackMove {
	if plan == nil {
		return nil
	}
	moveIndexByPodGroup := map[string]int{} // JobID ("ns/name") -> index in statusMoves
	statusMoves := []repackv1alpha1.RepackMove{}
	for _, move := range plan.Moves {
		if move == nil || move.Task == nil || move.To == move.From {
			continue
		}
		podGroupID := string(move.Task.Job)
		moveIndex, ok := moveIndexByPodGroup[podGroupID]
		if !ok {
			moveIndex = len(statusMoves)
			moveIndexByPodGroup[podGroupID] = moveIndex
			namespace, podGroupName := splitPodGroupID(podGroupID)
			statusMoves = append(statusMoves, repackv1alpha1.RepackMove{
				Namespace:    namespace,
				PodGroupName: podGroupName,
				Owner:        owners[podGroupID],
			})
		}
		var cards int64
		if move.Task.Resreq != nil {
			// Report whole devices to users; Resreq is stored in milli-units.
			cards = engineapi.Cards(move.Task.Resreq, targetResource)
		}
		statusMoves[moveIndex].Cards += cards
		statusMoves[moveIndex].Pods = append(statusMoves[moveIndex].Pods, repackv1alpha1.PodMove{
			Name:     move.Task.Name,
			FromNode: move.From,
			ToNode:   move.To,
			Cards:    cards,
		})
	}
	sort.Slice(statusMoves, func(i, j int) bool {
		if statusMoves[i].Namespace != statusMoves[j].Namespace {
			return statusMoves[i].Namespace < statusMoves[j].Namespace
		}
		return statusMoves[i].PodGroupName < statusMoves[j].PodGroupName
	})
	for moveIndex := range statusMoves {
		pods := statusMoves[moveIndex].Pods
		sort.Slice(pods, func(a, b int) bool {
			switch {
			case pods[a].Name != pods[b].Name:
				return pods[a].Name < pods[b].Name
			case pods[a].FromNode != pods[b].FromNode:
				return pods[a].FromNode < pods[b].FromNode
			default:
				return pods[a].ToNode < pods[b].ToNode
			}
		})
	}
	return statusMoves
}

// resolveMoveOwners returns the direct controller owner for every PodGroup
// affected by plan. Owner display is best-effort status enrichment: a deleted
// PodGroup or a transient read failure must not prevent an otherwise valid
// RepackRun from completing.
func (e *Engine) resolveMoveOwners(ctx context.Context, plan *engineapi.RepackPlan) map[string]*repackv1alpha1.WorkloadRef {
	if plan == nil || e.volcanoClient == nil {
		return nil
	}
	owners := make(map[string]*repackv1alpha1.WorkloadRef)
	for _, podGroupID := range plan.AffectedPodGroups() {
		namespace, name := splitPodGroupID(string(podGroupID))
		if namespace == "" || name == "" {
			continue
		}
		podGroup, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				klog.V(4).InfoS("repack: cannot resolve PodGroup owner for status move", "podGroup", podGroupID, "err", err)
			}
			continue
		}
		owner := metav1.GetControllerOf(podGroup)
		if owner == nil {
			continue
		}
		owners[string(podGroupID)] = &repackv1alpha1.WorkloadRef{
			APIVersion: owner.APIVersion,
			Kind:       owner.Kind,
			Name:       owner.Name,
		}
	}
	return owners
}

// splitPodGroupID splits a "namespace/name" JobID; missing "/" -> ("", id).
func splitPodGroupID(podGroupID string) (namespace, name string) {
	if separatorIndex := strings.IndexByte(podGroupID, '/'); separatorIndex >= 0 {
		return podGroupID[:separatorIndex], podGroupID[separatorIndex+1:]
	}
	return "", podGroupID
}

// sortedFreedNodeNames lists the names of nodes the plan empties (sorted).
func sortedFreedNodeNames(plan *engineapi.RepackPlan) []string {
	if plan == nil {
		return nil
	}
	freedNodeNames := append([]string(nil), plan.FreedNodes...)
	sort.Strings(freedNodeNames)
	return freedNodeNames
}

// buildRepackSummary renders the flat metrics layer. "Worth repacking?" is not
// here — it is folded into the terminal condition's reason. MovedCardCount is
// filled by applyPlan from moves; FragBefore/After are the target resource's
// cluster-wide rates and do not use resolved scope as their denominator.
func buildRepackSummary(report engineframework.Report) *repackv1alpha1.RepackSummary {
	return &repackv1alpha1.RepackSummary{
		FragBeforePercent: percentagePoints(report.FragmentationRateBefore),
		FragAfterPercent:  percentagePoints(report.FragmentationRateAfter),
		FreedNodeCount:    int32(report.NodesFreed),
	}
}

// percentagePoints rounds a 0-1 fraction to an integer percentage point, clamped to [0,100].
func percentagePoints(fraction float64) int32 {
	percentage := int32(fraction*100 + 0.5)
	if percentage < 0 {
		return 0
	}
	if percentage > 100 {
		return 100
	}
	return percentage
}

// buildPodNominations renders per-pod placement-steering intents (Execute-only). Claiming
// follows the placement identity contract (proposal §5.2.2): victimPodName exact
// match, then identityLabels (label-superset match), then fungible. IdentityLabels
// are resolved from the victim pod's own well-known labels by the framework.
func buildPodNominations(plan *engineapi.RepackPlan, nominationTTL time.Duration) []repackv1alpha1.PodNomination {
	if plan == nil {
		return nil
	}
	expirationTime := metav1.NewTime(time.Now().Add(nominationTTL))
	intents := engineframework.NominationIntents(plan)
	nominations := make([]repackv1alpha1.PodNomination, 0, len(intents))
	for _, intent := range intents {
		_, podGroupName := splitPodGroupID(string(intent.Gang))
		nominations = append(nominations, repackv1alpha1.PodNomination{
			Namespace:      intent.Namespace,
			PodGroupName:   podGroupName,
			VictimPodName:  intent.PodName,
			IdentityLabels: intent.IdentityLabels,
			NodeName:       intent.Node,
			Phase:          repackv1alpha1.PodPlacementPrepared,
			ExpirationTime: &expirationTime,
		})
	}
	return nominations
}
