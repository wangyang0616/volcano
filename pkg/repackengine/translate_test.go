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
	"k8s.io/apimachinery/pkg/api/resource"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

func TestResolveResource(t *testing.T) {
	e := &Engine{cfg: Config{DefaultResource: "nvidia.com/gpu"}}

	// goals[0].resource wins when set.
	run := &repackv1alpha1.RepackRun{}
	run.Spec.Goals = []repackv1alpha1.RepackGoal{{Resource: "huawei.com/Ascend910"}}
	if got := e.resolveResource(run); got != "huawei.com/Ascend910" {
		t.Errorf("goals[0] should win, got %q", got)
	}

	// empty goals -> engine default.
	if got := e.resolveResource(&repackv1alpha1.RepackRun{}); got != "nvidia.com/gpu" {
		t.Errorf("empty goals should fall back to default, got %q", got)
	}

	// empty goals AND empty default -> "" (driver then fails NoTargetResource).
	e0 := &Engine{cfg: Config{}}
	if got := e0.resolveResource(&repackv1alpha1.RepackRun{}); got != "" {
		t.Errorf("no goals, no default -> '', got %q", got)
	}
}

func TestMinFragImprovement(t *testing.T) {
	if got := minFragImprovement(&repackv1alpha1.RepackRun{}); got != 0 {
		t.Errorf("no goals -> 0, got %d", got)
	}
	run := &repackv1alpha1.RepackRun{}
	run.Spec.Goals = []repackv1alpha1.RepackGoal{{Resource: "nvidia.com/gpu", MinFragImprovementPercent: 25}}
	if got := minFragImprovement(run); got != 25 {
		t.Errorf("goals[0].minFragImprovementPercent -> 25, got %d", got)
	}
}

func TestMaxPerRun(t *testing.T) {
	// nil MaxPerRun -> unlimited (0, 0).
	if pg, res := maxPerRun(&repackv1alpha1.RepackRun{}, gpuResource); pg != 0 || res != 0 {
		t.Errorf("nil maxPerRun -> 0,0; got %d,%d", pg, res)
	}

	pgCap := int32(3)
	run := &repackv1alpha1.RepackRun{}
	run.Spec.MaxPerRun = &repackv1alpha1.MaxPerRun{
		PodGroups: &pgCap,
		Resources: v1.ResourceList{gpuResource: resource.MustParse("6")},
	}
	if pg, res := maxPerRun(run, gpuResource); pg != 3 || res != 6 {
		t.Errorf("maxPerRun -> 3,6; got %d,%d", pg, res)
	}

	// cap set for a different resource -> res cap is 0 (unlimited) for gpu.
	run2 := &repackv1alpha1.RepackRun{}
	run2.Spec.MaxPerRun = &repackv1alpha1.MaxPerRun{
		Resources: v1.ResourceList{"amd.com/gpu": resource.MustParse("9")},
	}
	if _, res := maxPerRun(run2, gpuResource); res != 0 {
		t.Errorf("cap for other resource -> 0 for gpu; got %d", res)
	}
}
