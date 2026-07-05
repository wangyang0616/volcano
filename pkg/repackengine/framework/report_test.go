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

package framework

import (
	"math"
	"testing"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// RenderReport reports absolute fragmentation before/after: after = before +
// delta, where freeing a node drops B by 1 → (B-A-freed)/M.
func TestRenderReport_FragBeforeAfter(t *testing.T) {
	plan := &api.RepackPlan{
		Before:     api.ResourceFrag{M: 10, B: 6, A: 2}, // before = (6-2)/10 = 0.4
		FreedNodes: []string{"n0", "n1"},                // delta = -2/10; after = 0.2
	}
	r := RenderReport(plan)
	if math.Abs(r.FragRateBefore-0.4) > 1e-9 {
		t.Errorf("before=%v, want 0.4", r.FragRateBefore)
	}
	if math.Abs(r.FragRateAfter-0.2) > 1e-9 {
		t.Errorf("after=%v, want 0.2", r.FragRateAfter)
	}
	if r.NodesFreed != 2 {
		t.Errorf("nodesFreed=%d, want 2", r.NodesFreed)
	}

	// Nil plan → empty report (all zero).
	if r0 := RenderReport(nil); r0.FragRateBefore != 0 || r0.FragRateAfter != 0 {
		t.Errorf("nil plan report=%+v, want zero", r0)
	}
}
