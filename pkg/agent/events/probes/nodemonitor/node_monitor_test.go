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

package nodemonitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/util/workqueue"
	"volcano.sh/volcano/pkg/agent/apis"
	"volcano.sh/volcano/pkg/agent/events/framework"
	"volcano.sh/volcano/pkg/agent/oversubscription/policy"
	"volcano.sh/volcano/pkg/agent/oversubscription/policy/extend"
	"volcano.sh/volcano/pkg/agent/utils/eviction"
	utilnode "volcano.sh/volcano/pkg/agent/utils/node"
	utilpod "volcano.sh/volcano/pkg/agent/utils/pod"
	"volcano.sh/volcano/pkg/config"
	"volcano.sh/volcano/pkg/resourceusage"
)

func makeNode() (*v1.Node, error) {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			Labels: map[string]string{
				apis.OverSubscriptionNodeLabelKey: "true",
			},
		},
		Spec: v1.NodeSpec{Taints: []v1.Taint{{
			Key:    apis.PodEvictingKey,
			Effect: v1.TaintEffectNoSchedule,
		}}},
	}, nil
}

type partialUsageGetter struct {
	value      resourceusage.Resource
	percentage resourceusage.Resource
}

func (g partialUsageGetter) UsagesByValue(bool, bool) resourceusage.Resource    { return g.value }
func (g partialUsageGetter) UsagesByPercentage(*v1.Node) resourceusage.Resource { return g.percentage }

func Test_monitor_detect(t *testing.T) {
	tests := []struct {
		name                    string
		Configuration           *config.Configuration
		policy                  func(cfg *config.Configuration, pods utilpod.ActivePods, evictor eviction.Eviction) policy.Interface
		queue                   workqueue.RateLimitingInterface
		lowWatermark            apis.Watermark
		highWatermark           apis.Watermark
		highUsageCountByResName map[v1.ResourceName]int
		getNodeFunc             utilnode.ActiveNode
		getPodsFunc             utilpod.ActivePods
		usageGetter             resourceusage.Getter
		expectedNode            func() *v1.Node
		expectedRes             v1.ResourceName
		expectedLen             int
	}{
		{
			name:                    "cpu in high usage with extend resource",
			highUsageCountByResName: map[v1.ResourceName]int{v1.ResourceCPU: 6},
			getNodeFunc:             makeNode,
			getPodsFunc: func() ([]*v1.Pod, error) {
				return []*v1.Pod{}, nil
			},
			policy: func(cfg *config.Configuration, pods utilpod.ActivePods, evictor eviction.Eviction) policy.Interface {
				return extend.NewExtendResource(cfg, nil, evictor, nil, "")
			},
			usageGetter: resourceusage.NewFakeResourceGetter(0, 0, 60, 60),
			expectedRes: v1.ResourceCPU,
			expectedLen: 1,
		},
		{
			name:                    "remove taint when use extend resource",
			highUsageCountByResName: map[v1.ResourceName]int{v1.ResourceCPU: 5},
			getNodeFunc:             makeNode,
			getPodsFunc: func() ([]*v1.Pod, error) {
				return []*v1.Pod{}, nil
			},
			policy: func(cfg *config.Configuration, pods utilpod.ActivePods, evictor eviction.Eviction) policy.Interface {
				return extend.NewExtendResource(cfg, nil, evictor, nil, "")
			},
			usageGetter: resourceusage.NewFakeResourceGetter(0, 0, 20, 20),
			expectedRes: v1.ResourceCPU,
			expectedNode: func() *v1.Node {
				node, err := makeNode()
				assert.NoError(t, err)
				node.Spec.Taints = nil
				return node
			},
			expectedLen: 0,
		},
		{
			name:                    "do not evict after CPU usage recovers below high watermark",
			highUsageCountByResName: map[v1.ResourceName]int{v1.ResourceCPU: 6},
			getNodeFunc:             makeNode,
			getPodsFunc:             func() ([]*v1.Pod, error) { return nil, nil },
			policy: func(cfg *config.Configuration, pods utilpod.ActivePods, evictor eviction.Eviction) policy.Interface {
				return extend.NewExtendResource(cfg, nil, evictor, nil, "")
			},
			usageGetter: resourceusage.NewFakeResourceGetter(0, 0, 20, 20),
			expectedLen: 0,
		},
		{
			name:                    "keep taint when memory usage is unavailable",
			highUsageCountByResName: map[v1.ResourceName]int{},
			getNodeFunc:             makeNode,
			getPodsFunc:             func() ([]*v1.Pod, error) { return nil, nil },
			policy: func(cfg *config.Configuration, pods utilpod.ActivePods, evictor eviction.Eviction) policy.Interface {
				return extend.NewExtendResource(cfg, nil, evictor, nil, "")
			},
			usageGetter:  partialUsageGetter{percentage: resourceusage.Resource{v1.ResourceCPU: 20}},
			expectedNode: func() *v1.Node { node, _ := makeNode(); return node },
			expectedLen:  0,
		},
		{
			name:                    "do not evict based on stale CPU pressure when usage is unavailable",
			highUsageCountByResName: map[v1.ResourceName]int{v1.ResourceCPU: 6},
			getNodeFunc:             makeNode,
			getPodsFunc:             func() ([]*v1.Pod, error) { return nil, nil },
			policy: func(cfg *config.Configuration, pods utilpod.ActivePods, evictor eviction.Eviction) policy.Interface {
				return extend.NewExtendResource(cfg, nil, evictor, nil, "")
			},
			usageGetter:  partialUsageGetter{percentage: resourceusage.Resource{v1.ResourceMemory: 20}},
			expectedNode: func() *v1.Node { node, _ := makeNode(); return node },
			expectedLen:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeNode, err := makeNode()
			assert.NoError(t, err)
			fakeClient := fakeclientset.NewSimpleClientset(fakeNode)
			cfg := &config.Configuration{GenericConfiguration: &config.VolcanoAgentConfiguration{
				KubeClient:   fakeClient,
				KubeNodeName: "test-node",
				NodeHasSynced: func() bool {
					return false
				},
			}}
			factory := &framework.EventQueueFactory{Queues: map[string]*framework.EventQueue{}}
			queue := factory.EventQueue(string(framework.NodeMonitorEventName)).GetQueue()
			m := &monitor{
				eventQueueFactory:       factory,
				Configuration:           cfg,
				Interface:               tt.policy(cfg, nil, nil),
				highUsageCountByResName: tt.highUsageCountByResName,
				lowWatermark:            map[v1.ResourceName]int{v1.ResourceCPU: 30, v1.ResourceMemory: 30},
				highWatermark:           map[v1.ResourceName]int{v1.ResourceCPU: 50, v1.ResourceMemory: 50},
				getNodeFunc:             tt.getNodeFunc,
				getPodsFunc:             tt.getPodsFunc,
				usageGetter:             tt.usageGetter,
			}
			m.detectEviction()
			assert.Equalf(t, tt.expectedLen, queue.Len(), "detect()")
			if queue.Len() != 0 {
				key, shutdown := queue.Get()
				if shutdown {
					t.Errorf("Unexpected: queue is shutdown")
				}
				event, ok := key.(framework.NodeMonitorEvent)
				if !ok {
					t.Errorf("Invalid event: %v", key)
				}
				assert.Equalf(t, tt.expectedRes, event.Resource, "detect()")
			}
			if tt.expectedNode != nil {
				node, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), "test-node", metav1.GetOptions{})
				if err != nil {
					t.Errorf("Failed to get node, err: %v", err)
				}
				assert.Equalf(t, tt.expectedNode(), node, "detect()")
			}
		})
	}
}

