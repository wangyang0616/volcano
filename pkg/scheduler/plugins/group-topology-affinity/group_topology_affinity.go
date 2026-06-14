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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	k8sFramework "k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/utils/set"

	scheduling "volcano.sh/apis/pkg/apis/scheduling"
	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/framework"
	"volcano.sh/volcano/pkg/scheduler/util/perflog"
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
}

func (gta *groupTopologyAffinityPlugin) OnSessionClose(ssn *framework.Session) {}

func (gta *groupTopologyAffinityPlugin) hyperNodeGradientForJob(
	ssn *framework.Session,
	job *api.JobInfo,
	root *api.HyperNodeInfo,
) [][]*api.HyperNodeInfo {
	start := time.Now()
	terms := job.RequiredPodGroupAntiAffinityTerms()
	if len(terms) == 0 {
		return nil
	}

	maxTier := maxHyperNodeTier(ssn.HyperNodesSetByTier)
	logPodGroupAntiAffinityStart(job, root)
	result, err := gta.buildPodGroupAntiAffinityGradient(ssn, job, root, terms, maxTier, job.AllocatedHyperNode)
	if err != nil {
		klog.ErrorS(err, "build podGroup anti-affinity gradient failed", "job", job.UID)
		return [][]*api.HyperNodeInfo{}
	}
	perflog.LogPodGroupAntiAffinityGradientForJob(
		podGroupAntiAffinityJobContext(job), len(terms), len(result), time.Since(start),
	)
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
	start := time.Now()
	collectStart := time.Now()
	matchingHyperNodesByTerm, collectStats, err := collectMatchingHyperNodesByTerm(ssn, job, terms)
	if err != nil {
		return nil, err
	}
	collectLatency := time.Since(collectStart)

	searchRoot, err := getSearchRootForGradient(
		ssn.HyperNodes, root, highestAllowedTier, allocatedHyperNode,
	)
	if err != nil {
		return nil, err
	}

	logPodGroupAntiAffinityMatchingOccupancy(ssn, job, terms, matchingHyperNodesByTerm)

	bfsStart := time.Now()
	eligibleHyperNodes, bfsStats := gta.bfsAntiAffinityEligibleHyperNodes(
		ssn, job, searchRoot, terms, matchingHyperNodesByTerm, highestAllowedTier,
	)
	bfsLatency := time.Since(bfsStart)
	logPodGroupAntiAffinityGradientResult(job, searchRoot, eligibleHyperNodes)
	perflog.LogPodGroupAntiAffinityBuildGradient(
		podGroupAntiAffinityJobContext(job),
		collectStats, collectLatency, bfsStats, bfsLatency, time.Since(start),
	)
	return groupHyperNodesByTierAsc(eligibleHyperNodes), nil
}

// collectMatchingHyperNodesByTerm resolves, for each required term, the ancestor HyperNodes
// at the term tier where matching PodGroups are already allocated.
func collectMatchingHyperNodesByTerm(
	ssn *framework.Session,
	job *api.JobInfo,
	terms []scheduling.PodGroupAffinityTerm,
) ([]sets.Set[string], perflog.CollectMatchingStats, error) {
	matchingHyperNodesByTerm := make([]sets.Set[string], len(terms))
	stats := perflog.CollectMatchingStats{}
	for index, term := range terms {
		matchingHyperNodes, err := api.MatchingPodGroupsAllocatedHyperNodesForTerm(
			ssn.Jobs, ssn.HyperNodes, ssn.HyperNodeTierNameMap, job, term, ssn.RealNodesSet,
		)
		if err != nil {
			return nil, stats, fmt.Errorf("term %d: %w", index, err)
		}
		matchingHyperNodesByTerm[index] = matchingHyperNodes
		stats.TotalOccupiedHyperNodes += matchingHyperNodes.Len()
	}
	return matchingHyperNodesByTerm, stats, nil
}

