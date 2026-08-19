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

// Package budget enforces the per-run blast-radius limits against the complete
// prospective plan (committed moves plus the candidate under consideration).
package budget

import (
	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const Name = "budget"

func init() {
	framework.RegisterPlugin(Name, func() framework.Plugin { return &budgetPlugin{} })
}

type budgetPlugin struct{}

func (*budgetPlugin) Name() string { return Name }

func (*budgetPlugin) OnSessionOpen(ssn *framework.Session) {
	ssn.AddCandidateFilterFn("maxPerRun", func(ctx *api.PlanContext, candidate *framework.PlanningCandidate) *framework.CandidateFilterResult {
		if candidate == nil || candidate.Plan == nil {
			return nil
		}
		aggregate := candidate.Plan.MoveAggregate(ctx)
		if (ssn.LimitPodGroups() || ssn.MaxPodGroups() > 0) && int(aggregate.AffectedPodGroups) > ssn.MaxPodGroups() {
			return &framework.CandidateFilterResult{Reason: "max_pod_groups", Message: "candidate exceeds maxPerRun.podGroups"}
		}
		if (ssn.LimitResource() || ssn.MaxResource() > 0) && aggregate.MovedResource > ssn.MaxResource() {
			return &framework.CandidateFilterResult{Reason: "max_resource", Message: "candidate exceeds maxPerRun.resources"}
		}
		return nil
	})
}

func (*budgetPlugin) OnSessionClose(*framework.Session) {}
