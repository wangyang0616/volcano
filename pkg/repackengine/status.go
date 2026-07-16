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
)

func (e *Engine) fail(ctx context.Context, run *repackv1alpha1.RepackRun, generation int64, reason string, err error) error {
	klog.ErrorS(err, "repack: run failed", "run", run.Name, "reason", reason)
	state.SetCondition(&run.Status.Conditions, state.CondProgressing, metav1.ConditionFalse, reason, err.Error(), generation)
	state.SetCondition(&run.Status.Conditions, state.CondFailed, metav1.ConditionTrue, reason, err.Error(), generation)
	run.Status.Phase = state.DerivePhase(run.Status.Conditions)
	return e.updateStatusTerminal(ctx, run)
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
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		// Re-apply the intended status onto the freshest object. Nomination phase is
		// controller-owned; preserve a concurrently observed Bound/Expired phase
		// instead of resetting it to Pending during the engine's terminal write.
		merged := desired.DeepCopy()
		mergeNominationPhases(merged.Nominations, latest.Status.Nominations)
		merged.DeepCopyInto(&latest.Status)
		_, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, latest, metav1.UpdateOptions{})
		return err
	})
}

func mergeNominationPhases(desired, latest []repackv1alpha1.PodNomination) {
	phases := make(map[string]repackv1alpha1.PodNominationPhase, len(latest))
	for i := range latest {
		r := &latest[i]
		if r.Phase == repackv1alpha1.PodNominationBound || r.Phase == repackv1alpha1.PodNominationExpired {
			phases[nominationKey(r)] = r.Phase
		}
	}
	for i := range desired {
		if phase := phases[nominationKey(&desired[i])]; phase != "" {
			desired[i].Phase = phase
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
		e.recorder.Event(run, etype, outcome, "repack run reached a terminal state")
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

// applyPlan maps the search outcome onto status.plan — the SAME shape for both
// modes: DryRun = predicted plan, Execute = executed plan. Each move carries the
// planned target node (fromNode -> toNode), visible in DryRun too. Execute also
// writes the durable status.nominations[] (consumed by the controller's nomination
// reconciler) and marks freed nodes as actuallyFreed. Per-move actual-landing /
// drift (outcome/actualNode) is filled later by the reconciler as replacement pods
// land; the engine's initial write leaves it empty.
func applyPlan(run *repackv1alpha1.RepackRun, report engineframework.Report, plan *engineapi.RepackPlan, targetResource v1.ResourceName, owners map[string]*repackv1alpha1.WorkloadRef, execute bool, nominationTTL time.Duration) {
	moves := buildStatusMoves(plan, targetResource, owners)
	summary := buildRepackSummary(report)
	if summary != nil {
		var cards int64
		for _, m := range moves {
			cards += m.Cards
		}
		summary.MovedCardCount = cards
	}
	run.Status.Plan = &repackv1alpha1.RepackPlan{
		Summary:    summary,
		Moves:      moves,
		FreedNodes: sortedFreedNodeNames(plan),
	}
	if execute {
		run.Status.Nominations = buildPodNominations(plan, nominationTTL)
	}
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

// buildRepackSummary renders the flat metrics layer. "Worth repacking?" is not here — it
// is folded into the terminal condition's reason. MovedCardCount is filled by
// applyPlan from moves; FragBefore/After come from the report (absolute rate).
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

// buildPodNominations renders per-pod landing-steering intents (Execute-only). Claiming
// follows the landing-identity contract (proposal §5.2.2): victimPodName exact
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
			Phase:          repackv1alpha1.PodNominationPending,
			ExpirationTime: &expirationTime,
		})
	}
	return nominations
}
