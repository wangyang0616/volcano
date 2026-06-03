/*
Copyright 2025 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the License and the specific language governing permissions and
limitations under the License.
*/

package grouptopologyaffinity

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	k8sFramework "k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/utils/set"

	scheduling "volcano.sh/apis/pkg/apis/scheduling"
	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/framework"
)

const (
	PluginName    = "group-topology-affinity"
	PluginWeight  = "weight"
	DefaultWeight = 1
	FullScore     = 1.0
	ZeroScore     = 0.0
)

type groupTopologyAffinityPlugin struct {
	pluginArguments framework.Arguments
	weight          int
	hyperNodeAffinityCache *hyperNodeAffinityCache
}

func New(arguments framework.Arguments) framework.Plugin {
	weight := DefaultWeight
	arguments.GetInt(&weight, PluginWeight)
	if weight < 0 {
		weight = DefaultWeight
	}
	return &groupTopologyAffinityPlugin{
		pluginArguments: arguments,
		weight:          weight,
	}
}

func (gta *groupTopologyAffinityPlugin) Name() string {
	return PluginName
}

func (gta *groupTopologyAffinityPlugin) OnSessionOpen(ssn *framework.Session) {
	gta.hyperNodeAffinityCache = buildHyperNodeAffinityCache(ssn.Jobs, ssn.HyperNodes, ssn.HyperNodeTierNameMap)

	ssn.AddHyperNodeGradientForJobFn(gta.Name(), func(job *api.JobInfo, hyperNode *api.HyperNodeInfo) [][]*api.HyperNodeInfo {
		return gta.hyperNodeGradientForJob(ssn, job, hyperNode)
	})

	ssn.AddHyperNodeOrderFn(gta.Name(), func(subJob *api.SubJobInfo, hyperNodes map[string][]*api.NodeInfo) (map[string]float64, error) {
		job, ok := ssn.Jobs[subJob.Job]
		if !ok {
			return nil, nil
		}
		return gta.hyperNodeOrderFn(ssn, job, hyperNodes)
	})

	ssn.AddEventHandler(&framework.EventHandler{
		AllocateFunc: func(event *framework.Event) {
			job, ok := ssn.Jobs[event.Task.Job]
			if !ok {
				return
			}
			gta.syncHyperNodeAffinityCache(ssn, job)
		},
		DeallocateFunc: func(event *framework.Event) {
			job, ok := ssn.Jobs[event.Task.Job]
			if !ok {
				return
			}
			gta.syncHyperNodeAffinityCache(ssn, job)
		},
	})
}

func (gta *groupTopologyAffinityPlugin) syncHyperNodeAffinityCache(ssn *framework.Session, job *api.JobInfo) {
	gta.hyperNodeAffinityCache.syncJob(job, ssn.HyperNodes, ssn.HyperNodeTierNameMap)
}

func (gta *groupTopologyAffinityPlugin) OnSessionClose(ssn *framework.Session) {}

func (gta *groupTopologyAffinityPlugin) hyperNodeGradientForJob(
	ssn *framework.Session,
	job *api.JobInfo,
	root *api.HyperNodeInfo,
) [][]*api.HyperNodeInfo {
	terms := job.RequiredPodGroupAntiAffinityTerms()
	if len(terms) == 0 {
		return nil
	}

	maxTier := maxHyperNodeTier(ssn.HyperNodesSetByTier)
	result, err := gta.buildPodGroupAntiAffinityGradient(ssn, job, root, terms, maxTier, job.AllocatedHyperNode)
	if err != nil {
		klog.ErrorS(err, "build podGroup anti-affinity gradient failed", "job", job.UID)
		return [][]*api.HyperNodeInfo{}
	}
	return result
}

