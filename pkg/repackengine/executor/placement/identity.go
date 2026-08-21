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

package placement

import repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

// Identity is the stable key shared by plan moves and durable relocation
// records. It is intentionally independent of replacement Pod identity.
type Identity struct {
	Namespace    string
	PodGroupName string
	PodName      string
	TargetNode   string
}

func IdentityForRelocation(relocation *repackv1alpha1.PodRelocationStatus) Identity {
	if relocation == nil {
		return Identity{}
	}
	return Identity{
		Namespace: relocation.Namespace, PodGroupName: relocation.PodGroupName,
		PodName: relocation.VictimPodName, TargetNode: relocation.PlannedNodeName,
	}
}

func IdentityForMove(namespace, podGroupName, podName, targetNode string) Identity {
	return Identity{Namespace: namespace, PodGroupName: podGroupName, PodName: podName, TargetNode: targetNode}
}

func (identity Identity) Less(other Identity) bool {
	switch {
	case identity.Namespace != other.Namespace:
		return identity.Namespace < other.Namespace
	case identity.PodGroupName != other.PodGroupName:
		return identity.PodGroupName < other.PodGroupName
	case identity.PodName != other.PodName:
		return identity.PodName < other.PodName
	default:
		return identity.TargetNode < other.TargetNode
	}
}
