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

package engine

import (
	"sort"

	"k8s.io/apimachinery/pkg/types"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
)

func plannedVictims(run *repackv1alpha1.RepackRun) []plannedVictim {
	if run == nil || run.Status.Plan == nil {
		return nil
	}
	relocationIndexes := make(map[placementexecutor.Identity]int, len(run.Status.Relocations))
	for index := range run.Status.Relocations {
		relocationIndexes[placementexecutor.IdentityForRelocation(&run.Status.Relocations[index])] = index
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
			identity := placementexecutor.IdentityForMove(move.Namespace, move.PodGroupName, pod.Name, pod.ToNode)
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
	accepted := make(map[placementexecutor.Identity]struct{}, len(run.Status.Relocations))
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if relocation.Eviction.Phase == repackv1alpha1.PodEvictionAccepted {
			accepted[placementexecutor.IdentityForRelocation(relocation)] = struct{}{}
		}
	}
	var movedCards int64
	for moveIndex := range run.Status.Plan.Moves {
		move := &run.Status.Plan.Moves[moveIndex]
		for podIndex := range move.Pods {
			pod := &move.Pods[podIndex]
			if _, found := accepted[placementexecutor.IdentityForMove(
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
