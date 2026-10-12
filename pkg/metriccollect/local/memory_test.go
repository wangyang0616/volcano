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

package local

import (
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"volcano.sh/volcano/pkg/agent/utils/cgroup"
)

func TestGetMemoryUsage(t *testing.T) {
	for _, tt := range []struct {
		name, version string
		files         map[string]string
		want          int64
		wantErr       bool
	}{
		{
			name: "v1 hierarchical counters", version: cgroup.CgroupV1,
			files: map[string]string{"memory.stat": "cache 10\nrss 20\ntotal_cache 100\ntotal_rss 200\ntotal_swap 50\ntotal_mapped_file 30\n"},
			want:  350,
		},
		{
			name: "v1 no swap and extra whitespace", version: cgroup.CgroupV1,
			files: map[string]string{"memory.stat": "total_cache\t100\ntotal_rss   200\n"}, want: 300,
		},
		{
			name: "v2 totals without overlapping stat fields", version: cgroup.CgroupV2,
			files: map[string]string{
				"memory.current": "350\n",
				"memory.stat":    "anon 200\nfile 100\nkernel 50\nshmem 60\nslab 20\npagetables 10\nkernel_stack 5\n",
			},
			want: 350,
		},
		{
			name: "v2 needs only memory.current", version: cgroup.CgroupV2,
			files: map[string]string{"memory.current": "8589934592\n"}, want: 8589934592,
		},
		{
			name: "v2 zero usage", version: cgroup.CgroupV2,
			files: map[string]string{"memory.current": "0\n"},
		},
		{
			name: "v2 missing memory.current", version: cgroup.CgroupV2,
			files: map[string]string{"memory.stat": "anon 200\nfile 100\n"}, wantErr: true,
		},
		{
			name: "v2 malformed current", version: cgroup.CgroupV2,
			files: map[string]string{"memory.current": "invalid\n"}, wantErr: true,
		},
		{
			name: "v1 malformed counter", version: cgroup.CgroupV1,
			files: map[string]string{"memory.stat": "total_rss invalid\ntotal_cache 100\n"}, wantErr: true,
		},
		{
			name: "v1 missing counter value", version: cgroup.CgroupV1,
			files: map[string]string{"memory.stat": "total_rss\ntotal_cache 100\n"}, wantErr: true,
		},
		{
			name: "v2 stats misidentified as v1", version: cgroup.CgroupV1,
			files: map[string]string{"memory.stat": "anon 200\nfile 100\n"}, wantErr: true,
		},
		{name: "v1 missing file", version: cgroup.CgroupV1, wantErr: true},
		{name: "unknown version", version: "", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for name, data := range tt.files {
				writeMetricFile(t, filepath.Join(root, name), data)
			}
			got, err := getMemoryUsage(root, tt.version)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("getMemoryUsage() = %d, %v; want %d, error=%t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestMemoryResourceCollectorCgroupLayouts(t *testing.T) {
	for _, layout := range []struct {
		version, driver, root, file, contents string
	}{
		{cgroup.CgroupV1, "cgroupfs", "memory/kubepods", "memory.stat", "total_rss 200\ntotal_cache 100\ntotal_swap 50\n"},
		{cgroup.CgroupV1, "systemd", "memory/kubepods.slice", "memory.stat", "total_rss 200\ntotal_cache 100\ntotal_swap 50\n"},
		{cgroup.CgroupV2, "cgroupfs", "kubepods", "memory.current", "350\n"},
		{cgroup.CgroupV2, "systemd", "kubepods.slice", "memory.current", "350\n"},
	} {
		t.Run(layout.version+"/"+layout.driver, func(t *testing.T) {
			t.Setenv("VOLCANO_TEST_CGROUP_VERSION", layout.version)
			root := t.TempDir()
			writeMetricFile(t, filepath.Join(root, layout.root, layout.file), layout.contents)
			procPath := filepath.Join(root, "meminfo")
			writeMetricFile(t, procPath, "MemTotal: 1000 kB\nMemAvailable: 600 kB\n")
			t.Setenv(memInfoPathEnv, procPath)
			collector := &MemoryResourceCollector{cgroupManager: cgroup.NewCgroupManager(layout.driver, root, "")}
			for _, system := range []bool{false, true} {
				metrics, err := collector.CollectLocalMetrics(&LocalMetricInfo{IncludeSystemUsed: system}, time.Time{}, metav1.Duration{})
				if err != nil {
					t.Fatal(err)
				}
				want := float64(350)
				if system {
					want = 400 * 1024
				}
				if len(metrics) != 1 || len(metrics[0].Samples) != 1 || metrics[0].Samples[0].Value != want {
					t.Fatalf("metrics = %v, want %v bytes", metrics, want)
				}
			}
		})
	}
}

func TestMemoryCollectorSystemUsageWithoutPodMemory(t *testing.T) {
	t.Setenv("VOLCANO_TEST_CGROUP_VERSION", cgroup.CgroupV2)
	root := t.TempDir()
	procPath := filepath.Join(root, "meminfo")
	writeMetricFile(t, procPath, "MemTotal: 1000 kB\nMemAvailable: 600 kB\n")
	t.Setenv(memInfoPathEnv, procPath)
	collector := &MemoryResourceCollector{cgroupManager: cgroup.NewCgroupManager(cgroup.CgroupDriverCgroupfs, root, "")}
	metrics, err := collector.CollectLocalMetrics(&LocalMetricInfo{IncludeSystemUsed: true}, time.Time{}, metav1.Duration{})
	if err != nil {
		t.Fatal(err)
	}
	if got := metrics[0].Samples[0].Value; got != 400*1024 {
		t.Fatalf("system memory usage = %v; want %d", got, 400*1024)
	}
}

func TestNodeMemoryUsageRejectsIncompleteMeminfo(t *testing.T) {
	for _, contents := range []string{"MemTotal: 1000 kB\n", "MemAvailable: 600 kB\n", "MemTotal: \nMemAvailable: 600 kB\n", "MemTotal: 1000 kB\nMemAvailable: 1100 kB\n"} {
		t.Run(contents, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meminfo")
			writeMetricFile(t, path, contents)
			t.Setenv(memInfoPathEnv, path)
			if _, err := nodeMemoryUsage(); err == nil {
				t.Fatal("expected an error for incomplete or inconsistent meminfo")
			}
		})
	}
}
