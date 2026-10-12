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

package extend

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"volcano.sh/volcano/pkg/agent/apis"
	"volcano.sh/volcano/pkg/agent/oversubscription/queue"
	"volcano.sh/volcano/pkg/resourceusage"
)

type partialUsageGetter struct {
	usage resourceusage.Resource
}

func TestSourceFailureWithdrawsOversubscription(t *testing.T) {
	for _, tt := range []struct {
		name    string
		getNode func() (*corev1.Node, error)
		getPods func() ([]*corev1.Pod, error)
	}{
		{"node unavailable", func() (*corev1.Node, error) { return nil, errors.New("node unavailable") }, nil},
		{"pods unavailable", func() (*corev1.Node, error) {
			return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{apis.OverSubscriptionNodeLabelKey: "true"}}}, nil
		}, func() ([]*corev1.Pod, error) { return nil, errors.New("pods unavailable") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := queue.NewSqQueue()
			q.Enqueue(apis.Resource{corev1.ResourceCPU: 1000, corev1.ResourceMemory: 2000})
			e := &extendResource{getNodeFunc: tt.getNode, getPodsFunc: tt.getPods, queue: q}
			e.CalOverSubscriptionResources()
			items := q.GetAll()
			last := items[len(items)-1]
			if last[corev1.ResourceCPU] != 0 || last[corev1.ResourceMemory] != 0 {
				t.Fatalf("last oversubscription = %v; want both zero", last)
			}
		})
	}
}

func (g partialUsageGetter) UsagesByValue(bool, bool) resourceusage.Resource {
	return g.usage
}

func (g partialUsageGetter) UsagesByPercentage(*corev1.Node) resourceusage.Resource {
	return nil
}

func TestMissingUsageWithdrawsOversubscription(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{apis.OverSubscriptionNodeLabelKey: "true"}},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(3000, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(10000, resource.BinarySI),
		}},
	}
	for _, tt := range []struct {
		name                string
		usage               resourceusage.Resource
		wantCPU, wantMemory int64
	}{
		{"missing CPU", resourceusage.Resource{corev1.ResourceMemory: 2000}, 0, 4800},
		{"missing memory", resourceusage.Resource{corev1.ResourceCPU: 1000}, 1200, 0},
		{"missing both", resourceusage.Resource{}, 0, 0},
		{"measured zero usage", resourceusage.Resource{corev1.ResourceCPU: 0, corev1.ResourceMemory: 0}, 1800, 6000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := queue.NewSqQueue()
			e := &extendResource{
				getNodeFunc: func() (*corev1.Node, error) { return node, nil },
				getPodsFunc: func() ([]*corev1.Pod, error) { return nil, nil },
				usageGetter: partialUsageGetter{usage: tt.usage},
				queue:       q,
				ratio:       60,
			}
			e.CalOverSubscriptionResources()
			items := q.GetAll()
			if len(items) != 1 || items[0][corev1.ResourceCPU] != tt.wantCPU || items[0][corev1.ResourceMemory] != tt.wantMemory {
				t.Fatalf("oversubscription = %v; want CPU=%d memory=%d", items, tt.wantCPU, tt.wantMemory)
			}
		})
	}
}