func (gta *groupTopologyAffinityPlugin) bfsAntiAffinityEligibleHyperNodes(
	ssn *framework.Session,
	job *api.JobInfo,
	searchRoot *api.HyperNodeInfo,
	terms []scheduling.PodGroupAffinityTerm,
	matchingHyperNodesByTerm []sets.Set[string],
	highestAllowedTier int,
) (map[int][]*api.HyperNodeInfo, perflog.BFSAntiAffinityStats) {
	enqueued := set.New[string]()
	processQueue := []*api.HyperNodeInfo{searchRoot}
	enqueued.Insert(searchRoot.Name)

	eligibleByTier := make(map[int][]*api.HyperNodeInfo)
	eligibleCount := 0
	for len(processQueue) > 0 {
		current := processQueue[0]
		processQueue = processQueue[1:]

		if gta.isEligibleForPodGroupAntiAffinity(
			ssn, job, current, terms, matchingHyperNodesByTerm, highestAllowedTier,
		) {
			eligibleByTier[current.Tier()] = append(eligibleByTier[current.Tier()], current)
			eligibleCount++
		}

		for child := range current.Children {
			if enqueued.Has(child) {
				continue
			}
			processQueue = append(processQueue, ssn.HyperNodes[child])
			enqueued.Insert(child)
		}
	}
	return eligibleByTier, perflog.BFSAntiAffinityStats{
		VisitedHyperNodes:  enqueued.Len(),
		EligibleHyperNodes: eligibleCount,
	}
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

// isEligibleForPodGroupAntiAffinity checks whether hn may host the job without violating
// hard podGroupAntiAffinity at each required term tier.
func (gta *groupTopologyAffinityPlugin) isEligibleForPodGroupAntiAffinity(
	ssn *framework.Session,
	job *api.JobInfo,
	hn *api.HyperNodeInfo,
	terms []scheduling.PodGroupAffinityTerm,
	matchingHyperNodesByTerm []sets.Set[string],
	highestAllowedTier int,
) bool {
	if hn.Tier() > highestAllowedTier {
		logPodGroupAntiAffinityRejectHyperNode(job, hn, "tierAboveHighestAllowed", -1, hn.Tier(), "")
		return false
	}

	for index, term := range terms {
		tier, err := api.ResolvePodGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			logPodGroupAntiAffinityRejectHyperNode(job, hn, "resolveTermTierFailed", index, 0, "")
			return false
		}
		// Compare at the term tier: reject if this candidate shares an ancestor HyperNode
		// with any matching PodGroup that is already placed there.
		ancestorHyperNode := ssn.HyperNodes.GetAncestorHyperNode(hn.Name, tier)
		if ancestorHyperNode == "" {
			logPodGroupAntiAffinityRejectHyperNode(job, hn, "emptyAncestorHyperNode", index, tier, "")
			return false
		}
		if matchingHyperNodesByTerm[index].Has(ancestorHyperNode) {
			logPodGroupAntiAffinityRejectHyperNode(job, hn, "conflictWithMatchingPodGroup", index, tier, ancestorHyperNode)
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
	start := time.Now()
	terms := job.PreferredPodGroupAntiAffinityTerms()
	if len(terms) == 0 {
		return nil, nil
	}

	hyperNodeCandidates := make([]string, 0, len(hyperNodes))
	for hyperNode := range hyperNodes {
		hyperNodeCandidates = append(hyperNodeCandidates, hyperNode)
	}
	logPodGroupAntiAffinityPreferredStart(job, hyperNodeCandidates)

	scores := make(map[string]float64, len(hyperNodes))
	for hyperNode := range hyperNodes {
		scores[hyperNode] = FullScore
	}

	collectStart := time.Now()
	matchingHyperNodesByTerm := make([]sets.Set[string], len(terms))
	totalOccupied := 0
	for termIndex, term := range terms {
		matchingHyperNodes, err := api.MatchingPodGroupsAllocatedHyperNodesForTerm(
			ssn.Jobs, ssn.HyperNodes, ssn.HyperNodeTierNameMap, job, term, ssn.RealNodesSet,
		)
		if err != nil {
			return nil, err
		}
		matchingHyperNodesByTerm[termIndex] = matchingHyperNodes
		totalOccupied += matchingHyperNodes.Len()
	}
	collectLatency := time.Since(collectStart)
	logPodGroupAntiAffinityMatchingOccupancy(ssn, job, terms, matchingHyperNodesByTerm)

	for termIndex, term := range terms {
		matchingHyperNodes := matchingHyperNodesByTerm[termIndex]
		tier, err := api.ResolvePodGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			return nil, err
		}

		if term.Weight < 1 || term.Weight > 100 {
			continue
		}
		weightFactor := float64(term.Weight) / 100.0
		for hyperNode := range hyperNodes {
			ancestorHyperNode := ssn.HyperNodes.GetAncestorHyperNode(hyperNode, tier)
			if ancestorHyperNode != "" && matchingHyperNodes.Has(ancestorHyperNode) {
				scoreBefore := scores[hyperNode]
				logPodGroupAntiAffinityPreferredPenalty(job, hyperNode, termIndex, tier, ancestorHyperNode)
				scores[hyperNode] -= weightFactor * FullScore
				if scores[hyperNode] < ZeroScore {
					scores[hyperNode] = ZeroScore
				}
				logPodGroupAntiAffinityPreferredScoreDetail(
					job, hyperNode, termIndex, term.Weight, weightFactor, scoreBefore, scores[hyperNode],
				)
			}
		}
	}

	for hyperNode, score := range scores {
		scores[hyperNode] = float64(gta.weight) * score * float64(k8sFramework.MaxNodeScore)
	}
	logPodGroupAntiAffinityPreferredFinalScores(job, scores, gta.weight)
	perflog.LogPodGroupAntiAffinityPreferredOrder(
		podGroupAntiAffinityJobContext(job),
		len(terms), len(hyperNodes), totalOccupied, collectLatency, time.Since(start),
	)
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

func podGroupAntiAffinityJobContext(job *api.JobInfo) perflog.PodGroupAntiAffinityJobContext {
	return perflog.PodGroupAntiAffinityJobContext{
		Namespace: job.Namespace,
		Name:      job.Name,
		Pods:      pendingPodNames(job),
	}
}

func pendingPodNames(job *api.JobInfo) string {
	if job == nil {
		return ""
	}
	names := make([]string, 0, len(job.TaskStatusIndex[api.Pending]))
	for _, task := range job.TaskStatusIndex[api.Pending] {
		names = append(names, task.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func describeMatchingJobPlacement(
	job *api.JobInfo,
	hyperNodes api.HyperNodeInfoMap,
	nodesByHyperNode map[string]sets.Set[string],
) string {
	if job == nil {
		return ""
	}
	allocatedHyperNode := job.AllocatedHyperNode
	if allocatedHyperNode == "" {
		for _, task := range allocatedTasks(job) {
			if task.NodeName == "" {
				continue
			}
			for hyperNode, nodes := range nodesByHyperNode {
				if nodes.Has(task.NodeName) {
					if allocatedHyperNode == "" {
						allocatedHyperNode = hyperNode
					} else {
						allocatedHyperNode = hyperNodes.GetLCAHyperNode(allocatedHyperNode, hyperNode)
					}
				}
			}
		}
	}
	pgName := job.Name
	if job.PodGroup != nil && job.PodGroup.Name != "" {
		pgName = job.PodGroup.Name
	}
	nodeNames := allocatedTaskNodeNames(job)
	if allocatedHyperNode == "" && nodeNames == "[]" {
		return ""
	}
	return fmt.Sprintf("%s/%s(hyperNode=%s,nodes=%s)", job.Namespace, pgName, allocatedHyperNode, nodeNames)
}

func allocatedTasks(job *api.JobInfo) []*api.TaskInfo {
	tasks := make([]*api.TaskInfo, 0)
	for status, taskMap := range job.TaskStatusIndex {
		if !api.AllocatedStatus(status) {
			continue
		}
		for _, task := range taskMap {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func allocatedTaskNodeNames(job *api.JobInfo) string {
	nodeSet := sets.New[string]()
	for _, task := range allocatedTasks(job) {
		if task.NodeName != "" {
			nodeSet.Insert(task.NodeName)
		}
	}
	nodes := nodeSet.UnsortedList()
	sort.Strings(nodes)
	if len(nodes) == 0 {
		return "[]"
	}
	return fmt.Sprintf("[%s]", strings.Join(nodes, ","))
}

func logPodGroupAntiAffinityStart(job *api.JobInfo, root *api.HyperNodeInfo) {
	klog.V(3).InfoS("podGroup anti-affinity: evaluate gradient",
		"job", klog.KRef(job.Namespace, job.Name),
		"pods", pendingPodNames(job),
		"rootHyperNode", root.Name,
		"allocatedHyperNode", job.AllocatedHyperNode,
	)
}

func logPodGroupAntiAffinityMatchingOccupancy(
	ssn *framework.Session,
	job *api.JobInfo,
	terms []scheduling.PodGroupAffinityTerm,
	matchingHyperNodesByTerm []sets.Set[string],
) {
	for index, term := range terms {
		tier, err := api.ResolvePodGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			klog.V(3).InfoS("podGroup anti-affinity: resolve term tier failed",
				"job", klog.KRef(job.Namespace, job.Name),
				"pods", pendingPodNames(job),
				"termIndex", index,
				"err", err,
			)
			continue
		}
		occupiedHyperNodes := matchingHyperNodesByTerm[index].UnsortedList()
		sort.Strings(occupiedHyperNodes)
		klog.V(3).InfoS("podGroup anti-affinity: matching occupancy",
			"job", klog.KRef(job.Namespace, job.Name),
			"pods", pendingPodNames(job),
			"termIndex", index,
			"tier", tier,
			"occupiedHyperNodes", occupiedHyperNodes,
			"matchingJobs", matchingJobPlacementsForTerm(ssn, job, term),
		)
	}
}

func matchingJobPlacementsForTerm(
	ssn *framework.Session,
	selfJob *api.JobInfo,
	term scheduling.PodGroupAffinityTerm,
) []string {
	placements := make([]string, 0)
	for _, matchingJob := range ssn.Jobs {
		if !api.PodGroupMatchesTerm(term, selfJob, matchingJob) {
			continue
		}
		placement := describeMatchingJobPlacement(matchingJob, ssn.HyperNodes, ssn.RealNodesSet)
		if placement == "" {
			continue
		}
		placements = append(placements, placement)
	}
	sort.Strings(placements)
	return placements
}

func logPodGroupAntiAffinityRejectHyperNode(
	job *api.JobInfo,
	candidate *api.HyperNodeInfo,
	reason string,
	termIndex int,
	tier int,
	conflictHyperNode string,
) {
	klog.V(3).InfoS("podGroup anti-affinity: reject hyperNode",
		"job", klog.KRef(job.Namespace, job.Name),
		"pods", pendingPodNames(job),
		"hyperNode", candidate.Name,
		"reason", reason,
		"termIndex", termIndex,
		"tier", tier,
		"conflictHyperNode", conflictHyperNode,
	)
}

func logPodGroupAntiAffinityGradientResult(
	job *api.JobInfo,
	searchRoot *api.HyperNodeInfo,
	eligibleHyperNodes map[int][]*api.HyperNodeInfo,
) {
	klog.V(3).InfoS("podGroup anti-affinity: gradient result",
		"job", klog.KRef(job.Namespace, job.Name),
		"pods", pendingPodNames(job),
		"searchRoot", searchRoot.Name,
		"eligibleHyperNodes", hyperNodeNamesByTier(eligibleHyperNodes),
	)
}

func logPodGroupAntiAffinityPreferredStart(job *api.JobInfo, hyperNodeCandidates []string) {
	sort.Strings(hyperNodeCandidates)
	klog.V(3).InfoS("podGroup anti-affinity: evaluate preferred",
		"job", klog.KRef(job.Namespace, job.Name),
		"pods", pendingPodNames(job),
		"hyperNodeCandidates", hyperNodeCandidates,
	)
}

func logPodGroupAntiAffinityPreferredPenalty(
	job *api.JobInfo,
	hyperNode string,
	termIndex int,
	tier int,
	conflictHyperNode string,
) {
	klog.V(3).InfoS("podGroup anti-affinity: preferred penalty",
		"job", klog.KRef(job.Namespace, job.Name),
		"pods", pendingPodNames(job),
		"hyperNode", hyperNode,
		"termIndex", termIndex,
		"tier", tier,
		"conflictHyperNode", conflictHyperNode,
	)
}

func logPodGroupAntiAffinityPreferredScoreDetail(
	job *api.JobInfo,
	hyperNode string,
	termIndex int,
	weight int32,
	weightFactor float64,
	scoreBefore float64,
	scoreAfter float64,
) {
	klog.V(4).InfoS("podGroup anti-affinity: preferred score detail",
		"job", klog.KRef(job.Namespace, job.Name),
		"pods", pendingPodNames(job),
		"hyperNode", hyperNode,
		"termIndex", termIndex,
		"weight", weight,
		"weightFactor", weightFactor,
		"scoreBefore", scoreBefore,
		"scoreAfter", scoreAfter,
		"penalty", scoreBefore-scoreAfter,
	)
}

func logPodGroupAntiAffinityPreferredFinalScores(job *api.JobInfo, scores map[string]float64, pluginWeight int) {
	if len(scores) == 0 {
		return
	}
	hyperNodes := make([]string, 0, len(scores))
	for hyperNode := range scores {
		hyperNodes = append(hyperNodes, hyperNode)
	}
	sort.Strings(hyperNodes)

	details := make([]string, 0, len(hyperNodes))
	for _, hyperNode := range hyperNodes {
		details = append(details, fmt.Sprintf("%s:%.2f", hyperNode, scores[hyperNode]))
	}
	klog.V(4).InfoS("podGroup anti-affinity: preferred final scores",
		"job", klog.KRef(job.Namespace, job.Name),
		"pods", pendingPodNames(job),
		"pluginWeight", pluginWeight,
		"scores", strings.Join(details, ","),
	)
}

func hyperNodeNamesByTier(hyperNodesByTier map[int][]*api.HyperNodeInfo) string {
	if len(hyperNodesByTier) == 0 {
		return "{}"
	}
	tiers := make([]int, 0, len(hyperNodesByTier))
	for tier := range hyperNodesByTier {
		tiers = append(tiers, tier)
	}
	sort.Ints(tiers)

	parts := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		names := make([]string, 0, len(hyperNodesByTier[tier]))
		for _, hn := range hyperNodesByTier[tier] {
			names = append(names, hn.Name)
		}
		sort.Strings(names)
		parts = append(parts, fmt.Sprintf("tier-%d:[%s]", tier, strings.Join(names, ",")))
	}
	return "{" + strings.Join(parts, " ") + "}"
}
