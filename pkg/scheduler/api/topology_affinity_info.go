/*
Copyright 2017 The Kubernetes Authors.
Copyright 2017-2025 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"volcano.sh/apis/pkg/apis/scheduling"
)

// ContainsHardPodGroupAntiAffinity returns whether the job has hard cross-PodGroup anti-affinity.
func (ji *JobInfo) ContainsHardPodGroupAntiAffinity() bool {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil {
		return false
	}
	anti := ji.PodGroup.Spec.TopologyAffinity.PodGroupAntiAffinity
	return anti != nil && len(anti.Required) > 0
}

// HasPreferredPodGroupAntiAffinity returns whether the job has soft cross-PodGroup anti-affinity.
func (ji *JobInfo) HasPreferredPodGroupAntiAffinity() bool {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil {
		return false
	}
	anti := ji.PodGroup.Spec.TopologyAffinity.PodGroupAntiAffinity
	return anti != nil && len(anti.Preferred) > 0
}

// RequiredPodGroupAntiAffinityTerms returns hard cross-PodGroup anti-affinity terms.
func (ji *JobInfo) RequiredPodGroupAntiAffinityTerms() []scheduling.PodGroupAffinityTerm {
	if !ji.ContainsHardPodGroupAntiAffinity() {
		return nil
	}
	return ji.PodGroup.Spec.TopologyAffinity.PodGroupAntiAffinity.Required
}

// PreferredPodGroupAntiAffinityTerms returns soft cross-PodGroup anti-affinity terms.
func (ji *JobInfo) PreferredPodGroupAntiAffinityTerms() []scheduling.PodGroupAffinityTerm {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil ||
		ji.PodGroup.Spec.TopologyAffinity.PodGroupAntiAffinity == nil {
		return nil
	}
	return ji.PodGroup.Spec.TopologyAffinity.PodGroupAntiAffinity.Preferred
}

// WithTopologyAffinity returns whether the job declares topologyAffinity.
func (ji *JobInfo) WithTopologyAffinity() bool {
	return ji.PodGroup != nil && ji.PodGroup.Spec.TopologyAffinity != nil
}

// ResolvePodGroupTermTier resolves topologyTier or topologyTierName on a PodGroupAffinityTerm.
func ResolvePodGroupTermTier(term scheduling.PodGroupAffinityTerm, tierNameMap HyperNodeTierNameMap) (int, error) {
	if term.TopologyTier != nil && term.TopologyTierName != "" {
		return 0, fmt.Errorf("topologyTier and topologyTierName are mutually exclusive")
	}
	if term.TopologyTier != nil {
		return int(*term.TopologyTier), nil
	}
	if term.TopologyTierName != "" {
		tier, ok := tierNameMap[term.TopologyTierName]
		if !ok {
			return 0, fmt.Errorf("unknown topologyTierName %q", term.TopologyTierName)
		}
		return tier, nil
	}
	return 0, fmt.Errorf("topologyTier or topologyTierName must be set")
}

// PodGroupMatchesTerm reports whether otherJob is selected by term's podGroupSelector (excluding selfJob).
func PodGroupMatchesTerm(term scheduling.PodGroupAffinityTerm, selfJob, otherJob *JobInfo) bool {
	if otherJob == nil || otherJob.PodGroup == nil || selfJob == nil || selfJob.UID == otherJob.UID {
		return false
	}
	if !matchesNamespaceSelector(term.NamespaceSelector, selfJob.Namespace, otherJob.Namespace) {
		return false
	}
	if term.PodGroupSelector == nil {
		return false
	}
	selector, err := metav1.LabelSelectorAsSelector(term.PodGroupSelector)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(otherJob.PodGroup.Labels))
}

func matchesNamespaceSelector(namespaceSelector *metav1.LabelSelector, selfNamespace, otherNamespace string) bool {
	if namespaceSelector == nil {
		return selfNamespace == otherNamespace
	}
	// Cross-namespace matching via namespaceSelector requires Namespace informer (future work).
	return selfNamespace == otherNamespace
}

// getJobAllocatedHyperNode returns job.AllocatedHyperNode when set, otherwise infers it from
// placed tasks (for example matching PodGroups without network topology).
func getJobAllocatedHyperNode(
	job *JobInfo,
	hyperNodes HyperNodeInfoMap,
	nodesByHyperNode map[string]sets.Set[string],
) string {
	if job == nil || len(hyperNodes) == 0 {
		return ""
	}
	if job.AllocatedHyperNode != "" {
		return job.AllocatedHyperNode
	}
	if len(nodesByHyperNode) == 0 {
		return ""
	}

	hyperNodeSet := sets.New[string]()
	for name := range hyperNodes {
		hyperNodeSet.Insert(name)
	}

	var lca string
	for _, subJob := range job.SubJobs {
		subJobHyperNode := subJob.AllocatedHyperNode
		if subJobHyperNode == "" {
			subJobHyperNode = getSubJobAllocatedHyperNodeFromTasks(subJob, hyperNodeSet, nodesByHyperNode, hyperNodes)
		}
		if subJobHyperNode == "" {
			continue
		}
		lca = hyperNodes.GetLCAHyperNode(lca, subJobHyperNode)
	}
	if lca != "" {
		return lca
	}

	return getAllocatedHyperNodeFromTasks(collectJobAllocatedTasks(job), hyperNodeSet, nodesByHyperNode, hyperNodes)
}

func collectJobAllocatedTasks(job *JobInfo) []*TaskInfo {
	tasks := make([]*TaskInfo, 0)
	for _, subJob := range job.SubJobs {
		tasks = append(tasks, collectSubJobAllocatedTasks(subJob)...)
	}
	if len(tasks) > 0 {
		return tasks
	}
	for status, taskMap := range job.TaskStatusIndex {
		if !AllocatedStatus(status) {
			continue
		}
		for _, task := range taskMap {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func collectSubJobAllocatedTasks(subJob *SubJobInfo) []*TaskInfo {
	if subJob == nil {
		return nil
	}
	tasks := make([]*TaskInfo, 0, subJob.AllocatedTaskNum())
	for status, taskMap := range subJob.TaskStatusIndex {
		if !AllocatedStatus(status) {
			continue
		}
		for _, task := range taskMap {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func getSubJobAllocatedHyperNodeFromTasks(
	subJob *SubJobInfo,
	hyperNodeSet sets.Set[string],
	nodesByHyperNode map[string]sets.Set[string],
	hyperNodes HyperNodeInfoMap,
) string {
	return getAllocatedHyperNodeFromTasks(
		collectSubJobAllocatedTasks(subJob), hyperNodeSet, nodesByHyperNode, hyperNodes,
	)
}

func getAllocatedHyperNodeFromTasks(
	tasks []*TaskInfo,
	hyperNodeSet sets.Set[string],
	nodesByHyperNode map[string]sets.Set[string],
	hyperNodes HyperNodeInfoMap,
) string {
	if len(tasks) == 0 {
		return ""
	}

	var candidateHyperNodes sets.Set[string]
	for _, task := range tasks {
		if task.NodeName == "" {
			continue
		}

		search := hyperNodeSet
		if candidateHyperNodes != nil {
			search = candidateHyperNodes
		}

		taskHyperNodes := sets.New[string]()
		for hyperNode := range search {
			if nodes, found := nodesByHyperNode[hyperNode]; found && nodes.Has(task.NodeName) {
				taskHyperNodes.Insert(hyperNode)
			}
		}
		if taskHyperNodes.Len() == 0 {
			return ""
		}
		candidateHyperNodes = taskHyperNodes
	}
	return getLowestTierHyperNode(candidateHyperNodes, hyperNodes)
}

func getLowestTierHyperNode(hyperNodeNames sets.Set[string], hyperNodes HyperNodeInfoMap) string {
	if hyperNodeNames == nil || hyperNodeNames.Len() == 0 {
		return ""
	}

	var lowest *HyperNodeInfo
	for name := range hyperNodeNames {
		hyperNode, found := hyperNodes[name]
		if !found {
			continue
		}
		if lowest == nil || hyperNode.Tier() < lowest.Tier() {
			lowest = hyperNode
		}
	}
	if lowest == nil {
		return ""
	}
	return lowest.Name
}

// MatchingPodGroupsAllocatedHyperNodesForTerm returns ancestor HyperNodes at the term tier
// where matching PodGroups (other than selfJob) are already allocated.
// nodesByHyperNode is used to infer placement for matching PodGroups without AllocatedHyperNode.
func MatchingPodGroupsAllocatedHyperNodesForTerm(
	jobs map[JobID]*JobInfo,
	hyperNodes HyperNodeInfoMap,
	tierNameMap HyperNodeTierNameMap,
	selfJob *JobInfo,
	term scheduling.PodGroupAffinityTerm,
	nodesByHyperNode map[string]sets.Set[string],
) (sets.Set[string], error) {
	tier, err := ResolvePodGroupTermTier(term, tierNameMap)
	if err != nil {
		return nil, err
	}

	matchingHyperNodes := sets.New[string]()
	for _, matchingJob := range jobs {
		if !PodGroupMatchesTerm(term, selfJob, matchingJob) {
			continue
		}
		allocatedHyperNode := getJobAllocatedHyperNode(matchingJob, hyperNodes, nodesByHyperNode)
		resolvedHyperNodes := hyperNodes.ResolveHyperNodesAtTier(allocatedHyperNode, tier)
		klog.V(3).InfoS("podGroup anti-affinity: matching job hyperNode",
			"job", klog.KRef(selfJob.Namespace, selfJob.Name),
			"matchingJob", klog.KRef(matchingJob.Namespace, matchingJob.Name),
			"termTier", tier,
			"allocatedHyperNode", allocatedHyperNode,
			"resolvedHyperNodes", resolvedHyperNodes,
		)
		if allocatedHyperNode == "" {
			continue
		}
		for _, hyperNode := range resolvedHyperNodes {
			matchingHyperNodes.Insert(hyperNode)
		}
	}
	return matchingHyperNodes, nil
}
