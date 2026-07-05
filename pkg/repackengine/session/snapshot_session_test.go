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

package session

import (
	"testing"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// SessionSnapshot.PodGroupView reads MinAvailable/Running/Priority/Footprint off
// the live JobInfos; unknown gangs yield a zero view.
func TestSessionSnapshot_PodGroupView(t *testing.T) {
	tasks := schedapi.TasksMap{
		"u0": gpuTask(0, "n0", 4),
		"u1": gpuTask(1, "n0", 4),
	}
	ji := &schedapi.JobInfo{
		MinAvailable:    2,
		Priority:        10,
		Tasks:           tasks,
		TaskStatusIndex: map[schedapi.TaskStatus]schedapi.TasksMap{schedapi.Running: tasks},
	}
	ssn := &schedframework.Session{Jobs: map[schedapi.JobID]*schedapi.JobInfo{"ns/big": ji}}
	snap := NewSessionSnapshot(ssn, gpu, nil)

	got := snap.PodGroupView("ns/big")
	want := api.PodGroupView{MinAvailable: 2, Running: 2, Priority: 10, Footprint: 8}
	if got != want {
		t.Errorf("view=%+v want %+v", got, want)
	}
	if z := snap.PodGroupView("ns/unknown"); z != (api.PodGroupView{}) {
		t.Errorf("unknown gang should yield zero view, got %+v", z)
	}
}

// Nodes returns every in-scope session node (nil filter = all).
func TestSessionSnapshot_Nodes(t *testing.T) {
	ssn := &schedframework.Session{Nodes: map[string]*schedapi.NodeInfo{
		"n0": capNode("n0", 8), "n1": capNode("n1", 8),
	}}
	snap := NewSessionSnapshot(ssn, gpu, nil)
	if len(snap.Nodes()) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(snap.Nodes()))
	}
}
