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

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

// plannedVictim is the minimum immutable data needed to resume an eviction
// without reopening a scheduler planning session.
type plannedVictim struct {
	relocationIndex int
	namespace       string
	podGroupName    string
	podName         string
	sourceNode      string
	targetNode      string
	freesNode       bool
}

type evictionSummary struct {
	accepted          int
	indirectlyRemoved int
	rejected          int
}

// executePreparedEvictions advances the durable per-Pod eviction journal. Every
// API call is preceded by an InProgress status barrier and followed by a durable
// outcome. If the engine crashes after the API call, the next reconcile observes
// the original Pod UID before deciding whether the request needs to be retried.
func (e *Engine) executePreparedEvictions(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	generation int64,
	targetResource v1.ResourceName,
) error {
	return e.executePreparedEvictionsWithClient(
		ctx, run, generation, targetResource, e.schedulerCache.Client())
}

func (e *Engine) executePreparedEvictionsWithClient(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	generation int64,
	targetResource v1.ResourceName,
	kubernetesClient kubernetes.Interface,
) error {
	if run == nil || run.Status.Plan == nil {
		return fmt.Errorf("resume evictions: durable plan is missing")
	}
	preparedPlacementGroups := plannedPodGroups(run)
	victims := plannedVictims(run)
	if len(victims) == 0 {
		return e.fail(ctx, run, generation, state.ReasonEvictionFailed,
			fmt.Errorf("resume evictions: durable plan contains no matching relocation"))
	}

	evictionHook := hooksFor(run, kubernetesClient).Evict
	if evictionHook == nil {
		return e.fail(ctx, run, generation, state.ReasonEvictionFailed,
			fmt.Errorf("resume evictions: eviction hook is not configured"))
	}

	missingVictims := make(map[int]string)
	for _, victim := range victims {
		relocation := &run.Status.Relocations[victim.relocationIndex]
		if evictionOutcomeIsFinal(relocation.Eviction.Phase) {
			continue
		}

		pod, err := kubernetesClient.CoreV1().Pods(victim.namespace).Get(ctx, victim.podName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if relocation.Eviction.Phase == repackv1alpha1.PodEvictionInProgress {
				if err := e.persistEvictionOutcome(ctx, run, relocation, repackv1alpha1.PodEvictionAccepted,
					"Victim Pod disappeared after the durable eviction intent; treating the request as accepted during recovery."); err != nil {
					return err
				}
			} else {
				missingVictims[victim.relocationIndex] = "Victim Pod was absent before this eviction attempt."
			}
			continue
		case err != nil:
			return fmt.Errorf("observe victim Pod %s/%s before eviction: %w", victim.namespace, victim.podName, err)
		}

		originalInstanceGone := relocation.VictimPodUID != "" && pod.UID != relocation.VictimPodUID
		originalInstanceTerminating := pod.DeletionTimestamp != nil &&
			(relocation.VictimPodUID == "" || pod.UID == relocation.VictimPodUID)
		if originalInstanceGone || originalInstanceTerminating {
			if relocation.Eviction.Phase == repackv1alpha1.PodEvictionInProgress {
				if err := e.persistEvictionOutcome(ctx, run, relocation, repackv1alpha1.PodEvictionAccepted,
					"The original victim is terminating or gone after the durable eviction intent; treating the request as accepted during recovery."); err != nil {
					return err
				}
			} else {
				missingVictims[victim.relocationIndex] = "The planned victim was already terminating or had been replaced before this eviction attempt."
			}
			continue
		}

		if relocation.VictimPodUID == "" {
			relocation.VictimPodUID = pod.UID
		}
		relocation.Eviction.Phase = repackv1alpha1.PodEvictionInProgress
		relocation.Eviction.Message = "Eviction intent is durable; the Eviction API request may now be issued."
		if err := e.updateStatus(ctx, run); err != nil {
			return fmt.Errorf("persist eviction intent for Pod %s/%s: %w", victim.namespace, victim.podName, err)
		}

		task := schedapi.NewTaskInfo(pod.DeepCopy())
		task.Job = schedapi.JobID(victim.namespace + "/" + victim.podGroupName)
		move := &engineapi.Move{Task: task, From: victim.sourceNode, To: victim.targetNode}
		evictionErr := evictionHook(move)
		phase := repackv1alpha1.PodEvictionAccepted
		message := "Eviction API accepted the planned victim."
		if evictionErr != nil {
			phase = repackv1alpha1.PodEvictionRejected
			message = evictionErr.Error()
			if errors.Is(evictionErr, engineframework.ErrVictimNotFound) {
				missingVictims[victim.relocationIndex] = "The Eviction API reported that the planned victim Pod was not found."
				continue
			}
		}
		if err := e.persistEvictionOutcome(ctx, run, relocation, phase, message); err != nil {
			return err
		}
		klog.V(4).InfoS("repack: durable eviction outcome recorded",
			"run", run.Name, "pod", victim.namespace+"/"+victim.podName,
			"victimPodUID", relocation.VictimPodUID, "phase", phase,
			"fromNode", victim.sourceNode, "plannedNode", victim.targetNode,
			"message", message)
	}

	// A workload-level recreation may indirectly remove remaining siblings after
	// one accepted eviction. Missing is an in-memory observation, not a public
	// phase. Persist only the final outcome after every victim has been visited so
	// classification is deterministic regardless of Pod order.
	if classifyMissingVictims(run.Status.Relocations, missingVictims) {
		if err := e.updateStatus(ctx, run); err != nil {
			return fmt.Errorf("persist indirect victim removal classification: %w", err)
		}
	}

	summary := summarizeEvictions(run.Status.Relocations)
	plannedVictimCount := plannedVictimCount(run)
	if classifiedCount := summary.accepted + summary.indirectlyRemoved + summary.rejected; classifiedCount < plannedVictimCount {
		// Rejected relocations are removed after the durable result barrier. If a
		// later placement-reconciliation write is retried, recover their count from the
		// immutable plan instead of losing it from operator output.
		summary.rejected += plannedVictimCount - classifiedCount
	}
	klog.V(3).InfoS("repack: durable eviction journal completed",
		"run", run.Name, "acceptedCount", summary.accepted,
		"indirectlyRemovedCount", summary.indirectlyRemoved, "rejectedCount", summary.rejected)

	retainSuccessfulRelocations(run)
	initializeExecuteResultFromStatus(run)
	groupsToRelease := placementGroupsDifference(preparedPlacementGroups, placementPodGroups(run))

	// Always persist the accepted subset and result before releasing a lease or
	// entering placement. This is the commit barrier for the complete eviction
	// journal, including the all-groups-retained case.
	if err := e.updateStatus(ctx, run); err != nil {
		return fmt.Errorf("persist completed eviction journal before lease cleanup: %w", err)
	}
	if err := e.releasePlacementLeases(ctx, run, groupsToRelease); err != nil {
		markExecuteBenefitUnverified(run)
		return e.fail(ctx, run, generation, state.ReasonEvictionFailed, err)
	}

	realizedFreedNodes := realizedFreedNodeNames(run)
	if summary.accepted == 0 {
		e.observeEvictionSummary(run, summary)
		return e.fail(ctx, run, generation, state.ReasonEvictionFailed,
			fmt.Errorf("all %d planned evictions were rejected; no Pods were moved", summary.rejected))
	}
	if len(realizedFreedNodes) == 0 {
		e.observeEvictionSummary(run, summary)
		return e.fail(ctx, run, generation, state.ReasonEvictionFailed,
			fmt.Errorf("Eviction API accepted %d Pods and workload recreation removed %d additional planned Pods, but no planned node was fully vacated (%d requests rejected)",
				summary.accepted, summary.indirectlyRemoved, summary.rejected))
	}

	state.MarkRunning(run, state.ReasonReconcilingPlacements, placementProgressMessage(run, targetResource))
	if err := e.updateStatus(ctx, run); err != nil {
		return fmt.Errorf("persist awaiting placement status: %w", err)
	}
	e.recordRunEvent(run, v1.EventTypeNormal, eventReasonReconcilingPlacements,
		placementProgressMessage(run, targetResource))
	e.observeEvictionSummary(run, summary)
	klog.V(3).InfoS("repack: accepted evictions are awaiting replacement placement",
		"run", run.Name, "acceptedCount", summary.accepted,
		"indirectlyRemovedCount", summary.indirectlyRemoved,
		"rejectedCount", summary.rejected, "realizedFreedNodes", realizedFreedNodes,
		"relocationCount", len(run.Status.Relocations))
	e.workQueue.AddAfter(run.Name, placementRetryInterval)
	return nil
}

