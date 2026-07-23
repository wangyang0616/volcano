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
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

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
	case state.ReasonExecutedWithPlacementDrift:
		result := runResult(run)
		_, drifted, _ := placementOutcomeCounts(run)
		return fmt.Sprintf(
			"Repack completed for %s with %d %s: all planned nodes were verified free, %d cards were moved, and cluster fragmentation changed from %d%% to %d%%.",
			resource, drifted, pluralNoun(drifted, "placement drift", "placement drifts"),
			result.movedCards, summary.fragBefore, result.fragAfter)
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

func placementStatusMessage(run *repackv1alpha1.RepackRun, targetResource v1.ResourceName, decision placementTerminalDecision) string {
	plan := runSummary(run)
	result := runResult(run)
	placed, drifted, expired := placementOutcomeCounts(run)
	resource := displayResource(targetResource)
	plannedNodes := formatNodeNames(decision.Nodes.Planned)
	actualNodes := formatNodeNames(decision.Nodes.Actual)
	switch decision.Reason {
	case state.ReasonExecuted:
		return fmt.Sprintf(
			"Repack succeeded for %s: all %d replacement %s were scheduled and all %d planned %s were verified free [%s]; cluster fragmentation changed from %d%% to %d%%.",
			resource, placed, pluralNoun(placed, "Pod", "Pods"),
			len(decision.Nodes.Planned), pluralNoun(len(decision.Nodes.Planned), "node", "nodes"),
			plannedNodes, plan.fragBefore, result.fragAfter) + podGroupReplacementStatusSuffix(run)
	case state.ReasonExecutedWithPlacementDrift:
		return fmt.Sprintf(
			"Repack succeeded for %s despite placement drift: all %d replacement %s were scheduled (%d reached selected nodes and %d were placed elsewhere), and all %d planned %s were verified free [%s]; cluster fragmentation changed from %d%% to %d%%.",
			resource, placed+drifted, pluralNoun(placed+drifted, "Pod", "Pods"), placed, drifted,
			len(decision.Nodes.Planned), pluralNoun(len(decision.Nodes.Planned), "node", "nodes"),
			plannedNodes, plan.fragBefore, result.fragAfter) + podGroupReplacementStatusSuffix(run)
	case state.ReasonPlacementExpired:
		return fmt.Sprintf(
			"Repack failed for %s because %d replacement %s did not bind before the placement deadline; %d reached selected nodes and %d were placed elsewhere. Planned nodes [%s] were not accepted as a verified complete result; inspect Pod scheduling events and available receiver capacity.",
			resource, expired, pluralNoun(expired, "Pod", "Pods"), placed, drifted, plannedNodes) +
			podGroupReplacementStatusSuffix(run)
	case state.ReasonMetricsUnverified:
		return fmt.Sprintf(
			"Repack failed verification for %s: replacement bindings were reported, but the scheduler cache did not expose one coherent terminal snapshot before the deadline. Planned nodes [%s] cannot be confirmed free; inspect scheduler cache and Pod informer synchronization.",
			resource, plannedNodes) + podGroupReplacementStatusSuffix(run)
	case state.ReasonBenefitNotRealized:
		return fmt.Sprintf(
			"Repack did not realize the planned benefit for %s: planned to free %d %s [%s], but verified %d %s free [%s]; nodes still occupied or unavailable: [%s]. All %d replacement %s were scheduled (%d %s); inspect target-resource usage on the missing nodes.",
			resource, len(decision.Nodes.Planned), pluralNoun(len(decision.Nodes.Planned), "node", "nodes"), plannedNodes,
			len(decision.Nodes.Actual), pluralNoun(len(decision.Nodes.Actual), "node", "nodes"), actualNodes,
			formatNodeNames(decision.Nodes.Missing), placed+drifted, pluralNoun(placed+drifted, "Pod", "Pods"),
			drifted, pluralNoun(drifted, "placement drift", "placement drifts")) +
			podGroupReplacementStatusSuffix(run)
	default:
		return fmt.Sprintf(
			"Repack for %s reached terminal placement outcome %s: planned nodes [%s], actually free nodes [%s].",
			resource, decision.Reason, plannedNodes, actualNodes)
	}
}

func formatNodeNames(nodeNames []string) string {
	const maxDisplayedNodes = 8
	if len(nodeNames) == 0 {
		return "none"
	}
	if len(nodeNames) <= maxDisplayedNodes {
		return strings.Join(nodeNames, ", ")
	}
	return fmt.Sprintf("%s, ... (%d more)", strings.Join(nodeNames[:maxDisplayedNodes], ", "), len(nodeNames)-maxDisplayedNodes)
}

func pluralNoun(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
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
		displayResource(targetResource), placed, total, drifted, expired, remaining) +
		podGroupReplacementStatusSuffix(run)
}

func podGroupReplacementStatusSuffix(run *repackv1alpha1.RepackRun) string {
	if run == nil {
		return ""
	}
	replacements := map[string]struct{}{}
	for index := range run.Status.Nominations {
		nomination := &run.Status.Nominations[index]
		if nomination.Namespace == "" || nomination.PodGroupName == "" ||
			nomination.ReplacementPodGroupName == "" ||
			nomination.ReplacementPodGroupName == nomination.PodGroupName {
			continue
		}
		replacements[fmt.Sprintf("%s/%s -> %s/%s",
			nomination.Namespace, nomination.PodGroupName,
			nomination.Namespace, nomination.ReplacementPodGroupName)] = struct{}{}
	}
	if len(replacements) == 0 {
		return ""
	}
	values := make([]string, 0, len(replacements))
	for replacement := range replacements {
		values = append(values, replacement)
	}
	sort.Strings(values)
	const maxDisplayedReplacements = 4
	if len(values) > maxDisplayedReplacements {
		return fmt.Sprintf(" PodGroup replacements: %s, ... (%d more).",
			strings.Join(values[:maxDisplayedReplacements], ", "),
			len(values)-maxDisplayedReplacements)
	}
	return " PodGroup replacements: " + strings.Join(values, ", ") + "."
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
	podGroups := make(map[types.NamespacedName]struct{})
	for index := range run.Status.Nominations {
		nomination := &run.Status.Nominations[index]
		podGroups[types.NamespacedName{
			Namespace: nomination.Namespace,
			Name:      nomination.PodGroupName,
		}] = struct{}{}
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
	case state.ReasonBenefitNotRealized:
		return "benefit verification"
	case state.ReasonPlacementExpired:
		return "replacement placement"
	case state.ReasonMetricsUnverified:
		return "terminal metric verification"
	case "Interrupted":
		return "engine recovery"
	case "ReconcileGaveUp":
		return "reconciliation"
	default:
		return reason
	}
}