func Test_monitor_detectCPUQuota(t *testing.T) {
	tests := []struct {
		name                   string
		cpuThrottlingThreshold int
		cpuJitterLimitPercent  int
		cpuRecoverLimitPercent int
		initCPUQuotaMilli      int64
		cpuUsageMilli          int64
		expectedEventCount     int
		expectedQuotaMilli     int64
	}{
		{
			name:                   "emit quota on first sample",
			cpuThrottlingThreshold: 80,
			cpuJitterLimitPercent:  1,
			cpuRecoverLimitPercent: 10,
			initCPUQuotaMilli:      -1,
			cpuUsageMilli:          200,
			expectedEventCount:     1,
			expectedQuotaMilli:     600,
		},
		{
			name:                   "skip emit within jitter limit",
			cpuThrottlingThreshold: 80,
			cpuJitterLimitPercent:  10,
			cpuRecoverLimitPercent: 10,
			initCPUQuotaMilli:      600,
			cpuUsageMilli:          210,
			expectedEventCount:     0,
		},
		{
			name:                   "quota floored at zero",
			cpuThrottlingThreshold: 80,
			cpuJitterLimitPercent:  1,
			cpuRecoverLimitPercent: 10,
			initCPUQuotaMilli:      -1,
			cpuUsageMilli:          900,
			expectedEventCount:     1,
			expectedQuotaMilli:     0,
		},
		{
			name:                   "limit quota recovery increase",
			cpuThrottlingThreshold: 80,
			cpuJitterLimitPercent:  1,
			cpuRecoverLimitPercent: 10,
			initCPUQuotaMilli:      400,
			cpuUsageMilli:          100,
			expectedEventCount:     1,
			expectedQuotaMilli:     440,
		},
		{
			name:                   "skip when throttling disabled",
			cpuThrottlingThreshold: 0,
			cpuUsageMilli:          200,
			expectedEventCount:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeNode, err := makeNodeWithAllocatable("1000m")
			assert.NoError(t, err)
			fakeClient := fakeclientset.NewSimpleClientset(fakeNode)
			cfg := &config.Configuration{GenericConfiguration: &config.VolcanoAgentConfiguration{
				KubeClient:   fakeClient,
				KubeNodeName: "test-node",
				NodeHasSynced: func() bool {
					return false
				},
			}}
			factory := &framework.EventQueueFactory{Queues: map[string]*framework.EventQueue{}}
			queue := factory.EventQueue(string(framework.NodeCPUThrottleEventName)).GetQueue()

			m := &monitor{
				Configuration:          cfg,
				eventQueueFactory:      factory,
				getNodeFunc:            func() (*v1.Node, error) { return fakeNode, nil },
				usageGetter:            resourceusage.NewFakeResourceGetter(tt.cpuUsageMilli, 0, 0, 0),
				cpuThrottlingThreshold: tt.cpuThrottlingThreshold,
				cpuJitterLimitPercent:  tt.cpuJitterLimitPercent,
				cpuRecoverLimitPercent: tt.cpuRecoverLimitPercent,
				lastCPUQuotaMilli:      tt.initCPUQuotaMilli,
			}

			m.detectCPUQuota()

			assert.Equal(t, tt.expectedEventCount, queue.Len(), "unexpected event count")
			if tt.expectedEventCount > 0 {
				key, shutdown := queue.Get()
				assert.False(t, shutdown, "queue should not be shutdown")

				event, ok := key.(framework.NodeCPUThrottleEvent)
				assert.True(t, ok, "event should be NodeCPUThrottleEvent")
				assert.Equal(t, v1.ResourceCPU, event.Resource, "unexpected event resource")
				assert.Equal(t, tt.expectedQuotaMilli, event.CPUQuotaMilli, "unexpected event quota")
				assert.True(t, time.Since(event.TimeStamp) < time.Second, "event timestamp should be recent")
			}
		})
	}
}