// buildPodGroupAntiAffinityGradient builds topology-only HyperNode gradients for hard
// podGroupAntiAffinity: BFS from the search root, drop candidates whose ancestor HyperNode
// at any required term tier overlaps a matching PodGroup's allocation.
func (gta *groupTopologyAffinityPlugin) buildPodGroupAntiAffinityGradient(
	ssn *framework.Session,
	job *api.JobInfo,
	root *api.HyperNodeInfo,
	terms []scheduling.PodGroupAffinityTerm,
	highestAllowedTier int,
	allocatedHyperNode string,
) ([][]*api.HyperNodeInfo, error) {
	matchingHyperNodesByTerm, err := collectMatchingHyperNodesByTerm(ssn, job, terms)
	if err != nil {
		return nil, err
	}

	searchRoot, err := getSearchRootForGradient(
		ssn.HyperNodes, root, highestAllowedTier, allocatedHyperNode,
	)
	if err != nil {
		return nil, err
	}

	eligibleHyperNodes := gta.bfsAntiAffinityEligibleHyperNodes(
		ssn, searchRoot, terms, matchingHyperNodesByTerm, highestAllowedTier, allocatedHyperNode,
	)
	return groupHyperNodesByTierAsc(eligibleHyperNodes), nil
}

// collectMatchingHyperNodesByTerm resolves, for each required term, the ancestor HyperNodes
// at the term tier where matching PodGroups are already allocated.
func collectMatchingHyperNodesByTerm(
	ssn *framework.Session,
	job *api.JobInfo,
	terms []scheduling.PodGroupAffinityTerm,
) ([]sets.Set[string], error) {
	matchingHyperNodesByTerm := make([]sets.Set[string], len(terms))
	for index, term := range terms {
		matchingHyperNodes, err := api.MatchingPodGroupsAllocatedHyperNodesForTerm(
			ssn.Jobs, ssn.HyperNodes, ssn.HyperNodeTierNameMap, job, term,
		)
		if err != nil {
			return nil, fmt.Errorf("term %d: %w", index, err)
		}
		matchingHyperNodesByTerm[index] = matchingHyperNodes
	}
	return matchingHyperNodesByTerm, nil
}

// bfsAntiAffinityEligibleHyperNodes walks the HyperNode tree from searchRoot and collects
// candidates that satisfy all required anti-affinity terms.
//
// TODO(performance): skip enqueueing descendants when a node is rejected because its tier-T
// ancestor HyperNode conflicts with a matching PodGroup allocation (descendants share the same
// tier-T ancestor). Do not prune when rejection is due to tier > highestAllowedTier or when
// GetAncestorHyperNode returns empty (finer descendants may still qualify). Optionally memoize
// GetAncestorHyperNode(hn, tier) per BFS pass when there are many terms.
func (gta *groupTopologyAffinityPlugin) bfsAntiAffinityEligibleHyperNodes(
	ssn *framework.Session,
	searchRoot *api.HyperNodeInfo,
	terms []scheduling.PodGroupAffinityTerm,
	matchingHyperNodesByTerm []sets.Set[string],
	highestAllowedTier int,
	allocatedHyperNode string,
) map[int][]*api.HyperNodeInfo {
	enqueued := set.New[string]()
	processQueue := []*api.HyperNodeInfo{searchRoot}
	enqueued.Insert(searchRoot.Name)

	eligibleHyperNodes := make(map[int][]*api.HyperNodeInfo)
	for len(processQueue) > 0 {
		current := processQueue[0]
		processQueue = processQueue[1:]

		if gta.isEligibleForPodGroupAntiAffinity(
			ssn, current, terms, matchingHyperNodesByTerm, highestAllowedTier, allocatedHyperNode,
		) {
			eligibleHyperNodes[current.Tier()] = append(eligibleHyperNodes[current.Tier()], current)
		}

		for child := range current.Children {
			if enqueued.Has(child) {
				continue
			}
			processQueue = append(processQueue, ssn.HyperNodes[child])
			enqueued.Insert(child)
		}
	}
	return eligibleHyperNodes
}

// groupHyperNodesByTierAsc groups HyperNodes by tier and returns tiers in ascending order.
func groupHyperNodesByTierAsc(eligibleHyperNodes map[int][]*api.HyperNodeInfo) [][]*api.HyperNodeInfo {
	var tiers []int
	for tier := range eligibleHyperNodes {
		tiers = append(tiers, tier)
	}
	sort.Ints(tiers)

	result := make([][]*api.HyperNodeInfo, 0, len(tiers))
	for _, tier := range tiers {
		result = append(result, eligibleHyperNodes[tier])
	}
	return result
}

