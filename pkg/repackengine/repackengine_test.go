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
	"testing"

	v1 "k8s.io/api/core/v1"
)

// Only fully-qualified extended resources (a name with a domain "/") can be
// defragmented; core compute resources are rejected. This mirrors the CEL rule
// on spec.goals[0].resource and guards the --repack-default-resource fallback.
func TestSupportedTarget(t *testing.T) {
	supported := []v1.ResourceName{
		"nvidia.com/gpu",
		"huawei.com/Ascend910",
		"amd.com/gpu",
		"example.com/foo", // any extended resource is fine; the engine is name-agnostic
	}
	for _, r := range supported {
		if !supportedTarget(r) {
			t.Errorf("supportedTarget(%q) = false, want true", r)
		}
	}
	unsupported := []v1.ResourceName{
		v1.ResourceCPU,              // cpu
		v1.ResourceMemory,           // memory
		v1.ResourceEphemeralStorage, // ephemeral-storage
		v1.ResourcePods,             // pods
		"hugepages-2Mi",
		"",
	}
	for _, r := range unsupported {
		if supportedTarget(r) {
			t.Errorf("supportedTarget(%q) = true, want false", r)
		}
	}
}
