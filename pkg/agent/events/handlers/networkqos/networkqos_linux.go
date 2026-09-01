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
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/agent/apis/extension"
	"volcano.sh/volcano/pkg/agent/config/api"
	"volcano.sh/volcano/pkg/agent/events/framework"
	"volcano.sh/volcano/pkg/agent/events/handlers"
	"volcano.sh/volcano/pkg/agent/events/handlers/base"
	"volcano.sh/volcano/pkg/agent/features"
	"volcano.sh/volcano/pkg/agent/utils/cgroup"
	"volcano.sh/volcano/pkg/config"
	"volcano.sh/volcano/pkg/metriccollect"
	"volcano.sh/volcano/pkg/networkqos"
)

func init() {
	handlers.RegisterEventHandleFunc(string(framework.PodEventName), NewNetworkQoSHandle)
}

type NetworkQoSHandle struct {
	*base.BaseHandle
	networkqosMgr networkqos.NetworkQoSManager
	poLister      listersv1.PodLister
	recorder      record.EventRecorder
}

func NewNetworkQoSHandle(config *config.Configuration, mgr *metriccollect.MetricCollectorManager, cgroupMgr cgroup.CgroupManager) framework.Handle {
	return newNetworkQoSHandle(config, networkqos.GetNetworkQoSManager(config, cgroupMgr))
}

func newNetworkQoSHandle(config *config.Configuration, networkQoSMgr networkqos.NetworkQoSManager) *NetworkQoSHandle {
	handle := &NetworkQoSHandle{
		BaseHandle: &base.BaseHandle{
			Name:   string(features.NetworkQoSFeature),
			Config: config,
		},
		networkqosMgr: networkQoSMgr,
		poLister:      config.InformerFactory.K8SInformerFactory.Core().V1().Pods().Lister(),
		recorder:      config.GenericConfiguration.Recorder,
	}
	_, err := config.InformerFactory.K8SInformerFactory.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: handle.handleDelete,
	})
	if err != nil {
		klog.ErrorS(err, "Failed to register pod deletion handler for Network QoS")
	}
	return handle
}

func (h *NetworkQoSHandle) Handle(event interface{}) error {
	podEvent, ok := event.(framework.PodEvent)
	if !ok {
		return fmt.Errorf("illegal pod event: %v", event)
	}

	pod, err := h.poLister.Pods(podEvent.Pod.Namespace).Get(podEvent.Pod.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.V(4).InfoS("pod does not existed, skipped handling network qos", "namespace", podEvent.Pod.Namespace, "name", podEvent.Pod.Name)
			return nil
		}
		return err
	}
	// hostNetwork Pods do not pass through the chained CNI data path, so a
	// cgroup priority attachment cannot affect their traffic. Also discard a
	// stale event when a Pod name has already been reused with a new UID.
	if pod.UID != podEvent.UID || pod.Spec.HostNetwork || pod.DeletionTimestamp != nil {
		return h.removePodPriority(podEvent.UID)
	}

	_, ingressExisted := pod.Annotations["kubernetes.io/ingress-bandwidth"]
	_, egressExisted := pod.Annotations["kubernetes.io/egress-bandwidth"]
	if ingressExisted || egressExisted {
		if err := h.removePodPriority(podEvent.UID); err != nil {
			return err
		}
		h.recorder.Event(pod, corev1.EventTypeWarning, "NetworkQoSSkipped",
			fmt.Sprintf("Colocation Network QoS is not set, because it already has an Ingress-Bandwidth/Egress-Bandwidth"+
				" network rate limit(with annotation key kubernetes.io/ingress-bandwidth or kubernetes.io/egress-bandwidth )"))
		return nil
	}

	uintQoSLevel := uint32(extension.NormalizeQosLevel(podEvent.QoSLevel))
	err = h.networkqosMgr.SetPodPriority(podEvent.UID, podEvent.QoSClass, uintQoSLevel)
	if os.IsNotExist(err) {
		klog.InfoS("Pod cgroup not found while setting network priority", "podUID", podEvent.UID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to set network priority for pod %s: %w", podEvent.UID, err)
	}
	// Delete notifications are delivered independently from the queued Pod
	// event. Reconcile once after attaching so a delete, name reuse, or switch
	// to an ineligible state that won the race cannot leave an orphaned link.
	removed, err := h.removePriorityIfPodIsStale(podEvent)
	if err != nil {
		return err
	}
	if removed {
		return nil
	}
	klog.InfoS("Successfully set pod network priority", "qosLevel", uintQoSLevel, "podUID", podEvent.UID)
	return nil
}