// isEligibleForPodGroupAntiAffinity checks whether hn is a valid first-time placement candidate.
// After the job has an AllocatedHyperNode, every HyperNode in the search subtree is eligible
// (follow-up tasks must stay within the chosen envelope).
func (gta *groupTopologyAffinityPlugin) isEligibleForPodGroupAntiAffinity(
	ssn *framework.Session,
	hn *api.HyperNodeInfo,
	terms []scheduling.PodGroupAffinityTerm,
	matchingHyperNodesByTerm []sets.Set[string],
	highestAllowedTier int,
	allocatedHyperNode string,
) bool {
	if allocatedHyperNode != "" {
		return true
	}
	if hn.Tier() > highestAllowedTier {
		return false
	}

	for index, term := range terms {
		tier, err := api.ResolvePodGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			return false
		}
		// Compare at the term tier: reject if this candidate shares an ancestor HyperNode
		// with any matching PodGroup that is already placed there.
		ancestorHyperNode := ssn.HyperNodes.GetAncestorHyperNode(hn.Name, tier)
		if ancestorHyperNode == "" {
			return false
		}
		if matchingHyperNodesByTerm[index].Has(ancestorHyperNode) {
			return false
		}
	}
	return true
}

func (gta *groupTopologyAffinityPlugin) hyperNodeOrderFn(
	ssn *framework.Session,
	job *api.JobInfo,
	hyperNodes map[string][]*api.NodeInfo,
) (map[string]float64, error) {
	terms := job.PreferredPodGroupAntiAffinityTerms()
	if len(terms) == 0 {
		return nil, nil
	}

	scores := make(map[string]float64, len(hyperNodes))
	for hyperNode := range hyperNodes {
		scores[hyperNode] = FullScore
	}

	for _, term := range terms {
		matchingHyperNodes, err := api.MatchingPodGroupsAllocatedHyperNodesForTerm(
			ssn.Jobs, ssn.HyperNodes, ssn.HyperNodeTierNameMap, job, term,
		)
		if err != nil {
			return nil, err
		}
		tier, err := api.ResolvePodGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			return nil, err
		}

		weightFactor := float64(term.Weight) / 100.0
		if weightFactor <= 0 {
			weightFactor = 1.0
		}
		for hyperNode := range hyperNodes {
			ancestorHyperNode := ssn.HyperNodes.GetAncestorHyperNode(hyperNode, tier)
			if ancestorHyperNode != "" && matchingHyperNodes.Has(ancestorHyperNode) {
				scores[hyperNode] -= weightFactor * FullScore
				if scores[hyperNode] < ZeroScore {
					scores[hyperNode] = ZeroScore
				}
			}
		}
	}

	for hyperNode, score := range scores {
		scores[hyperNode] = float64(gta.weight) * score * float64(k8sFramework.MaxNodeScore)
	}
	return scores, nil
}

func maxHyperNodeTier(hyperNodesSetByTier map[int]sets.Set[string]) int {
	maxTier := 0
	for tier := range hyperNodesSetByTier {
		if tier > maxTier {
			maxTier = tier
		}
	}
	return maxTier
}

func getSearchRootForGradient(
	hyperNodes api.HyperNodeInfoMap,
	hyperNodeAvailable *api.HyperNodeInfo,
	highestAllowedTier int,
	allocatedHyperNode string,
) (*api.HyperNodeInfo, error) {
	if allocatedHyperNode == "" {
		return hyperNodeAvailable, nil
	}

	hyperNodeHighestAllowed, err := getHighestAllowedHyperNode(hyperNodes, highestAllowedTier, allocatedHyperNode)
	if err != nil {
		return nil, fmt.Errorf("get highest allowed hyperNode failed: %w", err)
	}

	lca := hyperNodes.GetLCAHyperNode(hyperNodeAvailable.Name, hyperNodeHighestAllowed)
	if lca == hyperNodeHighestAllowed {
		return hyperNodeAvailable, nil
	}
	if lca == hyperNodeAvailable.Name {
		hni, ok := hyperNodes[hyperNodeHighestAllowed]
		if !ok {
			return nil, fmt.Errorf("failed to get highest allowed HyperNode info for %s", hyperNodeHighestAllowed)
		}
		return hni, nil
	}

	return nil, fmt.Errorf("there is no intersection between hyperNodeAvailable %s and hyperNodeHighestAllowed %s",
		hyperNodeAvailable.Name, hyperNodeHighestAllowed)
}

