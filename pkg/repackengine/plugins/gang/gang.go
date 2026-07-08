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

// Package gang is the gang-awareness plugin: it registers the gang-semantics
// disruption scores so the core prefers plans that don't shatter PodGroups —
// gangBreaches (gangs pushed below minAvailable) and damagedGPU (a breached gang
// counts its WHOLE footprint as lost, capturing "突破 minAvailable 即整组受损").
package gang

import (
	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the config name for this plugin.
const Name = "gang"

// Default P0 weights for the gang-semantics disruption dimensions: pushing a gang
// below minAvailable (a breach) is weighted above merely churning a breached gang's
// cards (disruptionPolicy overrides these in P1).
const (
	weightGangBreaches = 0.8
	weightDamagedGPU   = 0.6
)

func init() {
	framework.RegisterPlugin(Name, func() framework.Plugin { return &gangPlugin{} })
}

type gangPlugin struct{}

func (*gangPlugin) Name() string { return Name }

func (*gangPlugin) OnSessionOpen(ssn *framework.Session) {
	ssn.AddDisruptionScoreFn("gangBreaches", weightGangBreaches, scoreGangBreaches)
	ssn.AddDisruptionScoreFn("damagedGPU", weightDamagedGPU, scoreDamagedGPU)
}

func (*gangPlugin) OnSessionClose(*framework.Session) {}

// scoreGangBreaches: number of gangs pushed below minAvailable (moved pods exceed
// the gang's slack = Running − MinAvailable), risking gang eviction mid-repack.
func scoreGangBreaches(ctx *api.PlanContext, p *api.CandidatePlan) float64 {
	var breachedGangs int64
	for podGroupID, moved := range p.Aggregate(ctx).ByPG {
		view := ctx.View(podGroupID)
		slack := int64(view.Running) - int64(view.MinAvailable)
		if slack < 0 {
			slack = 0
		}
		if moved.MovedPods > slack {
			breachedGangs++
		}
	}
	return float64(breachedGangs)
}

// scoreDamagedGPU: gang-semantics "damaged cards". Within slack → only the moved
// cards count; breaching minAvailable → the whole gang Footprint counts (and
// further pods of an already-breached gang add nothing).
func scoreDamagedGPU(ctx *api.PlanContext, p *api.CandidatePlan) float64 {
	var damagedCards int64
	for podGroupID, moved := range p.Aggregate(ctx).ByPG {
		view := ctx.View(podGroupID)
		slack := int64(view.Running) - int64(view.MinAvailable)
		if slack < 0 {
			slack = 0
		}
		if moved.MovedPods > slack {
			damagedCards += view.Footprint
		} else {
			damagedCards += moved.MovedGPU
		}
	}
	return float64(damagedCards)
}
