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

// Package resource owns target-resource-specific planning policy: reject units
// whose victims cannot fit in the aggregate receiver slack and simulate larger
// accelerator victims first for deterministic fail-fast packing.
package resource

import (
	"cmp"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const Name = "resource"

func init() {
	framework.RegisterPlugin(Name, func() framework.Plugin { return &resourcePlugin{} })
}

type resourcePlugin struct{}

func (*resourcePlugin) Name() string { return Name }

func (*resourcePlugin) OnSessionOpen(ssn *framework.Session) {
	ssn.AddCandidateFilterFn("receiverResource", func(_ *api.PlanContext, candidate *framework.PlanningCandidate) *framework.CandidateFilterResult {
		if candidate == nil || candidate.AvailableReceiverResource >= candidate.RequiredResource {
			return nil
		}
		return &framework.CandidateFilterResult{
			Reason:         "insufficient_receiver_resource",
			Message:        "aggregate receiver target-resource capacity is insufficient",
			MarkInfeasible: true,
		}
	})
	resourceName := ssn.Resource()
	ssn.AddVictimOrderFn("largestTargetResourceFirst", func(left, right *schedapi.TaskInfo) int {
		return cmp.Compare(api.Scalar(right.InitResreq, resourceName), api.Scalar(left.InitResreq, resourceName))
	})
}

func (*resourcePlugin) OnSessionClose(*framework.Session) {}