func getHighestAllowedHyperNode(hyperNodes api.HyperNodeInfoMap, highestAllowedTier int, allocatedHyperNode string) (string, error) {
	var highestAllowedHyperNode string

	for _, ancestor := range hyperNodes.GetAncestors(allocatedHyperNode) {
		hni, ok := hyperNodes[ancestor]
		if !ok {
			return "", fmt.Errorf("allocated hyperNode %s ancestor %s not found", allocatedHyperNode, ancestor)
		}
		if hni.Tier() > highestAllowedTier {
			break
		}
		highestAllowedHyperNode = ancestor
	}

	if highestAllowedHyperNode == "" {
		return "", fmt.Errorf("allocated hyperNode %s tier is greater than highest allowed tier %d", allocatedHyperNode, highestAllowedTier)
	}

	return highestAllowedHyperNode, nil
}

// affinityHyperNodeKey identifies an ancestor HyperNode at a tier used for affinity comparison.
type affinityHyperNodeKey struct {
	tier      int
	hyperNode string
}

// hyperNodeAffinityCache records which jobs are allocated under each ancestor HyperNode scope.
type hyperNodeAffinityCache struct {
	jobsByHyperNode map[affinityHyperNodeKey]sets.Set[api.JobID]
}

func newHyperNodeAffinityCache() *hyperNodeAffinityCache {
	return &hyperNodeAffinityCache{
		jobsByHyperNode: make(map[affinityHyperNodeKey]sets.Set[api.JobID]),
	}
}

func (c *hyperNodeAffinityCache) record(jobID api.JobID, tier int, ancestorHyperNode string) {
	if ancestorHyperNode == "" {
		return
	}
	key := affinityHyperNodeKey{tier: tier, hyperNode: ancestorHyperNode}
	if c.jobsByHyperNode[key] == nil {
		c.jobsByHyperNode[key] = sets.New[api.JobID]()
	}
	c.jobsByHyperNode[key].Insert(jobID)
}

func (c *hyperNodeAffinityCache) removeJob(jobID api.JobID) {
	for key, jobs := range c.jobsByHyperNode {
		jobs.Delete(jobID)
		if jobs.Len() == 0 {
			delete(c.jobsByHyperNode, key)
		}
	}
}

func (c *hyperNodeAffinityCache) hasOtherJob(tier int, ancestorHyperNode string, excludeJob api.JobID) bool {
	if ancestorHyperNode == "" {
		return false
	}
	key := affinityHyperNodeKey{tier: tier, hyperNode: ancestorHyperNode}
	for jobID := range c.jobsByHyperNode[key] {
		if jobID != excludeJob {
			return true
		}
	}
	return false
}

func buildHyperNodeAffinityCache(
	jobs map[api.JobID]*api.JobInfo,
	hyperNodes api.HyperNodeInfoMap,
	tierNameMap api.HyperNodeTierNameMap,
) *hyperNodeAffinityCache {
	cache := newHyperNodeAffinityCache()
	for _, job := range jobs {
		cache.recordJob(job, hyperNodes, tierNameMap)
	}
	return cache
}

func (c *hyperNodeAffinityCache) recordJob(job *api.JobInfo, hyperNodes api.HyperNodeInfoMap, tierNameMap api.HyperNodeTierNameMap) {
	if job == nil || job.AllocatedHyperNode == "" || job.PodGroup == nil || job.PodGroup.Spec.TopologyAffinity == nil {
		return
	}
	anti := job.PodGroup.Spec.TopologyAffinity.PodGroupAntiAffinity
	if anti == nil {
		return
	}
	for _, term := range anti.Required {
		tier, err := api.ResolvePodGroupTermTier(term, tierNameMap)
		if err != nil {
			continue
		}
		ancestorHyperNode := hyperNodes.GetAncestorHyperNode(job.AllocatedHyperNode, tier)
		c.record(job.UID, tier, ancestorHyperNode)
	}
}

func (c *hyperNodeAffinityCache) syncJob(job *api.JobInfo, hyperNodes api.HyperNodeInfoMap, tierNameMap api.HyperNodeTierNameMap) {
	if c == nil || job == nil {
		return
	}
	c.removeJob(job.UID)
	if job.AllocatedHyperNode == "" {
		return
	}
	if job.AllocatedTaskNum() == 0 && job.WaitingTaskNum() == 0 {
		return
	}
	c.recordJob(job, hyperNodes, tierNameMap)
}
