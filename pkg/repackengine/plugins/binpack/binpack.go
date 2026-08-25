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

// Package binpack owns packing order and receiver preference policy. It places
// larger victims first, first fills nodes that cannot be drained later, and
// uses best-fit as the final deterministic preference. The planner's base
// receiver eligibility—not this optional plugin—excludes empty and full nodes.
package binpack

import (
	"cmp"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const Name = "binpack"

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory: func(framework.Arguments) framework.Plugin { return &binpackPlugin{} },
	})
}

type binpackPlugin struct{}

func (*binpackPlugin) Name() string { return Name }

func (*binpackPlugin) OnSessionOpen(ssn *framework.Session) {
	resourceName := ssn.Resource()
	// First-fit decreasing is the item-ordering half of the packing strategy:
	// place larger target-resource requests first so infeasible layouts fail fast.
	ssn.AddVictimOrderFn("largestTargetResourceFirst", func(left, right *schedapi.TaskInfo) int {
		return cmp.Compare(api.Scalar(right.InitResreq, resourceName), api.Scalar(left.InitResreq, resourceName))
	})
	ssn.AddReceiverPreferenceFn("staysOccupied", framework.ReceiverPreferencePhaseStability,
		func(_ *api.PlanContext, _ *framework.PlanningCandidate, receiver *framework.ReceiverCandidate) framework.ReceiverPreference {
			if receiver.StaysOccupied {
				return framework.ReceiverPreference{1}
			}
			return framework.ReceiverPreference{}
		})
	ssn.AddReceiverPreferenceFn("bestFit", framework.ReceiverPreferencePhasePacking,
		func(_ *api.PlanContext, _ *framework.PlanningCandidate, receiver *framework.ReceiverCandidate) framework.ReceiverPreference {
			return framework.ReceiverPreference{-receiver.AvailableResource}
		})
}

func (*binpackPlugin) OnSessionClose(*framework.Session) {}