func TestMonitorWithdrawsQuotaWhenCPUUsageUnavailable(t *testing.T) {
	for _, tt := range []struct {
		name          string
		previousQuota int64
		wantEvents    int
	}{
		{"first sample missing", -1, 1},
		{"positive quota becomes unsafe", 500, 1},
		{"already at minimum", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node, _ := makeNodeWithAllocatable("1000m")
			factory := &framework.EventQueueFactory{Queues: map[string]*framework.EventQueue{}}
			m := &monitor{
				eventQueueFactory:      factory,
				getNodeFunc:            func() (*v1.Node, error) { return node, nil },
				usageGetter:            partialUsageGetter{value: resourceusage.Resource{v1.ResourceMemory: 100}},
				cpuThrottlingThreshold: 80,
				lastCPUQuotaMilli:      tt.previousQuota,
			}
			m.detectCPUQuota()
			q := factory.EventQueue(string(framework.NodeCPUThrottleEventName)).GetQueue()
			if got := q.Len(); got != tt.wantEvents {
				t.Fatalf("CPU quota events = %d; want %d", got, tt.wantEvents)
			}
			if tt.wantEvents != 0 {
				item, _ := q.Get()
				if got := item.(framework.NodeCPUThrottleEvent).CPUQuotaMilli; got != 0 {
					t.Fatalf("CPU quota = %d; want 0", got)
				}
			}
		})
	}
}

func TestMonitorPreservesPressureOnMissingSample(t *testing.T) {
	node, _ := makeNode()
	m := &monitor{
		getNodeFunc:             func() (*v1.Node, error) { return node, nil },
		usageGetter:             partialUsageGetter{percentage: resourceusage.Resource{v1.ResourceMemory: 20}},
		highUsageCountByResName: map[v1.ResourceName]int{v1.ResourceCPU: highUsageCountLimit - 1},
		highWatermark:           map[v1.ResourceName]int{v1.ResourceCPU: 80, v1.ResourceMemory: 80},
	}
	m.utilizationMonitoring()
	if got := m.highUsageCountByResName[v1.ResourceCPU]; got != highUsageCountLimit-1 {
		t.Fatalf("CPU pressure count after missing sample = %d", got)
	}
	m.usageGetter = partialUsageGetter{percentage: resourceusage.Resource{v1.ResourceCPU: 90, v1.ResourceMemory: 20}}
	m.utilizationMonitoring()
	if got := m.highUsageCountByResName[v1.ResourceCPU]; got != highUsageCountLimit {
		t.Fatalf("CPU pressure count after recovery = %d", got)
	}
	m.getNodeFunc = func() (*v1.Node, error) { return nil, errors.New("node temporarily unavailable") }
	m.utilizationMonitoring()
	if got := m.highUsageCountByResName[v1.ResourceCPU]; got != highUsageCountLimit {
		t.Fatalf("CPU pressure count after node lookup failure = %d", got)
	}
}

func makeNodeWithAllocatable(cpu string) (*v1.Node, error) {
	node, err := makeNode()
	if err != nil {
		return nil, err
	}
	node.Status.Allocatable = v1.ResourceList{
		v1.ResourceCPU: resource.MustParse(cpu),
	}
	return node, nil
}
