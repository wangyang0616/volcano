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

func init() {
	framework.RegisterPlugin(Name, func() framework.Plugin { return &gangPlugin{} })
}

type gangPlugin struct{}

func (*gangPlugin) Name() string { return Name }

func (*gangPlugin) OnSessionOpen(ssn *framework.Session) {
	ssn.AddDisruptionScoreFn("gangBreaches", 0.8, scoreGangBreaches)
	ssn.AddDisruptionScoreFn("damagedGPU", 0.6, scoreDamagedGPU)
}

func (*gangPlugin) OnSessionClose(*framework.Session) {}

// scoreGangBreaches: number of gangs pushed below minAvailable (moved pods exceed
// the gang's slack = Running − MinAvailable), risking gang eviction mid-repack.
func scoreGangBreaches(ctx *api.PlanContext, p *api.CandidatePlan) float64 {
	var n int64
	for pg, agg := range p.Aggregate(ctx).ByPG {
		v := ctx.View(pg)
		slack := int64(v.Running) - int64(v.MinAvailable)
		if slack < 0 {
			slack = 0
		}
		if agg.MovedPods > slack {
			n++
		}
	}
	return float64(n)
}

// scoreDamagedGPU: gang-semantics "damaged cards". Within slack → only the moved
// cards count; breaching minAvailable → the whole gang Footprint counts (and
// further pods of an already-breached gang add nothing).
func scoreDamagedGPU(ctx *api.PlanContext, p *api.CandidatePlan) float64 {
	var sum int64
	for pg, agg := range p.Aggregate(ctx).ByPG {
		v := ctx.View(pg)
		slack := int64(v.Running) - int64(v.MinAvailable)
		if slack < 0 {
			slack = 0
		}
		if agg.MovedPods > slack {
			sum += v.Footprint
		} else {
			sum += agg.MovedGPU
		}
	}
	return float64(sum)
}
