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

package repackengine

import (
	"fmt"

	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

const (
	eventReasonPlanComputed              = "PlanComputed"
	eventReasonExecutePrepared           = "ExecutePrepared"
	eventReasonEvictionsIssued           = "EvictionsIssued"
	eventReasonAwaitingPlacement         = "AwaitingPlacement"
	eventReasonPlacementSelected         = "PlacementSelected"
	eventReasonPlacementAwaitingCapacity = "PlacementAwaitingCapacity"
	eventReasonPlacementExpired          = "PlacementExpired"
)

func (e *Engine) recordRunEvent(run *repackv1alpha1.RepackRun, eventType, reason, message string) {
	if e == nil || e.recorder == nil || run == nil {
		return
	}
	if eventType == "" {
		eventType = v1.EventTypeNormal
	}
	e.recorder.Event(run, eventType, reason, message)
}

func plannedBenefitEventMessage(run *repackv1alpha1.RepackRun) string {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil {
		return "Repack plan computed."
	}
	summary := run.Status.Plan.Summary
	return fmt.Sprintf(
		"Plan computed: move %d PodGroups and %d cards to free %d nodes; cluster fragmentation %d%% -> %d%%.",
		len(run.Status.Plan.Moves), summary.MovedCardCount, summary.FreedNodeCount,
		summary.FragBeforePercent, summary.FragAfterPercent)
}
