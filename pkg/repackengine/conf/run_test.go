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
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

const testGPUResource v1.ResourceName = "nvidia.com/gpu"

func TestResolveResource(t *testing.T) {
	run := &repackv1alpha1.RepackRun{}
	run.Spec.Goals = []repackv1alpha1.RepackGoal{{Resource: "huawei.com/Ascend910"}}
	if got := ResolveResource(run, string(testGPUResource)); got != "huawei.com/Ascend910" {
		t.Errorf("goals[0] should win, got %q", got)
	}
	if got := ResolveResource(&repackv1alpha1.RepackRun{}, string(testGPUResource)); got != testGPUResource {
		t.Errorf("empty goals should fall back to default, got %q", got)
	}
	if got := ResolveResource(&repackv1alpha1.RepackRun{}, ""); got != "" {
		t.Errorf("no goals, no default -> '', got %q", got)
	}
}

func TestMinFragImprovement(t *testing.T) {
	if got := MinFragImprovement(&repackv1alpha1.RepackRun{}); got != 0 {
		t.Errorf("no goals -> 0, got %d", got)
	}
	run := &repackv1alpha1.RepackRun{}
	run.Spec.Goals = []repackv1alpha1.RepackGoal{{Resource: testGPUResource, MinFragImprovementPercent: 25}}
	if got := MinFragImprovement(run); got != 25 {
		t.Errorf("goals[0].minFragImprovementPercent -> 25, got %d", got)
	}
}

func TestMaxPerRun(t *testing.T) {
	if pg, res, limitPG, limitRes := MaxPerRun(&repackv1alpha1.RepackRun{}, testGPUResource); pg != 0 || res != 0 || limitPG || limitRes {
		t.Errorf("nil maxPerRun -> unlimited 0,0; got %d,%d limits=%v,%v", pg, res, limitPG, limitRes)
	}

	pgCap := int32(3)
	run := &repackv1alpha1.RepackRun{}
	run.Spec.MaxPerRun = &repackv1alpha1.MaxPerRun{
		PodGroups: &pgCap,
		Resources: v1.ResourceList{testGPUResource: resource.MustParse("6")},
	}
	if pg, res, limitPG, limitRes := MaxPerRun(run, testGPUResource); pg != 3 || res != 6000 || !limitPG || !limitRes {
		t.Errorf("maxPerRun -> limited 3,6000; got %d,%d limits=%v,%v", pg, res, limitPG, limitRes)
	}

	run.Spec.MaxPerRun = &repackv1alpha1.MaxPerRun{
		Resources: v1.ResourceList{"amd.com/gpu": resource.MustParse("9")},
	}
	if _, res, _, limited := MaxPerRun(run, testGPUResource); res != 0 || limited {
		t.Errorf("cap for other resource -> 0 for gpu; got %d", res)
	}
}

func TestMaxPerRunExplicitZeroIsLimited(t *testing.T) {
	zero := int32(0)
	run := &repackv1alpha1.RepackRun{}
	run.Spec.MaxPerRun = &repackv1alpha1.MaxPerRun{
		PodGroups: &zero,
		Resources: v1.ResourceList{testGPUResource: resource.MustParse("0")},
	}
	pg, res, limitPG, limitRes := MaxPerRun(run, testGPUResource)
	if pg != 0 || res != 0 || !limitPG || !limitRes {
		t.Fatalf("explicit zero must remain an active zero cap; got %d,%d limits=%v,%v", pg, res, limitPG, limitRes)
	}
}

func TestSupportedTarget(t *testing.T) {
	for _, resourceName := range []v1.ResourceName{"nvidia.com/gpu", "huawei.com/Ascend910", "amd.com/gpu", "example.com/foo"} {
		if !SupportedTarget(resourceName) {
			t.Errorf("SupportedTarget(%q) = false, want true", resourceName)
		}
	}
	for _, resourceName := range []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory, v1.ResourceEphemeralStorage, v1.ResourcePods, "hugepages-2Mi", ""} {
		if SupportedTarget(resourceName) {
			t.Errorf("SupportedTarget(%q) = true, want false", resourceName)
		}
	}
}
