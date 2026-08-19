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

package repack

import (
	"testing"

	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/base"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/binpack"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/budget"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/gang"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/node"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/resource"
)

const testResource = v1.ResourceName("nvidia.com/gpu")

type actionSnapshot struct {
	nodes []*schedapi.NodeInfo
}

func (s *actionSnapshot) Nodes() []*schedapi.NodeInfo       { return s.nodes }
func (*actionSnapshot) NodeInScope(*schedapi.NodeInfo) bool { return true }
func (*actionSnapshot) PodGroupView(schedapi.JobID) api.PodGroupView {
	return api.PodGroupView{MinAvailable: 1, Running: 1, Footprint: 1}
}
func (*actionSnapshot) FeasibleRelocation(_ []*api.Move, victims []*schedapi.TaskInfo, receivers []*schedapi.NodeInfo) ([]*api.Move, bool) {
	if len(receivers) == 0 {
		return nil, false
	}
	moves := make([]*api.Move, 0, len(victims))
	for _, victim := range victims {
		moves = append(moves, &api.Move{Task: victim, From: victim.NodeName, To: receivers[0].Name})
	}
	return moves, true
}

func actionResource(value int64) *schedapi.Resource {
	return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{testResource: float64(value)}}
}

func actionNode(name string, capacity int64, task *schedapi.TaskInfo) *schedapi.NodeInfo {
	tasks := map[schedapi.TaskID]*schedapi.TaskInfo{}
	used := int64(0)
	if task != nil {
		task.NodeName = name
		tasks[schedapi.TaskID(task.Name)] = task
		used = api.Scalar(task.InitResreq, testResource)
	}
	return &schedapi.NodeInfo{Name: name, Tasks: tasks, Allocatable: actionResource(capacity), Used: actionResource(used)}
}

func actionSession(minNodesFreed int) *framework.Session {
	victimResource := actionResource(2)
	receiverResource := actionResource(4)
	snapshot := &actionSnapshot{nodes: []*schedapi.NodeInfo{
		actionNode("victim", 8, &schedapi.TaskInfo{Name: "victim-pod", Job: "ns/victim", InitResreq: victimResource, Resreq: victimResource}),
		actionNode("receiver", 8, &schedapi.TaskInfo{Name: "receiver-pod", Job: "ns/receiver", InitResreq: receiverResource, Resreq: receiverResource}),
		actionNode("empty", 8, nil),
	}}
	return framework.OpenSession(framework.SessionConfig{
		Snapshot:      snapshot,
		Resource:      testResource,
		Mode:          repackv1alpha1.RepackModeDryRun,
		MinNodesFreed: minNodesFreed,
		Free: func(node *schedapi.NodeInfo) *schedapi.Resource {
			return actionResource(api.Scalar(node.Allocatable, testResource) - api.Scalar(node.Used, testResource))
		},
	}, []string{"resource", "budget", "node", "base", "gang", "binpack"})
}

func TestRepackActionOwnsPlanAdmissionAndReport(t *testing.T) {
	ssn := actionSession(1)
	defer framework.CloseSession(ssn)

	(&repackAction{}).Execute(ssn)

	if ssn.Plan() == nil || ssn.Report().NodesFreed != 1 {
		t.Fatalf("plan=%v report=%+v, want one admitted freed node", ssn.Plan(), ssn.Report())
	}
	if ssn.Plan().Cost.MovedResource != 2 || ssn.Report().MovedResource != 2 {
		t.Fatalf("cost=%+v report=%+v, want action-computed moved resource 2", ssn.Plan().Cost, ssn.Report())
	}
}

func TestRepackActionRejectsBelowBenefitButPreservesCurrentMetric(t *testing.T) {
	ssn := actionSession(2)
	defer framework.CloseSession(ssn)

	(&repackAction{}).Execute(ssn)

	if ssn.Plan() != nil {
		t.Fatalf("plan=%v, want benefit constraint rejection", ssn.Plan())
	}
	if ssn.Report().FragmentationRateBefore <= 0 || ssn.Report().FragmentationRateAfter != ssn.Report().FragmentationRateBefore {
		t.Fatalf("report=%+v, want current fragmentation retained for rejected plan", ssn.Report())
	}
}
