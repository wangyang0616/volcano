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

// MatchingPodGroupsAllocatedHyperNodesForTerm returns ancestor HyperNodes at the term tier
// where matching PodGroups (other than selfJob) are already allocated.
func MatchingPodGroupsAllocatedHyperNodesForTerm(
	jobs map[JobID]*JobInfo,
	hyperNodes HyperNodeInfoMap,
	tierNameMap HyperNodeTierNameMap,
	selfJob *JobInfo,
	term scheduling.PodGroupAffinityTerm,
) (sets.Set[string], error) {
	tier, err := ResolvePodGroupTermTier(term, tierNameMap)
	if err != nil {
		return nil, err
	}

	matchingHyperNodes := sets.New[string]()
	for _, otherJob := range jobs {
		if !PodGroupMatchesTerm(term, selfJob, otherJob) || otherJob.AllocatedHyperNode == "" {
			continue
		}
		ancestorHyperNode := hyperNodes.GetAncestorHyperNode(otherJob.AllocatedHyperNode, tier)
		if ancestorHyperNode != "" {
			matchingHyperNodes.Insert(ancestorHyperNode)
		}
	}
	return matchingHyperNodes, nil
}
