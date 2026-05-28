/*
Copyright 2025 The Volcano Authors.

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

package api

import (
	"sort"

	"k8s.io/apimachinery/pkg/util/sets"
)

// UnionGradientHyperNodeNames returns the set of HyperNode names appearing in any gradient layer.
func UnionGradientHyperNodeNames(gradients [][]*HyperNodeInfo) sets.Set[string] {
	names := sets.New[string]()
	for _, layer := range gradients {
		for _, hn := range layer {
			if hn != nil {
				names.Insert(hn.Name)
			}
		}
	}
	return names
}

// IntersectHyperNodeGradients returns HyperNode names eligible under all plugin gradients:
// for each plugin, union its layers, then intersect across plugins.
func IntersectHyperNodeGradients(perPlugin [][][]*HyperNodeInfo) sets.Set[string] {
	if len(perPlugin) == 0 {
		return sets.New[string]()
	}
	result := UnionGradientHyperNodeNames(perPlugin[0])
	for i := 1; i < len(perPlugin); i++ {
		result = result.Intersection(UnionGradientHyperNodeNames(perPlugin[i]))
	}
	return result
}

// RebuildGradientsByTier groups eligible HyperNodes into ascending tier layers.
func RebuildGradientsByTier(hyperNodes HyperNodeInfoMap, eligible sets.Set[string]) [][]*HyperNodeInfo {
	if eligible.Len() == 0 {
		return nil
	}

	byTier := make(map[int][]*HyperNodeInfo)
	for name := range eligible {
		hn := hyperNodes[name]
		if hn == nil {
			continue
		}
		byTier[hn.Tier()] = append(byTier[hn.Tier()], hn)
	}

	tiers := make([]int, 0, len(byTier))
	for tier := range byTier {
		tiers = append(tiers, tier)
	}
	sort.Ints(tiers)

	result := make([][]*HyperNodeInfo, 0, len(tiers))
	for _, tier := range tiers {
		layer := dedupeHyperNodesByName(byTier[tier])
		if len(layer) > 0 {
			result = append(result, layer)
		}
	}
	return result
}

func dedupeHyperNodesByName(hyperNodes []*HyperNodeInfo) []*HyperNodeInfo {
	seen := sets.New[string]()
	deduped := make([]*HyperNodeInfo, 0, len(hyperNodes))
	for _, hn := range hyperNodes {
		if hn == nil || seen.Has(hn.Name) {
			continue
		}
		seen.Insert(hn.Name)
		deduped = append(deduped, hn)
	}
	return deduped
}
