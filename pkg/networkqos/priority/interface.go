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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"volcano.sh/volcano/pkg/agent/utils/cgroup"
)

// Manager transparently applies the online/offline network priority using the
// mechanism provided by the node's cgroup version.
type Manager interface {
	Init() error
	SetPodPriority(podUID types.UID, qosClass corev1.PodQOSClass, priority uint32) error
	RemovePodPriority(podUID types.UID) error
	HealthCheck() error
	Close() error
}

type cgroupPathProvider interface {
	GetPodCgroupPath(qos corev1.PodQOSClass, subsystem cgroup.CgroupSubsystem, podUID types.UID) (string, error)
	GetCgroupVersion() string
}

// NewManager returns the implementation matching the detected cgroup version.
func NewManager(cgroupMgr cgroup.CgroupManager, cgroupV2ProgramPath string) Manager {
	if cgroupMgr.GetCgroupVersion() == cgroup.CgroupV2 {
		return newCgroupSKBV2Manager(cgroupMgr, cgroupV2ProgramPath)
	}
	return newNetCLSV1Manager(cgroupMgr)
}
