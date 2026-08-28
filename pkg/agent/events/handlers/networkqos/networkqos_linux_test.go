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

package networkqos

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	coloapi "volcano.sh/volcano/pkg/agent/config/api"
	"volcano.sh/volcano/pkg/agent/events/framework"
	"volcano.sh/volcano/pkg/config"
)

type fakeNetworkQoSManager struct {
	priorities map[types.UID]uint32
	removed    []types.UID
}

func (m *fakeNetworkQoSManager) Init() error                                { return nil }
func (m *fakeNetworkQoSManager) HealthCheck() error                         { return nil }
func (m *fakeNetworkQoSManager) EnableNetworkQoS(*coloapi.NetworkQos) error { return nil }
func (m *fakeNetworkQoSManager) DisableNetworkQoS() error                   { return nil }
func (m *fakeNetworkQoSManager) Close() error                               { return nil }
func (m *fakeNetworkQoSManager) SetPodPriority(podUID types.UID, _ corev1.PodQOSClass, priority uint32) error {
	m.priorities[podUID] = priority
	return nil
}
func (m *fakeNetworkQoSManager) RemovePodPriority(podUID types.UID) error {
	m.removed = append(m.removed, podUID)
	delete(m.priorities, podUID)
	return nil
}

func TestNeworkQoSHandle_Handle(t *testing.T) {
	testCases := []struct {
		name             string
		recorder         record.EventRecorder
		event            framework.PodEvent
		expectedErr      bool
		expectedQoSLevel string
	}{
		{
			name: "Burstable pod event",
			event: framework.PodEvent{
				UID:      "00000000-1111-2222-3333-000000000001",
				QoSLevel: -1,
				QoSClass: "Burstable",
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test1",
						Namespace: "default",
					},
				},
			},
			expectedErr:      false,
			expectedQoSLevel: "4294967295",
		},

		{
			name: "Guaranteed pod event",
			event: framework.PodEvent{
				UID:      "00000000-1111-2222-3333-000000000002",
				QoSLevel: -1,
				QoSClass: "Guaranteed",
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test2",
						Namespace: "default",
					},
				},
			},
			expectedErr:      false,
			expectedQoSLevel: "4294967295",
		},

		{
			name: "BestEffort pod event",
			event: framework.PodEvent{
				UID:      "00000000-1111-2222-3333-000000000003",
				QoSLevel: -1,
				QoSClass: "BestEffort",
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test3",
						Namespace: "default",
					},
				},
			},
			expectedErr:      false,
			expectedQoSLevel: "4294967295",
		},
	}

	for _, tc := range testCases {
		fakeClient := fake.NewSimpleClientset(tc.event.Pod)
		informerFactory := informers.NewSharedInformerFactory(fakeClient, 0)
		informerFactory.Core().V1().Pods().Informer()
		informerFactory.Start(context.TODO().Done())
		if !cache.WaitForNamedCacheSync("", context.TODO().Done(), informerFactory.Core().V1().Pods().Informer().HasSynced) {
			t.Fatalf("%s: failed to sync pod informer", tc.name)
		}

		cfg := &config.Configuration{
			InformerFactory: &config.InformerFactory{
				K8SInformerFactory: informerFactory,
			},
			GenericConfiguration: &config.VolcanoAgentConfiguration{
				KubeClient: fakeClient,
				Recorder:   record.NewFakeRecorder(100),
			},
		}

		manager := &fakeNetworkQoSManager{priorities: make(map[types.UID]uint32)}
		h := newNetworkQoSHandle(cfg, manager)
		handleErr := h.Handle(tc.event)
		assert.Equal(t, tc.expectedErr, handleErr != nil, tc.name)
		assert.Equal(t, tc.expectedQoSLevel, fmt.Sprint(manager.priorities[tc.event.UID]), tc.name)
	}
}

func TestNetworkQoSHandleDelete(t *testing.T) {
	manager := &fakeNetworkQoSManager{priorities: map[types.UID]uint32{
		"pod-1": 1,
		"pod-2": 1,
	}}
	handle := &NetworkQoSHandle{networkqosMgr: manager}

	handle.handleDelete(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod-1"}})
	handle.handleDelete(cache.DeletedFinalStateUnknown{
		Obj: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod-2"}},
	})

	assert.Equal(t, []types.UID{"pod-1", "pod-2"}, manager.removed)
	assert.Empty(t, manager.priorities)
}

func TestNetworkQoSHandleSyncPodPriorities(t *testing.T) {
	ready := []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "offline", Namespace: "default", UID: "offline-uid",
				Annotations: map[string]string{"volcano.sh/qos-level": "BE"},
			},
			Spec:   corev1.PodSpec{NodeName: "node-1"},
			Status: corev1.PodStatus{QOSClass: corev1.PodQOSBestEffort, Conditions: ready},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "host-network", Namespace: "default", UID: "host-uid"},
			Spec:       corev1.PodSpec{NodeName: "node-1", HostNetwork: true},
			Status:     corev1.PodStatus{QOSClass: corev1.PodQOSBestEffort, Conditions: ready},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "other-node", Namespace: "default", UID: "other-uid"},
			Spec:       corev1.PodSpec{NodeName: "node-2"},
			Status:     corev1.PodStatus{QOSClass: corev1.PodQOSBestEffort, Conditions: ready},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "not-ready", Namespace: "default", UID: "not-ready-uid"},
			Spec:       corev1.PodSpec{NodeName: "node-1"},
			Status:     corev1.PodStatus{QOSClass: corev1.PodQOSBestEffort},
		},
	}
	objects := make([]runtime.Object, 0, len(pods))
	for i := range pods {
		objects = append(objects, &pods[i])
	}
	fakeClient := fake.NewSimpleClientset(objects...)
	informerFactory := informers.NewSharedInformerFactory(fakeClient, 0)
	informerFactory.Core().V1().Pods().Informer()
	stop := make(chan struct{})
	defer close(stop)
	informerFactory.Start(stop)
	assert.True(t, cache.WaitForNamedCacheSync(t.Name(), stop, informerFactory.Core().V1().Pods().Informer().HasSynced))

	cfg := &config.Configuration{
		InformerFactory: &config.InformerFactory{K8SInformerFactory: informerFactory},
		GenericConfiguration: &config.VolcanoAgentConfiguration{
			KubeClient:   fakeClient,
			KubeNodeName: "node-1",
			Recorder:     record.NewFakeRecorder(100),
		},
	}
	manager := &fakeNetworkQoSManager{priorities: make(map[types.UID]uint32)}
	handle := newNetworkQoSHandle(cfg, manager)

	assert.NoError(t, handle.syncPodPriorities())
	assert.Equal(t, map[types.UID]uint32{"offline-uid": ^uint32(0)}, manager.priorities)
}
