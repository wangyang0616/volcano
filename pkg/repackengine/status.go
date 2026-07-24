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
	"errors"
	"fmt"
	"sort"
	"strings"
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

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

func (e *Engine) fail(ctx context.Context, run *repackv1alpha1.RepackRun, generation int64, reason string, err error) error {
	klog.ErrorS(err, "repack: run failed", "run", run.Name, "reason", reason)
	message := failureStatusMessage(e.resolveResource(run), reason, err)
	state.MarkFailed(run, reason, message)
	if err := e.updateStatusTerminal(ctx, run); err != nil {
		return err
	}
	if run.Spec.Mode != repackv1alpha1.RepackModeExecute {
		return nil
	}
	// A failure after the prepare barrier must release its PodGroup lease.
	// Pod-level gate cleanup is driven by the placement controller from this
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
		mergeRelocationProgress(merged.Relocations, latest.Status.Relocations)
		merged.DeepCopyInto(&latest.Status)
		_, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, latest, metav1.UpdateOptions{})
		return err
	})
}

// mergeRelocationProgress preserves the two independently owned journals when
// an engine status retry races with placement-controller updates.
func mergeRelocationProgress(desired, latest []repackv1alpha1.PodRelocationStatus) {
	placements := make(map[placementIdentity]repackv1alpha1.PodRelocationStatus, len(latest))
	evictions := make(map[placementIdentity]repackv1alpha1.PodRelocationStatus, len(latest))
	replacements := make(map[placementIdentity]string, len(latest))
	for i := range latest {
		r := &latest[i]
		evictions[placementIdentityForRelocation(r)] = *r
		if r.ReplacementPodGroupName != "" {
			replacements[placementIdentityForRelocation(r)] = r.ReplacementPodGroupName
		}
		switch r.Placement.Phase {
		case repackv1alpha1.PodPlacementWaitingForNodeSelection,
			repackv1alpha1.PodPlacementNominated,
			repackv1alpha1.PodPlacementPlaced,
			repackv1alpha1.PodPlacementTimedOut:
			placements[placementIdentityForRelocation(r)] = *r
		}
	}
	for i := range desired {
		if persisted, found := evictions[placementIdentityForRelocation(&desired[i])]; found {
			persistedPhase := persisted.Eviction.Phase
			desiredPhase := desired[i].Eviction.Phase
			if persistedPhase != "" && (persistedPhase == desiredPhase ||
				evictionPhaseAdvances(desiredPhase, persistedPhase)) {
				desired[i].VictimPodUID = persisted.VictimPodUID
				desired[i].Eviction.Phase = persistedPhase
				desired[i].Eviction.Message = persisted.Eviction.Message
			}
		}
		if replacementPodGroupName := replacements[placementIdentityForRelocation(&desired[i])]; replacementPodGroupName != "" {
			desired[i].ReplacementPodGroupName = replacementPodGroupName
		}
		if placement, found := placements[placementIdentityForRelocation(&desired[i])]; found {
			latestPhase := placement.Placement.Phase
			desiredPhase := desired[i].Placement.Phase
			// Preserve controller progress only when it advances the placement
			// journal (or refreshes the same phase). In particular, an older
			// WaitingForNodeSelection observation must not overwrite the
			// Engine's durable TimedOut decision during terminal status merge.
			if latestPhase == desiredPhase ||
				placementPhaseAdvances(desiredPhase, latestPhase) {
				desired[i].Placement.Phase = latestPhase
				desired[i].Placement.SelectedNodeName = placement.Placement.SelectedNodeName
				desired[i].Placement.ReplacementPodName = placement.Placement.ReplacementPodName
				desired[i].Placement.ReplacementPodUID = placement.Placement.ReplacementPodUID
				desired[i].Placement.ActualNodeName = placement.Placement.ActualNodeName
			}
		}
	}
}

// placementPhaseAdvances reports whether observed is a later placement state
// than current. Placed and TimedOut are sibling terminal states: neither may
// replace the other.
func placementPhaseAdvances(current, observed repackv1alpha1.PodPlacementPhase) bool {
	switch observed {
	case repackv1alpha1.PodPlacementWaitingForReplacement:
		return current == ""
	case repackv1alpha1.PodPlacementWaitingForNodeSelection:
		return current == "" ||
			current == repackv1alpha1.PodPlacementWaitingForReplacement
	case repackv1alpha1.PodPlacementNominated:
		return current == "" ||
			current == repackv1alpha1.PodPlacementWaitingForReplacement ||
			current == repackv1alpha1.PodPlacementWaitingForNodeSelection
	case repackv1alpha1.PodPlacementPlaced,
		repackv1alpha1.PodPlacementTimedOut:
		return current == "" ||
			current == repackv1alpha1.PodPlacementWaitingForReplacement ||
			current == repackv1alpha1.PodPlacementWaitingForNodeSelection ||
			current == repackv1alpha1.PodPlacementNominated
	}
	return false
}