func (e *Engine) observeEvictionSummary(run *repackv1alpha1.RepackRun, summary evictionSummary) {
	metrics.ObserveEvictions(summary.accepted, summary.rejected)
	metrics.ObserveIndirectRemovals(summary.indirectlyRemoved)
	eventType := v1.EventTypeNormal
	if summary.rejected > 0 {
		eventType = v1.EventTypeWarning
	}
	e.recordRunEvent(run, eventType, eventReasonEvictionsIssued,
		fmt.Sprintf("Eviction API accepted %d Pods; %d additional planned Pods were indirectly removed; %d requests were rejected.",
			summary.accepted, summary.indirectlyRemoved, summary.rejected))
	if summary.indirectlyRemoved > 0 {
		e.recordRunEvent(run, v1.EventTypeNormal, eventReasonIndirectRemovalObserved,
			fmt.Sprintf("Retained %d replacement placements after their original Pods were indirectly removed.",
				summary.indirectlyRemoved))
	}
}

func classifyMissingVictims(relocations []repackv1alpha1.PodRelocationStatus, missingVictims map[int]string) bool {
	if len(missingVictims) == 0 {
		return false
	}
	acceptedPodGroups := map[string]struct{}{}
	for index := range relocations {
		relocation := &relocations[index]
		if relocation.Eviction.Phase == repackv1alpha1.PodEvictionAccepted {
			acceptedPodGroups[relocation.Namespace+"/"+relocation.PodGroupName] = struct{}{}
		}
	}
	for index, observation := range missingVictims {
		if index < 0 || index >= len(relocations) {
			continue
		}
		relocation := &relocations[index]
		if _, found := acceptedPodGroups[relocation.Namespace+"/"+relocation.PodGroupName]; found {
			relocation.Eviction.Phase = repackv1alpha1.PodEvictionIndirectlyRemoved
			relocation.Eviction.Message = observation +
				" Another eviction in the same PodGroup was accepted, so Repack is treating this victim as indirectly removed and retaining replacement placement."
		} else {
			relocation.Eviction.Phase = repackv1alpha1.PodEvictionRejected
			relocation.Eviction.Message = observation +
				" No accepted eviction in the same PodGroup supports an indirect removal, so replacement placement will not be attempted."
		}
	}
	return true
}

