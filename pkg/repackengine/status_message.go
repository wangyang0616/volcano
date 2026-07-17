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
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
)

// The helpers in this file render the compact, operator-facing status surface.
// Stable machine decisions remain in Condition.Reason; these messages explain
// the scope, result, and next diagnostic action without duplicating move lists.

func completionStatusMessage(run *repackv1alpha1.RepackRun, targetResource v1.ResourceName, reason string) string {
	resource := displayResource(targetResource)
	summary := runSummary(run)
	switch reason {
	case state.ReasonRepackRecommended:
		return fmt.Sprintf(
			"Repack recommended for %s: move %d PodGroups and %d cards to free %d nodes; cluster fragmentation is expected to improve from %d%% to %d%%.",
			resource, summary.podGroups, summary.movedCards, summary.freedNodes, summary.fragBefore, summary.fragAfter)
	case state.ReasonExecuted:
		result := runResult(run)
		return fmt.Sprintf(
			"Repack completed for %s: moved %d PodGroups and %d cards, actually freed %d nodes; cluster fragmentation changed from %d%% to %d%%.",
			resource, acceptedPodGroupCount(run), result.movedCards, result.freedNodes, summary.fragBefore, result.fragAfter)
	case state.ReasonNoFragmentation:
		return fmt.Sprintf(
			"No repack needed for %s: cluster fragmentation is %d%%; the resolved scope contains %d resource nodes and %d target-resource PodGroups.",
			resource, summary.fragBefore, summary.scopeNodes, summary.scopePodGroups)
	case state.ReasonBelowGoalThreshold:
		return fmt.Sprintf(
			"No repack performed for %s: cluster fragmentation is %d%%, but no feasible plan within the resolved scope met the required %d percentage-point improvement.",
			resource, summary.fragBefore, minFragImprovement(run))
	default:
		return fmt.Sprintf("Repack for %s completed with outcome %s.", resource, reason)
	}
}

func placementStatusMessage(run *repackv1alpha1.RepackRun, targetResource v1.ResourceName, degraded, metricsUnverified bool) string {
	plan := runSummary(run)
	result := runResult(run)
	placed, drifted, expired := placementOutcomeCounts(run)
	if !degraded {
		return fmt.Sprintf(
			"Repack completed for %s: all %d replacement Pods reached their selected nodes, %d nodes were actually freed; cluster fragmentation changed from %d%% to %d%%.",
			displayResource(targetResource), placed, result.freedNodes, plan.fragBefore, result.fragAfter)
	}
	if metricsUnverified {
		return fmt.Sprintf(
			"Repack placement degraded for %s: replacement bindings were reported but could not be verified in a coherent scheduler snapshot before the deadline; no node-freeing benefit is claimed.",
			displayResource(targetResource))
	}
	return fmt.Sprintf(
		"Repack placement degraded for %s: %d replacement Pods reached their selected nodes, %d were placed elsewhere, and %d expired; %d nodes were actually freed and cluster fragmentation changed from %d%% to %d%%.",
		displayResource(targetResource), placed, drifted, expired, result.freedNodes, plan.fragBefore, result.fragAfter)
}

func failureStatusMessage(targetResource v1.ResourceName, reason string, err error) string {
	detail := "unknown error"
	if err != nil {
		detail = strings.TrimSpace(err.Error())
	}
	message := fmt.Sprintf("Repack failed for %s during %s: %s", displayResource(targetResource), failureStage(reason), detail)
	if reason == state.ReasonExecuteFailed &&
		(strings.Contains(strings.ToLower(detail), "evict") || strings.Contains(strings.ToLower(detail), "reject")) {
		message += "; check PodDisruptionBudgets and Kubernetes eviction events"
	}
	return strings.TrimRight(message, ".") + "."
}

func placementProgressMessage(run *repackv1alpha1.RepackRun, targetResource v1.ResourceName) string {
	total := len(run.Status.Nominations)
	placed, drifted, expired := placementOutcomeCounts(run)
	remaining := total - placed - drifted - expired
	return fmt.Sprintf(
		"Waiting for replacement placement for %s: %d of %d Pods placed, %d placed elsewhere, %d expired, and %d still pending.",
		displayResource(targetResource), placed, total, drifted, expired, remaining)
}

type statusSummaryValues struct {
	podGroups      int
	movedCards     int64
	freedNodes     int32
	fragBefore     int32
	fragAfter      int32
	scopeNodes     int32
	scopePodGroups int32
}

func runSummary(run *repackv1alpha1.RepackRun) statusSummaryValues {
	var result statusSummaryValues
	if run == nil || run.Status.Plan == nil {
		return result
	}
	result.podGroups = len(run.Status.Plan.Moves)
	if run.Status.Plan.Summary == nil {
		return result
	}
	summary := run.Status.Plan.Summary
	result.movedCards = summary.MovedCardCount
	result.freedNodes = summary.FreedNodeCount
	result.fragBefore = summary.FragBeforePercent
	result.fragAfter = summary.FragAfterPercent
	if summary.ResolvedScope != nil {
		result.scopeNodes = summary.ResolvedScope.NodeCount
		result.scopePodGroups = summary.ResolvedScope.PodGroupCount
	}
	return result
}

func runResult(run *repackv1alpha1.RepackRun) statusSummaryValues {
	var result statusSummaryValues
	if run == nil || run.Status.Result == nil {
		return result
	}
	result.movedCards = run.Status.Result.MovedCardCount
	result.freedNodes = run.Status.Result.FreedNodeCount
	result.fragAfter = run.Status.Result.FragAfterPercent
	return result
}

func placementOutcomeCounts(run *repackv1alpha1.RepackRun) (placed, drifted, expired int) {
	if run == nil {
		return 0, 0, 0
	}
	for index := range run.Status.Nominations {
		switch run.Status.Nominations[index].Phase {
		case repackv1alpha1.PodPlacementPlaced:
			placed++
		case repackv1alpha1.PodPlacementDegraded:
			drifted++
		case repackv1alpha1.PodPlacementExpired:
			expired++
		}
	}
	return placed, drifted, expired
}

func acceptedPodGroupCount(run *repackv1alpha1.RepackRun) int {
	if run == nil {
		return 0
	}
	podGroups := make(map[string]struct{})
	for index := range run.Status.Nominations {
		nomination := &run.Status.Nominations[index]
		podGroups[nomination.Namespace+"\x00"+nomination.PodGroupName] = struct{}{}
	}
	return len(podGroups)
}

func displayResource(targetResource v1.ResourceName) string {
	if targetResource == "" {
		return "the target resource"
	}
	return string(targetResource)
}

func failureStage(reason string) string {
	switch reason {
	case "NoTargetResource", "UnsupportedResource":
		return "target resource validation"
	case "InvalidEngineConfiguration":
		return "engine configuration validation"
	case "ScopeError":
		return "scope resolution"
	case state.ReasonExecuteFailed:
		return "execution"
	case state.ReasonPlacementDegraded:
		return "replacement placement"
	case "Interrupted":
		return "engine recovery"
	case "ReconcileGaveUp":
		return "reconciliation"
	default:
		return reason
	}
}
