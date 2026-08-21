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

package conf

import (
	"strings"

	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

func ResolveResource(run *repackv1alpha1.RepackRun, defaultResource string) v1.ResourceName {
	if len(run.Spec.Goals) > 0 && run.Spec.Goals[0].Resource != "" {
		return run.Spec.Goals[0].Resource
	}
	return v1.ResourceName(defaultResource)
}

func MinFragImprovement(run *repackv1alpha1.RepackRun) int {
	if len(run.Spec.Goals) == 0 {
		return 0
	}
	return int(run.Spec.Goals[0].MinFragImprovementPercent)
}

func MaxPerRun(run *repackv1alpha1.RepackRun, targetResource v1.ResourceName) (maxPodGroups int, maxResource int64, hasPodGroupLimit bool, hasResourceLimit bool) {
	if run.Spec.MaxPerRun == nil {
		return 0, 0, false, false
	}
	if run.Spec.MaxPerRun.PodGroups != nil {
		maxPodGroups = int(*run.Spec.MaxPerRun.PodGroups)
		hasPodGroupLimit = true
	}
	if quantity, found := run.Spec.MaxPerRun.Resources[targetResource]; found {
		// The planner accounts scalar resources in milli-units (one device is
		// 1000), so normalize the user-facing whole-device quantity here.
		maxResource = quantity.MilliValue()
		hasResourceLimit = true
	}
	return
}

func SupportedTarget(targetResource v1.ResourceName) bool {
	return strings.Contains(string(targetResource), "/")
}