// evictionPhaseAdvances reports whether observed is a later eviction state than
// current. Accepted, IndirectlyRemoved, and Rejected are sibling terminal states.
func evictionPhaseAdvances(current, observed repackv1alpha1.PodEvictionPhase) bool {
	switch observed {
	case repackv1alpha1.PodEvictionPending:
		return current == ""
	case repackv1alpha1.PodEvictionInProgress:
		return current == "" ||
			current == repackv1alpha1.PodEvictionPending
	case repackv1alpha1.PodEvictionAccepted,
		repackv1alpha1.PodEvictionIndirectlyRemoved,
		repackv1alpha1.PodEvictionRejected:
		return current == "" ||
			current == repackv1alpha1.PodEvictionPending ||
			current == repackv1alpha1.PodEvictionInProgress
	}
	return false
}

type placementIdentity struct {
	Namespace    string
	PodGroupName string
	PodName      string
	TargetNode   string
}

func placementIdentityForRelocation(relocation *repackv1alpha1.PodRelocationStatus) placementIdentity {
	if relocation == nil {
		return placementIdentity{}
	}
	return placementIdentity{
		Namespace: relocation.Namespace, PodGroupName: relocation.PodGroupName,
		PodName: relocation.VictimPodName, TargetNode: relocation.PlannedNodeName,
	}
}

func placementIdentityForMove(namespace, podGroupName, podName, targetNode string) placementIdentity {
	return placementIdentity{
		Namespace: namespace, PodGroupName: podGroupName,
		PodName: podName, TargetNode: targetNode,
	}
}

func (identity placementIdentity) less(other placementIdentity) bool {
	switch {
	case identity.Namespace != other.Namespace:
		return identity.Namespace < other.Namespace
	case identity.PodGroupName != other.PodGroupName:
		return identity.PodGroupName < other.PodGroupName
	case identity.PodName != other.PodName:
		return identity.PodName < other.PodName
	default:
		return identity.TargetNode < other.TargetNode
	}
}

const terminalStatusWriteAttempts = 3

type terminalStatusPersistenceError struct {
	runName string
	err     error
}

func (e *terminalStatusPersistenceError) Error() string {
	return fmt.Sprintf("persist terminal status for %s: %v", e.runName, e.err)
}

func (e *terminalStatusPersistenceError) Unwrap() error { return e.err }

func isTerminalStatusPersistenceError(err error) bool {
	var target *terminalStatusPersistenceError
	return errors.As(err, &target)
}

