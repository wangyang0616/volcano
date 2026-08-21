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

package workloaddisruption

import (
	"testing"

	"volcano.sh/volcano/pkg/repackengine/framework"
)

func TestConfiguredWorkloadDisruptionWeights(t *testing.T) {
	arguments := framework.Arguments{
		argAffectedPodGroupsWeight: 20,
		argMovedResourceWeight:     5,
		argMovedPodsWeight:         0,
	}
	if err := framework.ValidatePluginArguments(Name, arguments); err != nil {
		t.Fatalf("valid weights rejected: %v", err)
	}
	plugin, ok := framework.GetPlugin(Name, arguments)
	if !ok {
		t.Fatal("workloaddisruption plugin is not registered")
	}
	configured := plugin.(*workloadDisruptionPlugin)
	if configured.affectedPodGroupsWeight != 20 || configured.movedResourceWeight != 5 || configured.movedPodsWeight != 0 {
		t.Fatalf("configured weights=%+v, want 20/5/0", configured)
	}
}

func TestWorkloadDisruptionWeightValidation(t *testing.T) {
	for name, arguments := range map[string]framework.Arguments{
		"negative":   {argMovedPodsWeight: -1},
		"fractional": {argMovedPodsWeight: 0.5},
		"unknown":    {"movedPodWeight": 1},
	} {
		if err := framework.ValidatePluginArguments(Name, arguments); err == nil {
			t.Errorf("%s arguments should be rejected", name)
		}
	}
}
