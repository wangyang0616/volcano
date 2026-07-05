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

// Package base registers the gang-agnostic disruption scores every repack run
// wants: number of affected PodGroups, moved cards, moved pods. Weights are the
// P0 defaults (disruptionPolicy overrides them in P1). Gang-semantics scores live
// in the gang plugin.
package base

import (
	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the config name for this plugin.
const Name = "base"

func init() {
	framework.RegisterPlugin(Name, func() framework.Plugin { return &basePlugin{} })
}

type basePlugin struct{}

func (*basePlugin) Name() string { return Name }

func (*basePlugin) OnSessionOpen(ssn *framework.Session) {
	ssn.AddDisruptionScoreFn("affectedPodGroups", 1.0, func(ctx *api.PlanContext, p *api.CandidatePlan) float64 {
		return float64(p.Aggregate(ctx).AffectedPGs)
	})
	ssn.AddDisruptionScoreFn("movedGPU", 0.3, func(ctx *api.PlanContext, p *api.CandidatePlan) float64 {
		return float64(p.Aggregate(ctx).MovedGPU)
	})
	ssn.AddDisruptionScoreFn("movedPods", 0.1, func(ctx *api.PlanContext, p *api.CandidatePlan) float64 {
		return float64(p.Aggregate(ctx).MovedPods)
	})
}

func (*basePlugin) OnSessionClose(*framework.Session) {}