// updateStatusTerminal performs a bounded local retry. The workqueue owns the
// longer-lived retry so a persistently broken object cannot monopolize the
// engine's only worker and starve unrelated RepackRuns.
func (e *Engine) updateStatusTerminal(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	stampLifecycle(run, time.Now())
	desired := run.Status.DeepCopy()
	name := run.Name
	e.rememberPendingTerminalStatus(name, desired)
	backoff := retry.DefaultBackoff
	backoff.Steps = terminalStatusWriteAttempts
	err := retry.OnError(backoff, func(err error) bool {
		return !apierrors.IsNotFound(err) && ctx.Err() == nil
	}, func() error {
		return e.writeStatus(ctx, name, desired)
	})
	if apierrors.IsNotFound(err) {
		e.forgetPendingTerminalStatus(name)
		return nil // explicitly deleted; no terminal object remains to persist
	}
	if err != nil {
		klog.ErrorS(err, "repack: terminal status persistence exhausted local retry budget",
			"run", name, "attempts", terminalStatusWriteAttempts)
		return &terminalStatusPersistenceError{runName: name, err: err}
	}
	e.forgetPendingTerminalStatus(name)

	outcome := terminalOutcome(run)
	metrics.ObserveRun(string(run.Spec.Mode), outcome)
	klog.V(4).InfoS("repack: terminal status persisted", "run", run.Name, "mode", run.Spec.Mode,
		"phase", run.Status.Phase, "outcome", outcome, "relocationCount", len(run.Status.Relocations))
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

// terminalOutcome is the reason of the True Complete/Failed condition
// (the run's terminal verdict) for the runs_total metric; "Unknown" if none.
func terminalOutcome(run *repackv1alpha1.RepackRun) string {
	for _, c := range run.Status.Conditions {
		if c.Status == metav1.ConditionTrue &&
			(c.Type == state.CondComplete || c.Type == state.CondFailed) {
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
// reported through status.relocations and status.result instead of rewriting
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
	run.Status.Relocations = nil
}

// prepareExecuteRelocations records the complete per-Pod execution journal
// before the eviction barrier. A later commit retains only Pods that require a
// replacement after a directly accepted or indirectly observed removal.
type podGroupPlacementPolicyReader interface {
	PodGroupUsesSubGroupPolicy(schedapi.JobID) bool
}

func prepareExecuteRelocations(
	run *repackv1alpha1.RepackRun,
	plan *engineapi.RepackPlan,
	nominationTTL time.Duration,
	policyReader podGroupPlacementPolicyReader,
) error {
	if run == nil {
		return nil
	}
	relocations, err := buildPodRelocations(plan, nominationTTL, policyReader)
	if err != nil {
		return err
	}
	run.Status.Relocations = relocations
	return nil
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

// realizedFreedNodeNames derives source nodes whose complete planned victim set
// was removed and retained as a relocation record. status.plan remains the
// complete proposal, so a source with a genuinely rejected victim is not excluded.
func realizedFreedNodeNames(run *repackv1alpha1.RepackRun) []string {
	if run == nil || run.Status.Plan == nil {
		return nil
	}
	accepted := make(map[placementIdentity]struct{}, len(run.Status.Relocations))
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		accepted[placementIdentityForRelocation(relocation)] = struct{}{}
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
				key := placementIdentityForMove(move.Namespace, move.PodGroupName, pod.Name, pod.ToNode)
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

// buildPodRelocations renders per-Pod relocation journals. PodGroups
// without SubGroup policies are explicitly treated as homogeneous, so they do
// not pay the API-size cost of storing a hash on every relocation. A SubGroup
// policy opts the PodGroup into hash-based matching for renamed heterogeneous
// replacements.
func buildPodRelocations(
	plan *engineapi.RepackPlan,
	nominationTTL time.Duration,
	policyReader podGroupPlacementPolicyReader,
) ([]repackv1alpha1.PodRelocationStatus, error) {
	if plan == nil {
		return nil, nil
	}
	if policyReader == nil {
		return nil, fmt.Errorf("PodGroup placement policy reader is required")
	}
	expirationTime := metav1.NewTime(time.Now().Add(nominationTTL))
	relocations := make([]repackv1alpha1.PodRelocationStatus, 0, len(plan.Moves))
	for _, move := range plan.Moves {
		if move == nil || move.Task == nil || move.To == move.From {
			continue
		}
		task := move.Task
		_, podGroupName := splitPodGroupID(string(task.Job))
		var victimPodUID types.UID
		if task.Pod != nil {
			victimPodUID = task.Pod.UID
		}
		schedulingRequirementsHash := ""
		if policyReader.PodGroupUsesSubGroupPolicy(task.Job) {
			var err error
			schedulingRequirementsHash, err = placement.SchedulingRequirementsHash(task.Pod)
			if err != nil {
				return nil, fmt.Errorf(
					"derive scheduling requirements for SubGroup victim Pod %s/%s in PodGroup %s: %w",
					task.Namespace, task.Name, task.Job, err)
			}
			klog.V(4).InfoS("repack: recorded scheduling requirements for SubGroup replacement matching",
				"pod", task.Namespace+"/"+task.Name, "podGroup", task.Job,
				"schedulingRequirementsHash", schedulingRequirementsHash)
		}
		relocations = append(relocations, repackv1alpha1.PodRelocationStatus{
			Namespace:                  task.Namespace,
			PodGroupName:               podGroupName,
			VictimPodName:              task.Name,
			VictimPodUID:               victimPodUID,
			SchedulingRequirementsHash: schedulingRequirementsHash,
			PlannedNodeName:            move.To,
			Eviction: repackv1alpha1.PodEvictionStatus{
				Phase: repackv1alpha1.PodEvictionPending,
			},
			Placement: repackv1alpha1.PodPlacementStatus{
				Phase:          repackv1alpha1.PodPlacementWaitingForReplacement,
				ExpirationTime: &expirationTime,
			},
		})
	}
	sort.Slice(relocations, func(left, right int) bool {
		return placementIdentityForRelocation(&relocations[left]).less(
			placementIdentityForRelocation(&relocations[right]))
	})
	return relocations, nil
}
