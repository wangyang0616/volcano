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

package priority

import (
	"fmt"
	"path/filepath"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	agentutils "volcano.sh/volcano/pkg/agent/utils"
	"volcano.sh/volcano/pkg/agent/utils/cgroup"
)

type netCLSV1Manager struct {
	cgroupMgr cgroupPathProvider
}

func newNetCLSV1Manager(cgroupMgr cgroupPathProvider) Manager {
	return &netCLSV1Manager{cgroupMgr: cgroupMgr}
}

func (m *netCLSV1Manager) Init() error {
	return nil
}

func (m *netCLSV1Manager) Enable() error {
	return nil
}

func (m *netCLSV1Manager) SetPodPriority(podUID types.UID, qosClass corev1.PodQOSClass, priority uint32) error {
	cgroupPath, err := m.cgroupMgr.GetPodCgroupPath(qosClass, cgroup.CgroupNetCLSSubsystem, podUID)
	if err != nil {
		return fmt.Errorf("failed to get pod cgroup path %s: %w", podUID, err)
	}

	priorityFile := filepath.Join(cgroupPath, cgroup.NetCLSFileName)
	value := []byte(strconv.FormatUint(uint64(priority), 10))
	if err := agentutils.UpdatePodCgroup(priorityFile, value); err != nil {
		return fmt.Errorf("failed to set network priority for pod %s: %w", podUID, err)
	}
	return nil
}

func (m *netCLSV1Manager) RemovePodPriority(types.UID) error {
	// The cgroup directory and its net_cls state are removed with the pod.
	return nil
}

func (m *netCLSV1Manager) HealthCheck() error {
	return nil
}

func (m *netCLSV1Manager) Close() error {
	return nil
}