func (h *NetworkQoSHandle) removePriorityIfPodIsStale(podEvent framework.PodEvent) (bool, error) {
	pod, err := h.poLister.Pods(podEvent.Pod.Namespace).Get(podEvent.Pod.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			return true, h.removePodPriority(podEvent.UID)
		}
		return false, err
	}

	_, ingressExisted := pod.Annotations["kubernetes.io/ingress-bandwidth"]
	_, egressExisted := pod.Annotations["kubernetes.io/egress-bandwidth"]
	if pod.UID != podEvent.UID || pod.Spec.HostNetwork || pod.DeletionTimestamp != nil || ingressExisted || egressExisted {
		return true, h.removePodPriority(podEvent.UID)
	}
	return false, nil
}

func (h *NetworkQoSHandle) removePodPriority(podUID types.UID) error {
	if err := h.networkqosMgr.RemovePodPriority(podUID); err != nil {
		return fmt.Errorf("failed to remove network priority for pod %s: %w", podUID, err)
	}
	return nil
}

func (h *NetworkQoSHandle) handleDelete(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, tombstoneOK := obj.(cache.DeletedFinalStateUnknown)
		if !tombstoneOK {
			klog.ErrorS(nil, "Failed to decode deleted pod for Network QoS", "object", obj)
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			klog.ErrorS(nil, "Deleted object is not a pod for Network QoS", "object", tombstone.Obj)
			return
		}
	}

	if err := h.networkqosMgr.RemovePodPriority(pod.UID); err != nil {
		klog.ErrorS(err, "Failed to remove pod network priority", "podUID", pod.UID)
	}
}

func (h *NetworkQoSHandle) RefreshCfg(cfg *api.ColocationConfig) error {
	wasActive := h.IsActive()
	if err := h.BaseHandle.RefreshCfg(cfg); err != nil {
		return err
	}

	h.Lock.Lock()
	defer h.Lock.Unlock()
	if h.Active {
		klog.InfoS("Start applying enabled NetworkQoS configuration", "wasActive", wasActive)
		err := h.networkqosMgr.EnableNetworkQoS(cfg.NetworkQosConfig)
		if err != nil {
			klog.ErrorS(err, "Failed to enable network qos")
			return err
		}
		klog.InfoS("Successfully enabled NetworkQoS manager", "wasActive", wasActive)
		if !wasActive {
			klog.InfoS("Start syncing existing Pod network priorities after enabling NetworkQoS")
			if err := h.syncPodPriorities(); err != nil {
				return fmt.Errorf("failed to sync pod network priorities after enabling network qos: %w", err)
			}
			klog.InfoS("Finished syncing existing Pod network priorities after enabling NetworkQoS")
		}
		klog.V(5).InfoS("Successfully enable/update network QoS")
		return nil
	}

	err := h.networkqosMgr.DisableNetworkQoS()
	if err != nil {
		klog.ErrorS(err, "Failed to disable network qos")
		return err
	}
	klog.V(5).InfoS("Successfully disable network QoS")
	return nil
}

// syncPodPriorities restores the transient cgroup v2 BPF links when NetworkQoS
// transitions from disabled to enabled. cgroup v1 priorities live in cgroup
// files and the same reconciliation is harmless there.
func (h *NetworkQoSHandle) syncPodPriorities() error {
	pods, err := h.poLister.List(labels.Everything())
	if err != nil {
		return err
	}

	for _, pod := range pods {
		if pod.Spec.NodeName != h.Config.GenericConfiguration.KubeNodeName || pod.Spec.HostNetwork || pod.DeletionTimestamp != nil {
			continue
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}

		podEvent := framework.PodEvent{
			UID:      pod.UID,
			QoSClass: pod.Status.QOSClass,
			QoSLevel: int64(extension.GetQosLevel(pod)),
			Pod:      pod,
		}
		klog.V(2).InfoS("Syncing existing Pod network priority", "pod", klog.KObj(pod), "podUID", pod.UID, "qosClass", pod.Status.QOSClass)
		if err := h.Handle(podEvent); err != nil {
			// A ready Pod can disappear between the informer list and cgroup
			// lookup. Keep the config transition successful; a later Pod event
			// will retry it.
			klog.ErrorS(err, "Failed to sync pod network priority after enabling NetworkQoS", "pod", klog.KObj(pod))
		}
	}
	return nil
}
