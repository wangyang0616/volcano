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

// Package repack is the P0 action: run the selected core to produce a plan, render
// the report, and — in Execute mode — commit it (evict + steer via nomination).
// Future actions (relief, simulate) compose after it in the pipeline.
package repack

import (
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

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
		// but no worthwhile plan). RenderReport(nil) leaves FragRateBefore at 0.
		f := ssn.CurrentFragRate()
		report.FragRateBefore, report.FragRateAfter = f, f
	}
	ssn.SetReport(report)
	if !found || plan == nil {
		return // NoRepackNeeded
	}

	if ssn.Mode() == repackv1alpha1.RepackModeExecute {
		res, err := framework.CommitPlan(plan, ssn.Hooks())
		if err != nil {
			klog.ErrorS(err, "repack: commit failed")
		}
		// Keep the result (evicted vs failed per-pod) so the driver can distinguish
		// a real Execute from one where every eviction was rejected (e.g. by PDBs).
		ssn.SetCommit(&res)
	}
}
