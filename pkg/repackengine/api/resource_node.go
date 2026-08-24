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

package api

import (
	v1 "k8s.io/api/core/v1"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

// TargetResourceNodeClass describes one node's allocation shape for the target
// resource. Repack consolidates only partially occupied nodes: empty nodes must
// remain idle and fully occupied nodes are already in a compact layout.
type TargetResourceNodeClass string

const (
	TargetResourceNodeUnavailable TargetResourceNodeClass = "unavailable"
	TargetResourceNodeEmpty       TargetResourceNodeClass = "empty"
	TargetResourceNodePartial     TargetResourceNodeClass = "partial"
	TargetResourceNodeFull        TargetResourceNodeClass = "full"
)

// NodeFreeCapacity returns the immediately available resources computed from
// the authoritative Allocatable and Used ledgers. Repack cannot rely only on
// NodeInfo.Idle: when an extended resource is added to node status after pods
// bind, the scheduler cache can already expose the new Allocatable value while
// Idle still lacks that resource until the node snapshot is rebuilt.
func NodeFreeCapacity(node *schedapi.NodeInfo) *schedapi.Resource {
	if node == nil || node.Allocatable == nil {
		return schedapi.EmptyResource()
	}
	free := node.Allocatable.Clone()
	if node.Used != nil {
		free.SubWithoutAssert(node.Used)
	}
	return free
}

// ClassifyTargetResourceNode classifies a node from the scheduler snapshot's
// Allocatable and Used values. Used values at or above Allocatable are treated
// as full defensively.
func ClassifyTargetResourceNode(node *schedapi.NodeInfo, resourceName v1.ResourceName) TargetResourceNodeClass {
	if node == nil || node.Allocatable == nil {
		return TargetResourceNodeUnavailable
	}
	capacity := Scalar(node.Allocatable, resourceName)
	if capacity <= 0 {
		return TargetResourceNodeUnavailable
	}
	used := Scalar(node.Used, resourceName)
	switch {
	case used <= 0:
		return TargetResourceNodeEmpty
	case used >= capacity:
		return TargetResourceNodeFull
	default:
		return TargetResourceNodePartial
	}
}
