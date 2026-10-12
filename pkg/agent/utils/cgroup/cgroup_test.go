/*
Copyright 2025 The Volcano Authors.

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

package cgroup

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
)

func TestDetectCgroupDriverAtHostRoot(t *testing.T) {
	for _, tt := range []struct {
		name string
		dirs []string
		want string
	}{
		{"v2 systemd", []string{"kubepods.slice"}, CgroupDriverSystemd},
		{"v2 cgroupfs on systemd host", []string{"kubepods", "system.slice"}, CgroupDriverCgroupfs},
		{"v1 systemd", []string{"cpu/kubepods.slice", "memory/kubepods.slice"}, CgroupDriverSystemd},
		{"v1 cgroupfs", []string{"cpu/kubepods", "systemd"}, CgroupDriverCgroupfs},
		{"v1 combined controllers", []string{"cpu,cpuacct/kubepods.slice"}, CgroupDriverSystemd},
		{"no hierarchy", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range tt.dirs {
				if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
					t.Fatal(err)
				}
			}
			got, err := detectCgroupDriver(root)
			if got != tt.want || (err != nil) != (tt.want == "") {
				t.Fatalf("detectCgroupDriver(%s) = %q, %v; want %q", root, got, err, tt.want)
			}
		})
	}
}

func TestNewCgroupManagerExplicitDriver(t *testing.T) {
	t.Setenv("VOLCANO_TEST_CGROUP_VERSION", CgroupV2)
	t.Setenv("CGROUP_DRIVER", CgroupDriverCgroupfs)
	root := t.TempDir()
	for _, tt := range []struct {
		driver, want string
	}{
		{"", filepath.Join(root, "kubepods")},
		{CgroupDriverSystemd, filepath.Join(root, "kubepods.slice")},
	} {
		mgr := NewCgroupManager(tt.driver, root, "")
		got, err := mgr.GetRootCgroupPath(CgroupCpuAcctSubsystem)
		if err != nil || got != tt.want {
			t.Fatalf("root path = %q, %v; want %q", got, err, tt.want)
		}
	}
}

func TestCPUAccountingPaths(t *testing.T) {
	for _, controller := range []string{"cpuacct", "cpu,cpuacct", "cpuacct,cpu", "cpu"} {
		t.Run(controller, func(t *testing.T) {
			t.Setenv("VOLCANO_TEST_CGROUP_VERSION", CgroupV1)
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, controller), 0755); err != nil {
				t.Fatal(err)
			}
			// cpu must not take precedence over an independent accounting mount.
			if err := os.MkdirAll(filepath.Join(root, "cpu"), 0755); err != nil {
				t.Fatal(err)
			}
			mgr := NewCgroupManager(CgroupDriverCgroupfs, root, "")
			for _, qos := range []corev1.PodQOSClass{corev1.PodQOSGuaranteed, corev1.PodQOSBurstable, corev1.PodQOSBestEffort} {
				suffix := ""
				switch qos {
				case corev1.PodQOSBurstable:
					suffix = "burstable"
				case corev1.PodQOSBestEffort:
					suffix = "besteffort"
				}
				got, err := mgr.GetQoSCgroupPath(qos, CgroupCpuAcctSubsystem)
				want := filepath.Join(root, controller, "kubepods", suffix)
				if err != nil || got != want {
					t.Fatalf("QoS %s path = %q, %v; want %q", qos, got, err, want)
				}
			}
			got, err := mgr.GetRootCgroupPath(CgroupCpuSubsystem)
			want := filepath.Join(root, "cpu", "kubepods")
			if err != nil || got != want {
				t.Fatalf("CPU control path = %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestCombinedV1CPUControllerPaths(t *testing.T) {
	t.Setenv("VOLCANO_TEST_CGROUP_VERSION", CgroupV1)
	root := t.TempDir()
	combined := filepath.Join(root, "cpu,cpuacct")
	if err := os.MkdirAll(combined, 0755); err != nil {
		t.Fatal(err)
	}
	// A stale cpuacct alias must not hide the actual combined mount.
	if err := os.MkdirAll(filepath.Join(root, "cpuacct"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, fileName := range []string{CPUUsageFile, CPUQuotaTotalFile} {
		if err := os.WriteFile(filepath.Join(combined, fileName), []byte("0"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mgr := NewCgroupManager(CgroupDriverCgroupfs, root, "")
	for _, subsystem := range []CgroupSubsystem{CgroupCpuSubsystem, CgroupCpuAcctSubsystem} {
		got, err := mgr.GetQoSCgroupPath(corev1.PodQOSBestEffort, subsystem)
		want := filepath.Join(combined, "kubepods", "besteffort")
		if err != nil || got != want {
			t.Fatalf("%s path = %q, %v; want %q", subsystem, got, err, want)
		}
	}
}

func TestCgroupPathsWithKubeRoot(t *testing.T) {
	for _, tt := range []struct {
		version, driver, want string
	}{
		{CgroupV1, CgroupDriverCgroupfs, "cpuacct/workloads/kubepods/burstable/pod123-456"},
		{CgroupV2, CgroupDriverCgroupfs, "workloads/kubepods/burstable/pod123-456"},
		{CgroupV1, CgroupDriverSystemd, "cpuacct/workloads.slice/workloads-kubepods.slice/workloads-kubepods-burstable.slice/workloads-kubepods-burstable-pod123_456.slice"},
		{CgroupV2, CgroupDriverSystemd, "workloads.slice/workloads-kubepods.slice/workloads-kubepods-burstable.slice/workloads-kubepods-burstable-pod123_456.slice"},
	} {
		t.Run(tt.version+"/"+tt.driver, func(t *testing.T) {
			t.Setenv("VOLCANO_TEST_CGROUP_VERSION", tt.version)
			root := t.TempDir()
			mgr := NewCgroupManager(tt.driver, root, "workloads")
			got, err := mgr.GetPodCgroupPath(corev1.PodQOSBurstable, CgroupCpuAcctSubsystem, "123-456")
			if want := filepath.Join(root, tt.want); err != nil || got != want {
				t.Fatalf("pod path = %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestCgroupVersionDetectionPerMount(t *testing.T) {
	var st unix.Statfs_t
	if err := unix.Statfs("/sys/fs/cgroup", &st); err != nil || st.Type != unix.CGROUP2_SUPER_MAGIC {
		t.Skip("requires a mounted cgroup v2 filesystem")
	}
	t.Setenv("VOLCANO_TEST_CGROUP_VERSION", "")
	root := t.TempDir()
	// An unsuccessful probe or a non-cgroup mount must not poison later
	// detection of the host's v2 mount, including through a different path.
	if IsCgroupsV2(filepath.Join(root, "missing")) {
		t.Fatal("missing mount reported as v2")
	}
	if got := DetectCgroupVersion("/sys/fs/cgroup"); got != CgroupV2 {
		t.Fatalf("host version = %q, want v2", got)
	}
	if IsCgroupsV2(root) {
		t.Fatal("temporary directory reported as v2")
	}
	alias := filepath.Join(root, "host-cgroup")
	if err := os.Symlink("/sys/fs/cgroup", alias); err != nil {
		t.Fatal(err)
	}
	if got := DetectCgroupVersion(alias); got != CgroupV2 {
		t.Fatalf("host mount alias version = %q, want v2", got)
	}
}

func TestValidateCgroupRoot(t *testing.T) {
	root := t.TempDir()
	if err := ValidateCgroupRoot(root); err == nil {
		t.Fatal("ordinary directory accepted as cgroup root")
	}
	if err := ValidateCgroupRoot(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing directory accepted as cgroup root")
	}
	var st unix.Statfs_t
	if err := unix.Statfs("/sys/fs/cgroup", &st); err == nil && st.Type == unix.CGROUP2_SUPER_MAGIC {
		if err := ValidateCgroupRoot("/sys/fs/cgroup"); err != nil {
			t.Fatalf("mounted cgroup v2 root rejected: %v", err)
		}
	}
}

func TestBuildContainerCgroupName(t *testing.T) {
	tests := []struct {
		name         string
		cgroupDriver string
		containerID  string
		expected     string
	}{
		{
			name:         "cgroupfs with containerd",
			cgroupDriver: CgroupDriverCgroupfs,
			containerID:  "containerd://abc123",
			expected:     "abc123",
		},
		{
			name:         "cgroupfs with docker",
			cgroupDriver: CgroupDriverCgroupfs,
			containerID:  "docker://xyz789",
			expected:     "xyz789",
		},
		{
			name:         "cgroupfs with pure ID",
			cgroupDriver: CgroupDriverCgroupfs,
			containerID:  "abc123",
			expected:     "abc123",
		},
		{
			name:         "systemd with containerd",
			cgroupDriver: CgroupDriverSystemd,
			containerID:  "containerd://abc123",
			expected:     "cri-containerd-abc123.scope",
		},
		{
			name:         "systemd with docker",
			cgroupDriver: CgroupDriverSystemd,
			containerID:  "docker://xyz789",
			expected:     "docker-xyz789.scope",
		},
		{
			name:         "systemd with cri-o",
			cgroupDriver: CgroupDriverSystemd,
			containerID:  "cri-o://def456",
			expected:     "crio-def456.scope",
		},
		{
			name:         "systemd with pure ID",
			cgroupDriver: CgroupDriverSystemd,
			containerID:  "abc123",
			expected:     "abc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &CgroupManagerImpl{
				cgroupDriver: tt.cgroupDriver,
			}
			got := mgr.BuildContainerCgroupName(tt.containerID)
			if got != tt.expected {
				t.Errorf("BuildContainerCgroupName() = %v, want %v", got, tt.expected)
			}
		})
	}
}
