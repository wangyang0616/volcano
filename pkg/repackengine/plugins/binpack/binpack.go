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

// Package binpack owns receiver-universe and packing preference policy. It
// avoids lighting up target-resource-empty nodes, first fills nodes that cannot
// be drained later, and uses best-fit as the final deterministic preference.
package binpack

import (
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const Name = "binpack"

const (
	staysOccupiedPriority = 10
	bestFitPriority       = 30
)

func init() {
	framework.RegisterPlugin(Name, func() framework.Plugin { return &binpackPlugin{} })
}

type binpackPlugin struct{}

func (*binpackPlugin) Name() string { return Name }

func (*binpackPlugin) OnSessionOpen(ssn *framework.Session) {
	resourceName := ssn.Resource()
	ssn.AddReceiverPoolFn(func(_ *api.PlanContext, nodes []*schedapi.NodeInfo) []*schedapi.NodeInfo {
		pool := make([]*schedapi.NodeInfo, 0, len(nodes))
		for _, node := range nodes {
			if node != nil && node.Used != nil && api.Scalar(node.Used, resourceName) > 0 {
				pool = append(pool, node)
			}
		}
		return pool
	})
	ssn.AddReceiverRankFn("staysOccupied", staysOccupiedPriority,
		func(_ *api.PlanContext, _ *framework.PlanningCandidate, receiver *framework.ReceiverCandidate) framework.ReceiverRank {
			if receiver.StaysOccupied {
				return framework.ReceiverRank{1}
			}
			return framework.ReceiverRank{}
		})
	ssn.AddReceiverRankFn("bestFit", bestFitPriority,
		func(_ *api.PlanContext, _ *framework.PlanningCandidate, receiver *framework.ReceiverCandidate) framework.ReceiverRank {
			return framework.ReceiverRank{-receiver.AvailableResource}
		})
}

func (*binpackPlugin) OnSessionClose(*framework.Session) {}
