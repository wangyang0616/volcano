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

package api

// HyperNodeResourceStatusMap stores per-HyperNode resource accounting for allocate pre-filtering.
type HyperNodeResourceStatusMap map[string]*HyperNodeResourceStatus

// HyperNodeResourceStatus is the session-readable HyperNode resource ledger.
type HyperNodeResourceStatus struct {
	Allocatable *Resource
	Used        *Resource
	Idle        *Resource
	FutureIdle  *Resource
}

// SatisfiesMinResource reports whether minResource fits in idle or futureIdle capacity.
// Missing status entries are treated as satisfiable (same as legacy network-topology-aware behavior).
func (status *HyperNodeResourceStatus) SatisfiesMinResource(minResource *Resource) bool {
	if status == nil || minResource == nil || minResource.IsEmpty() {
		return true
	}
	if minResource.LessEqual(status.Idle, Zero) || minResource.LessEqual(status.FutureIdle, Zero) {
		return true
	}
	return false
}

// Clone returns a deep copy of HyperNodeResourceStatus.
func (status *HyperNodeResourceStatus) Clone() *HyperNodeResourceStatus {
	if status == nil {
		return nil
	}
	return &HyperNodeResourceStatus{
		Allocatable: status.Allocatable.Clone(),
		Used:        status.Used.Clone(),
		Idle:        status.Idle.Clone(),
		FutureIdle:  status.FutureIdle.Clone(),
	}
}
