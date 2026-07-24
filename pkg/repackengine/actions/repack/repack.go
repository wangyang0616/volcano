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

// Package repack is the planning action: run the selected core to produce a plan
// and render the report. Execute side effects deliberately live in the engine
// driver, after the plan and relocation journal have been durably persisted.
// Future planning actions (relief, simulate) compose after it in the pipeline.
package repack

import (
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/repackengine/framework"
)

func init() {
	framework.RegisterAction(framework.ActionRepack, func() framework.Action { return &repackAction{} })
}

type repackAction struct{}

func (*repackAction) Name() string { return framework.ActionRepack }

func (*repackAction) Execute(ssn *framework.Session) {
	name := ssn.CoreName()
	if name == "" {
		name = framework.CoreDrain
	}
	core, ok := framework.GetCore(name)
	if !ok {
		klog.ErrorS(nil, "repack: unknown core in config", "core", name, "registered", framework.CoreNames())
		return
	}

	plan, found := core.Plan(ssn)
	ssn.SetPlan(plan)
	report := framework.RenderReport(plan)
	if plan == nil {
		// No plan: still record the cluster's current fragmentation so the driver
		// can tell NoFragmentation (clean) apart from BelowGoalThreshold (fragmented
		// but no worthwhile plan). RenderReport(nil) leaves FragmentationRateBefore at 0.
		currentFragmentationRate := ssn.CurrentFragmentationRate()
		report.FragmentationRateBefore, report.FragmentationRateAfter = currentFragmentationRate, currentFragmentationRate
	}
	ssn.SetReport(report)
	if !found || plan == nil {
		return // NoRepackNeeded
	}
}
