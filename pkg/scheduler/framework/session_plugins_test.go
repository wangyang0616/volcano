/*
Copyright 2024 The Volcano Authors.

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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	topologyv1alpha1 "volcano.sh/apis/pkg/apis/topology/v1alpha1"

	schedulingv1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/cache"
	"volcano.sh/volcano/pkg/scheduler/util"
)

func newFitErr(taskName, nodeName string, sts ...*api.Status) *api.FitError {
	return api.NewFitErrWithStatus(&api.TaskInfo{Name: taskName}, &api.NodeInfo{Name: nodeName}, sts...)
}

func TestFilterOutPreemptMayNotHelpNodes(t *testing.T) {
	tests := []struct {
		Name      string
		PodGroups []*schedulingv1.PodGroup
		Pods      []*v1.Pod
		Nodes     []*v1.Node
		Queues    []*schedulingv1.Queue
		status    map[api.TaskID]*api.FitError
		want      map[api.TaskID][]string // task's nodes name list which is helpful for preemption
	}{
		{
			Name:      "all are helpful for preemption",
			PodGroups: []*schedulingv1.PodGroup{util.BuildPodGroup("pg1", "c1", "c1", 1, nil, schedulingv1.PodGroupInqueue)},
			Pods: []*v1.Pod{
				util.BuildPod("c1", "p1", "", v1.PodPending, api.BuildResourceList("2", "1G"), "pg1", map[string]string{"volcano.sh/task-spec": "master"}, nil),
				util.BuildPod("c1", "p2", "", v1.PodPending, api.BuildResourceList("2", "1G"), "pg1", map[string]string{"volcano.sh/task-spec": "worker"}, nil),
			},
			Nodes: []*v1.Node{
				util.BuildNode("n1", api.BuildResourceList("2", "4Gi", []api.ScalarResource{{Name: "pods", Value: "10"}}...), map[string]string{"nodeRole": "worker"}),
				util.BuildNode("n2", api.BuildResourceList("2", "4Gi", []api.ScalarResource{{Name: "pods", Value: "10"}}...), map[string]string{"nodeRole": "worker"}),
			},
			Queues: []*schedulingv1.Queue{util.BuildQueue("c1", 1, nil)},
			status: map[api.TaskID]*api.FitError{},
			want:   map[api.TaskID][]string{"c1-p2": {"n1", "n2"}, "c1-p1": {"n1", "n2"}},
		},
		{
			Name:      "master predicate failed: node selector does not match",
			PodGroups: []*schedulingv1.PodGroup{util.BuildPodGroup("pg1", "c1", "c1", 1, nil, schedulingv1.PodGroupInqueue)},
			Pods: []*v1.Pod{
				util.BuildPod("c1", "p1", "", v1.PodPending, api.BuildResourceList("2", "1G"), "pg1", map[string]string{"volcano.sh/task-spec": "master"}, map[string]string{"nodeRole": "master"}),
				util.BuildPod("c1", "p2", "", v1.PodPending, api.BuildResourceList("2", "1G"), "pg1", map[string]string{"volcano.sh/task-spec": "worker"}, map[string]string{"nodeRole": "worker"}),
			},
			Nodes:  []*v1.Node{util.BuildNode("n1", api.BuildResourceList("2", "4Gi", []api.ScalarResource{{Name: "pods", Value: "10"}}...), map[string]string{"nodeRole": "worker"})},
			Queues: []*schedulingv1.Queue{util.BuildQueue("c1", 1, nil)},
			status: map[api.TaskID]*api.FitError{"c1-p1": newFitErr("c1-p1", "n1", &api.Status{Reason: "node(s) didn't match Pod's node selector", Code: api.UnschedulableAndUnresolvable})},
			want:   map[api.TaskID][]string{"c1-p2": {"n1"}, "c1-p1": {}},
		},
		{
			Name:      "p1,p3 has node fit error",
			PodGroups: []*schedulingv1.PodGroup{util.BuildPodGroup("pg1", "c1", "c1", 2, map[string]int32{"master": 1, "worker": 1}, schedulingv1.PodGroupInqueue)},
			Pods: []*v1.Pod{
				util.BuildPod("c1", "p0", "", v1.PodPending, api.BuildResourceList("1", "1G"), "pg1", map[string]string{"volcano.sh/task-spec": "master"}, map[string]string{"nodeRole": "master"}),
				util.BuildPod("c1", "p1", "", v1.PodPending, api.BuildResourceList("1", "1G"), "pg1", map[string]string{"volcano.sh/task-spec": "master"}, map[string]string{"nodeRole": "master"}),
				util.BuildPod("c1", "p2", "", v1.PodPending, api.BuildResourceList("1", "1G"), "pg1", map[string]string{"volcano.sh/task-spec": "worker"}, map[string]string{"nodeRole": "worker"}),
				util.BuildPod("c1", "p3", "", v1.PodPending, api.BuildResourceList("1", "1G"), "pg1", map[string]string{"volcano.sh/task-spec": "worker"}, map[string]string{"nodeRole": "worker"}),
			},
			Nodes: []*v1.Node{
				util.BuildNode("n1", api.BuildResourceList("1", "2Gi", []api.ScalarResource{{Name: "pods", Value: "10"}}...), map[string]string{"nodeRole": "master"}),
				util.BuildNode("n2", api.BuildResourceList("1", "2Gi", []api.ScalarResource{{Name: "pods", Value: "10"}}...), map[string]string{"nodeRole": "worker"}),
			},
			Queues: []*schedulingv1.Queue{util.BuildQueue("c1", 1, nil)},
			status: map[api.TaskID]*api.FitError{
				"c1-p1": newFitErr("c1-p1", "n2", &api.Status{Reason: "node(s) didn't match Pod's node selector", Code: api.UnschedulableAndUnresolvable}),
				"c1-p3": newFitErr("c1-p3", "n1", &api.Status{Reason: "node(s) didn't match Pod's node selector", Code: api.UnschedulableAndUnresolvable}),
			},
			// notes that are useful for preempting
			want: map[api.TaskID][]string{
				"c1-p0": {"n1", "n2"},
				"c1-p1": {"n1"},
				"c1-p2": {"n1", "n2"},
				"c1-p3": {"n2"},
			},
		},
	}

	for i, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			scherCache := cache.NewDefaultMockSchedulerCache("test-scheduler")
			for _, node := range test.Nodes {
				scherCache.AddOrUpdateNode(node)
			}
			for _, pod := range test.Pods {
				scherCache.AddPod(pod)
			}
			for _, pg := range test.PodGroups {
				scherCache.AddPodGroupV1beta1(pg)
			}
			for _, queue := range test.Queues {
				scherCache.AddQueueV1beta1(queue)
			}
			ssn := OpenSession(scherCache, nil, nil)
			defer CloseSession(ssn)
			for _, job := range ssn.Jobs {
				for _, task := range job.TaskStatusIndex[api.Pending] {
					if fitErr, exist := test.status[task.UID]; exist {
						fe := api.NewFitErrors()
						fe.SetNodeError(fitErr.NodeName, fitErr)
						job.NodesFitErrors[task.UID] = fe
					}

					// check potential nodes
					potentialNodes := ssn.FilterOutUnschedulableAndUnresolvableNodesForTask(task)
					want := test.want[task.UID]
					got := make([]string, 0, len(potentialNodes))
					for _, node := range potentialNodes {
						got = append(got, node.Name)
					}
					assert.Equal(t, want, got, fmt.Sprintf("case %d: task %s", i, task.UID))
				}
			}
		})
	}
}

func testHyperNodeInfo(name string, tier int) *api.HyperNodeInfo {
	return api.NewHyperNodeInfo(api.BuildHyperNode(name, tier, nil))
}

func TestIntersectHyperNodeGradients(t *testing.T) {
	pluginA := [][]*api.HyperNodeInfo{
		{testHyperNodeInfo("a1", 1), testHyperNodeInfo("a2", 1)},
		{testHyperNodeInfo("a3", 2)},
	}
	pluginB := [][]*api.HyperNodeInfo{
		{testHyperNodeInfo("a2", 1), testHyperNodeInfo("b1", 1)},
		{testHyperNodeInfo("a3", 2), testHyperNodeInfo("b2", 2)},
	}

	result := intersectHyperNodeGradients([][][]*api.HyperNodeInfo{pluginA, pluginB})
	assert.Len(t, result, 2)
	assert.Equal(t, []string{"a2"}, hyperNodeNamesAtTier(result, 0))
	assert.Equal(t, []string{"a3"}, hyperNodeNamesAtTier(result, 1))

	empty := intersectHyperNodeGradients([][][]*api.HyperNodeInfo{
		{{testHyperNodeInfo("only-a", 1)}},
		{{testHyperNodeInfo("only-b", 1)}},
	})
	assert.Nil(t, empty)
}

func TestIntersectHyperNodeGradientsSinglePlugin(t *testing.T) {
	gradients := [][]*api.HyperNodeInfo{{testHyperNodeInfo("x", 1)}}
	assert.Equal(t, gradients, intersectHyperNodeGradients([][][]*api.HyperNodeInfo{gradients}))
}

func hyperNodeNamesAtTier(gradients [][]*api.HyperNodeInfo, tierIdx int) []string {
	if tierIdx >= len(gradients) {
		return nil
	}
	names := make([]string, 0, len(gradients[tierIdx]))
	for _, h := range gradients[tierIdx] {
		names = append(names, h.Name)
	}
	return names
}

func TestRebuildGradientsByTierPreservesTierOrder(t *testing.T) {
	members := []api.MemberConfig{{Name: "child", Type: topologyv1alpha1.MemberTypeHyperNode, Selector: "exact"}}
	parent := api.NewHyperNodeInfo(api.BuildHyperNode("parent", 2, members))
	child := api.NewHyperNodeInfo(api.BuildHyperNode("child", 1, nil))
	parent.Children.Insert("child")

	byName := map[string]*api.HyperNodeInfo{"parent": parent, "child": child}
	eligible := sets.New("parent", "child")

	result := rebuildGradientsByTier(byName, eligible)
	assert.Len(t, result, 2)
	assert.Equal(t, 1, result[0][0].Tier())
	assert.Equal(t, 2, result[1][0].Tier())
}

// buildBenchmarkPluginGradients builds [plugin][tier-layer][hyperNodes] for intersection benchmarks.
// Each plugin exposes tier-1 HyperNodes hn-1-0..hn-1-(numTier1-1) plus one tier-2 root.
// pluginOffset skips the first N tier-1 HyperNodes to simulate partial overlap across plugins.
func buildBenchmarkPluginGradients(numPlugins, numTier1, pluginOffset int) [][][]*api.HyperNodeInfo {
	tier1 := make([]*api.HyperNodeInfo, 0, numTier1)
	for index := 0; index < numTier1; index++ {
		tier1 = append(tier1, testHyperNodeInfo(fmt.Sprintf("hn-1-%d", index), 1))
	}
	tier2 := testHyperNodeInfo("hn-2-0", 2)

	pluginGradients := make([][][]*api.HyperNodeInfo, 0, numPlugins)
	for pluginIndex := 0; pluginIndex < numPlugins; pluginIndex++ {
		offset := pluginIndex * pluginOffset
		pluginTier1 := tier1
		if offset > 0 && offset < len(tier1) {
			pluginTier1 = tier1[offset:]
		}
		pluginGradients = append(pluginGradients, [][]*api.HyperNodeInfo{pluginTier1, {tier2}})
	}
	return pluginGradients
}

func BenchmarkIntersectHyperNodeGradients(b *testing.B) {
	// Typical production shape: 2 plugins, 100 tier-1 HyperNodes, partial overlap.
	pluginGradients := buildBenchmarkPluginGradients(2, 100, 10)

	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		intersectHyperNodeGradients(pluginGradients)
	}
}

func BenchmarkIntersectHyperNodeGradients_ManyPlugins(b *testing.B) {
	pluginGradients := buildBenchmarkPluginGradients(5, 100, 5)

	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		intersectHyperNodeGradients(pluginGradients)
	}
}

func BenchmarkIntersectHyperNodeGradients_LargeCluster(b *testing.B) {
	// 1000 tier-1 HyperNodes to stress nested loops in phase 2.
	pluginGradients := buildBenchmarkPluginGradients(2, 1000, 50)

	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		intersectHyperNodeGradients(pluginGradients)
	}
}
