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
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"volcano.sh/volcano/pkg/agent/utils/cgroup"
)

func writeMetricFile(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSampleCPUUsages(t *testing.T) {
	for _, version := range []string{cgroup.CgroupV1, cgroup.CgroupV2} {
		t.Run(version, func(t *testing.T) {
			root := t.TempDir()
			name, initial, updated := "cpuacct.usage", "1000000000\n", "2000000000\n"
			if version == cgroup.CgroupV2 {
				name = "cpu.stat"
				initial = "user_usec 600000\nsystem_usec 400000\nusage_usec 1000000\nnr_periods 5\n"
				updated = "user_usec 1200000\nsystem_usec 800000\nusage_usec 2000000\nnr_periods 10\n"
			}
			name = filepath.Join(root, name)
			writeMetricFile(t, name, initial)
			synctest.Test(t, func(t *testing.T) {
				go func() {
					time.Sleep(500 * time.Millisecond)
					writeMetricFile(t, name, updated)
				}()
				got, _, err := sampleCPUUsages([]string{root}, version, false)
				if err != nil || got[root] != 1000 {
					t.Fatalf("sampleCPUUsages() = %v, %v; want 1000 millicores", got, err)
				}
			})
		})
	}
}

func TestCPUResourceCollectorSamplesQoSWithinOneWindow(t *testing.T) {
	t.Setenv("VOLCANO_TEST_CGROUP_VERSION", cgroup.CgroupV2)
	root := t.TempDir()
	rootFile := filepath.Join(root, "kubepods", "cpu.stat")
	bestFile := filepath.Join(root, "kubepods", "besteffort", "cpu.stat")
	writeMetricFile(t, rootFile, "usage_usec 0\n")
	writeMetricFile(t, bestFile, "usage_usec 0\n")
	collector := &CPUResourceCollector{cgroupManager: cgroup.NewCgroupManager(cgroup.CgroupDriverCgroupfs, root, "")}
	synctest.Test(t, func(t *testing.T) {
		go func() {
			time.Sleep(500 * time.Millisecond)
			// All CPU time in this interval belongs to BestEffort.
			writeMetricFile(t, rootFile, "usage_usec 1000000\n")
			writeMetricFile(t, bestFile, "usage_usec 1000000\n")
		}()
		metrics, err := collector.CollectLocalMetrics(&LocalMetricInfo{
			IncludeGuaranteedPods: true, IncludeBestEffortPods: false,
		}, time.Time{}, metav1.Duration{})
		if err != nil {
			t.Fatal(err)
		}
		if got := metrics[0].Samples[0].Value; got != 0 {
			t.Fatalf("non-BestEffort usage = %v; want 0", got)
		}
	})
}

func TestReadCPUUsageErrors(t *testing.T) {
	for _, tt := range []struct {
		name, version, file, contents string
	}{
		{"missing v1 file", cgroup.CgroupV1, "cpu.stat", "usage_usec 1000\n"},
		{"missing v2 file", cgroup.CgroupV2, "cpuacct.usage", "1000000\n"},
		{"invalid v1 counter", cgroup.CgroupV1, "cpuacct.usage", "invalid\n"},
		{"invalid v2 counter", cgroup.CgroupV2, "cpu.stat", "usage_usec invalid\n"},
		{"missing usage_usec", cgroup.CgroupV2, "cpu.stat", "user_usec 1000\n"},
		{"unknown version", "", "cpuacct.usage", "1000000\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeMetricFile(t, filepath.Join(root, tt.file), tt.contents)
			if _, err := readCPUUsage(root, tt.version); err == nil {
				t.Fatal("expected an error instead of a zero usage sample")
			}
		})
	}
}

func TestCPUResourceCollectorCgroupLayouts(t *testing.T) {
	for _, layout := range []struct {
		name, version, driver, root, burstable, bestEffort string
	}{
		{"v1 separate controllers", cgroup.CgroupV1, "cgroupfs", "cpuacct/kubepods", "burstable", "besteffort"},
		{"v1 combined controllers", cgroup.CgroupV1, "systemd", "cpu,cpuacct/kubepods.slice", "kubepods-burstable.slice", "kubepods-besteffort.slice"},
		{"v2 cgroupfs", cgroup.CgroupV2, "cgroupfs", "kubepods", "burstable", "besteffort"},
		{"v2 systemd", cgroup.CgroupV2, "systemd", "kubepods.slice", "kubepods-burstable.slice", "kubepods-besteffort.slice"},
	} {
		t.Run(layout.name, func(t *testing.T) {
			t.Setenv("VOLCANO_TEST_CGROUP_VERSION", layout.version)
			for _, guaranteed := range []bool{false, true} {
				for _, bestEffort := range []bool{false, true} {
					for _, system := range []bool{false, true} {
						t.Run(fmt.Sprintf("guaranteed=%t/besteffort=%t/system=%t", guaranteed, bestEffort, system), func(t *testing.T) {
							root := t.TempDir()
							// Keep the v1 CPU controller separate and without accounting files.
							if layout.version == cgroup.CgroupV1 {
								if err := os.Mkdir(filepath.Join(root, "cpu"), 0755); err != nil {
									t.Fatal(err)
								}
							}
							procPath := filepath.Join(root, "proc.stat")
							t.Setenv(procStatPathEnv, procPath)
							usageFile := "cpuacct.usage"
							if layout.version == cgroup.CgroupV2 {
								usageFile = "cpu.stat"
							}
							counters := map[string]int64{
								filepath.Join(root, layout.root, usageFile):                    600,
								filepath.Join(root, layout.root, layout.burstable, usageFile):  200,
								filepath.Join(root, layout.root, layout.bestEffort, usageFile): 100,
							}
							update := func(sample int64) {
								for name, milliCPU := range counters {
									value := milliCPU * sample * 1000000
									contents := fmt.Sprintf("%d\n", value)
									if layout.version == cgroup.CgroupV2 {
										contents = fmt.Sprintf("usage_usec %d\nuser_usec 0\nsystem_usec 0\n", value/1000)
									}
									writeMetricFile(t, name, contents)
								}
								// 100 busy ticks per second = 1000 millicores.
								writeMetricFile(t, procPath, fmt.Sprintf("cpu %d 0 0 0 0 0 0\n", sample*100))
							}
							update(0)
							collector := &CPUResourceCollector{cgroupManager: cgroup.NewCgroupManager(layout.driver, root, "")}
							synctest.Test(t, func(t *testing.T) {
								done := make(chan struct{})
								defer close(done)
								go func() {
									timer := time.NewTimer(500 * time.Millisecond)
									defer timer.Stop()
									for sample := int64(1); ; sample++ {
										select {
										case <-done:
											return
										case <-timer.C:
											update(sample)
											timer.Reset(time.Second)
										}
									}
								}()
								metrics, err := collector.CollectLocalMetrics(&LocalMetricInfo{
									IncludeGuaranteedPods: guaranteed,
									IncludeBestEffortPods: bestEffort,
									IncludeSystemUsed:     system,
								}, time.Time{}, metav1.Duration{})
								if err != nil {
									t.Fatal(err)
								}
								want := float64(200) // Burstable is always included.
								if guaranteed {
									want += 300
								}
								if bestEffort {
									want += 100
								}
								if system {
									want += 400 // Node usage minus all pod usage.
								}
								if len(metrics) != 1 || len(metrics[0].Samples) != 1 || metrics[0].Samples[0].Value != want {
									t.Fatalf("metrics = %v, want %v millicores", metrics, want)
								}
							})
						})
					}
				}
			}
		})
	}
}