func (e *Engine) persistEvictionOutcome(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	relocation *repackv1alpha1.PodRelocationStatus,
	phase repackv1alpha1.PodEvictionPhase,
	message string,
) error {
	relocation.Eviction.Phase = phase
	relocation.Eviction.Message = message
	if err := e.updateStatus(ctx, run); err != nil {
		return fmt.Errorf("persist eviction outcome %s for Pod %s/%s: %w",
			phase, relocation.Namespace, relocation.VictimPodName, err)
	}
	return nil
}

func plannedVictims(run *repackv1alpha1.RepackRun) []plannedVictim {
	if run == nil || run.Status.Plan == nil {
		return nil
	}
	relocationIndexes := make(map[placementIdentity]int, len(run.Status.Relocations))
	for index := range run.Status.Relocations {
		relocationIndexes[placementIdentityForRelocation(&run.Status.Relocations[index])] = index
	}
	freedNodes := make(map[string]struct{}, len(run.Status.Plan.FreedNodes))
	for _, nodeName := range run.Status.Plan.FreedNodes {
		freedNodes[nodeName] = struct{}{}
	}
	var victims []plannedVictim
	for moveIndex := range run.Status.Plan.Moves {
		move := &run.Status.Plan.Moves[moveIndex]
		for podIndex := range move.Pods {
			pod := &move.Pods[podIndex]
			identity := placementIdentityForMove(move.Namespace, move.PodGroupName, pod.Name, pod.ToNode)
			relocationIndex, found := relocationIndexes[identity]
			if !found {
				continue
			}
			_, freesNode := freedNodes[pod.FromNode]
			victims = append(victims, plannedVictim{
				relocationIndex: relocationIndex,
				namespace:       move.Namespace,
				podGroupName:    move.PodGroupName,
				podName:         pod.Name,
				sourceNode:      pod.FromNode,
				targetNode:      pod.ToNode,
				freesNode:       freesNode,
			})
		}
	}
	sort.SliceStable(victims, func(left, right int) bool {
		if victims[left].freesNode != victims[right].freesNode {
			return victims[left].freesNode
		}
		if victims[left].namespace != victims[right].namespace {
			return victims[left].namespace < victims[right].namespace
		}
		return victims[left].podName < victims[right].podName
	})
	return victims
}

