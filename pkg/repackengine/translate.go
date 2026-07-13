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
	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

// minFragImprovement reads the run's benefit gate (spec.goals[0].
// minFragImprovementPercent, percentage points 0-100; 0 = no gate).
func minFragImprovement(run *repackv1alpha1.RepackRun) int {
	if len(run.Spec.Goals) > 0 {
		return int(run.Spec.Goals[0].MinFragImprovementPercent)
	}
	return 0
}

// maxPerRun reads the run's blast-radius caps for the target resource. The bools
// distinguish an omitted cap (unlimited) from an explicit zero (move nothing).
func maxPerRun(run *repackv1alpha1.RepackRun, targetResource v1.ResourceName) (maxPodGroups int, maxResource int64, hasPodGroupLimit bool, hasResourceLimit bool) {
	if run.Spec.MaxPerRun == nil {
		return 0, 0, false, false
	}
	if run.Spec.MaxPerRun.PodGroups != nil {
		maxPodGroups = int(*run.Spec.MaxPerRun.PodGroups)
		hasPodGroupLimit = true
	}
	if quantity, ok := run.Spec.MaxPerRun.Resources[targetResource]; ok {
		// The user writes whole devices (e.g. "6" GPUs), but the drain budget counts
		// cards in Volcano's milli-units (1 device = 1000, via api.Scalar). Convert to
		// milli so maxResource and the running movedResource are the same unit.
		maxResource = quantity.MilliValue()
		hasResourceLimit = true
	}
	return maxPodGroups, maxResource, hasPodGroupLimit, hasResourceLimit
}
