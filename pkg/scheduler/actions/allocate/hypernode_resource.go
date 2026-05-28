/*
Copyright 2025 The Volcano Authors.

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

package allocate

import (
	"volcano.sh/volcano/pkg/scheduler/api"
)

// filterGradientsByMinResource removes HyperNodes that cannot satisfy minResource using the session ledger.
// Partially allocated Job/SubJob skips filtering, consistent with legacy network-topology-aware behavior.
func (alloc *Action) filterGradientsByMinResource(
	job *api.JobInfo,
	subJob *api.SubJobInfo,
	gradients [][]*api.HyperNodeInfo,
) [][]*api.HyperNodeInfo {
	if subJob != nil && subJob.AllocatedHyperNode != "" {
		return gradients
	}
	if subJob == nil && job.AllocatedHyperNode != "" {
		return gradients
	}

	var minResource *api.Resource
	if subJob != nil {
		minResource = subJob.GetMinResources()
	} else {
		minResource = job.GetMinResources()
	}
	if minResource == nil || minResource.IsEmpty() {
		return gradients
	}

	ssn := alloc.session
	filtered := make([][]*api.HyperNodeInfo, 0, len(gradients))
	for _, layer := range gradients {
		kept := make([]*api.HyperNodeInfo, 0, len(layer))
		for _, hn := range layer {
			if hn == nil {
				continue
			}
			if ssn.HyperNodeSatisfiesMinResource(hn.Name, minResource) {
				kept = append(kept, hn)
			}
		}
		if len(kept) > 0 {
			filtered = append(filtered, kept)
		}
	}
	return filtered
}