func plannedPodGroups(run *repackv1alpha1.RepackRun) map[types.NamespacedName]struct{} {
	groups := make(map[types.NamespacedName]struct{})
	if run == nil || run.Status.Plan == nil {
		return groups
	}
	for index := range run.Status.Plan.Moves {
		move := &run.Status.Plan.Moves[index]
		if move.Namespace != "" && move.PodGroupName != "" {
			groups[types.NamespacedName{Namespace: move.Namespace, Name: move.PodGroupName}] = struct{}{}
		}
	}
	return groups
}

func plannedVictimCount(run *repackv1alpha1.RepackRun) int {
	if run == nil || run.Status.Plan == nil {
		return 0
	}
	count := 0
	for moveIndex := range run.Status.Plan.Moves {
		count += len(run.Status.Plan.Moves[moveIndex].Pods)
	}
	return count
}

func evictionOutcomeIsFinal(phase repackv1alpha1.PodEvictionPhase) bool {
	switch phase {
	case repackv1alpha1.PodEvictionAccepted,
		repackv1alpha1.PodEvictionIndirectlyRemoved,
		repackv1alpha1.PodEvictionRejected:
		return true
	default:
		return false
	}
}

func summarizeEvictions(relocations []repackv1alpha1.PodRelocationStatus) evictionSummary {
	var summary evictionSummary
	for index := range relocations {
		switch relocations[index].Eviction.Phase {
		case repackv1alpha1.PodEvictionAccepted:
			summary.accepted++
		case repackv1alpha1.PodEvictionIndirectlyRemoved:
			summary.indirectlyRemoved++
		case repackv1alpha1.PodEvictionRejected:
			summary.rejected++
		}
	}
	return summary
}

func retainSuccessfulRelocations(run *repackv1alpha1.RepackRun) {
	retained := make([]repackv1alpha1.PodRelocationStatus, 0, len(run.Status.Relocations))
	for index := range run.Status.Relocations {
		relocation := run.Status.Relocations[index]
		if relocation.Eviction.Phase == repackv1alpha1.PodEvictionAccepted ||
			relocation.Eviction.Phase == repackv1alpha1.PodEvictionIndirectlyRemoved {
			retained = append(retained, relocation)
		}
	}
	run.Status.Relocations = retained
}

func initializeExecuteResultFromStatus(run *repackv1alpha1.RepackRun) {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil {
		return
	}
	accepted := make(map[placementIdentity]struct{}, len(run.Status.Relocations))
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if relocation.Eviction.Phase == repackv1alpha1.PodEvictionAccepted {
			accepted[placementIdentityForRelocation(relocation)] = struct{}{}
		}
	}
	var movedCards int64
	for moveIndex := range run.Status.Plan.Moves {
		move := &run.Status.Plan.Moves[moveIndex]
		for podIndex := range move.Pods {
			pod := &move.Pods[podIndex]
			if _, found := accepted[placementIdentityForMove(
				move.Namespace, move.PodGroupName, pod.Name, pod.ToNode)]; found {
				movedCards += pod.Cards
			}
		}
	}
	run.Status.Result = &repackv1alpha1.RepackResult{
		FragAfterPercent: run.Status.Plan.Summary.FragBeforePercent,
		MovedCardCount:   movedCards,
		MetricsVerified:  false,
	}
}
