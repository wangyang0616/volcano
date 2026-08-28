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
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"volcano.sh/volcano/pkg/agent/utils/cgroup"
)

type fakeCgroupPathProvider struct {
	path      string
	version   string
	subsystem cgroup.CgroupSubsystem
}

func (f *fakeCgroupPathProvider) GetPodCgroupPath(_ corev1.PodQOSClass, subsystem cgroup.CgroupSubsystem, _ types.UID) (string, error) {
	f.subsystem = subsystem
	return f.path, nil
}

func (f *fakeCgroupPathProvider) GetCgroupVersion() string {
	return f.version
}

func TestNetCLSV1ManagerSetPodPriority(t *testing.T) {
	cgroupPath := t.TempDir()
	priorityFile := filepath.Join(cgroupPath, cgroup.NetCLSFileName)
	if err := os.WriteFile(priorityFile, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(cgroupPath, "container")
	if err := os.Mkdir(childPath, 0o750); err != nil {
		t.Fatal(err)
	}
	childPriorityFile := filepath.Join(childPath, cgroup.NetCLSFileName)
	if err := os.WriteFile(childPriorityFile, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &fakeCgroupPathProvider{path: cgroupPath, version: cgroup.CgroupV1}
	manager := newNetCLSV1Manager(provider)
	if err := manager.SetPodPriority("pod-1", corev1.PodQOSBestEffort, ^uint32(0)); err != nil {
		t.Fatalf("SetPodPriority() error = %v", err)
	}

	if provider.subsystem != cgroup.CgroupNetCLSSubsystem {
		t.Fatalf("subsystem = %q, want %q", provider.subsystem, cgroup.CgroupNetCLSSubsystem)
	}
	for _, file := range []string{priorityFile, childPriorityFile} {
		got, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "4294967295" {
			t.Fatalf("priority in %s = %q, want %q", file, got, "4294967295")
		}
	}
}
