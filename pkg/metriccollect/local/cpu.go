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

package local

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/prompb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/agent/utils/cgroup"
	"volcano.sh/volcano/pkg/agent/utils/file"
)

const (
	defaultProcPath = "/host/proc/stat"
	procStatPathEnv = "PROC_STAT_PATH"

	// jiffies is the kernel tick unit.
	jiffies = float64(10 * time.Millisecond)
)

type CPUResourceCollector struct {
	cgroupManager cgroup.CgroupManager
}

func NewCPUResourceCollector(cgroupManager cgroup.CgroupManager) (SubCollector, error) {
	return &CPUResourceCollector{
		cgroupManager: cgroupManager,
	}, nil
}

func (c *CPUResourceCollector) Run() {}

func (c *CPUResourceCollector) CollectLocalMetrics(metricInfo *LocalMetricInfo, start time.Time, window metav1.Duration) ([]*prompb.TimeSeries, error) {
	rootPath, err := c.cgroupManager.GetRootCgroupPath(cgroup.CgroupCpuAcctSubsystem)
	if err != nil {
		return nil, err
	}

	cgroupVersion := c.cgroupManager.GetCgroupVersion()
	paths := []string{rootPath}
	var burstablePath, bestEffortPath string
	if !metricInfo.IncludeGuaranteedPods {
		burstablePath, err = c.cgroupManager.GetQoSCgroupPath(corev1.PodQOSBurstable, cgroup.CgroupCpuAcctSubsystem)
		if err != nil {
			return nil, err
		}
		paths = append(paths, burstablePath)
	}
	if metricInfo.IncludeGuaranteedPods != metricInfo.IncludeBestEffortPods {
		bestEffortPath, err = c.cgroupManager.GetQoSCgroupPath(corev1.PodQOSBestEffort, cgroup.CgroupCpuAcctSubsystem)
		if err != nil {
			return nil, err
		}
		paths = append(paths, bestEffortPath)
	}

	usage, nodeUsage, err := sampleCPUUsages(paths, cgroupVersion, metricInfo.IncludeSystemUsed)
	if err != nil {
		return nil, err
	}
	podAllUsage := usage[rootPath]

	finalUsage := int64(0)
	if metricInfo.IncludeGuaranteedPods {
		finalUsage = podAllUsage
		if !metricInfo.IncludeBestEffortPods {
			// Root counters include all QoS children, so subtract BestEffort.
			finalUsage -= usage[bestEffortPath]
		}
	} else {
		finalUsage = usage[burstablePath]
		if metricInfo.IncludeBestEffortPods {
			finalUsage += usage[bestEffortPath]
		}
	}

	if metricInfo.IncludeSystemUsed {
		if nodeUsage < podAllUsage {
			klog.V(2).InfoS("Node CPU usage is below pod CPU usage; ignoring negative system usage", "nodeMilliCPU", nodeUsage, "podMilliCPU", podAllUsage)
		} else {
			finalUsage += nodeUsage - podAllUsage
		}
	}
	if finalUsage < 0 {
		klog.V(2).InfoS("Calculated CPU usage is negative; using zero", "milliCPU", finalUsage)
		finalUsage = 0
	}
	sample := prompb.TimeSeries{
		Samples: []prompb.Sample{
			{
				Timestamp: timestamp.FromTime(time.Now()),
				Value:     float64(finalUsage),
			},
		},
	}
	return []*prompb.TimeSeries{&sample}, nil
}

// sampleCPUUsages samples every requested cgroup and the optional node counter
// over one interval so QoS subtraction compares the same second of activity.
func sampleCPUUsages(paths []string, cgroupVersion string, includeNode bool) (map[string]int64, int64, error) {
	startTime := time.Now()
	startCounters := make(map[string]int64, len(paths))
	for _, path := range paths {
		value, err := readCPUUsage(path, cgroupVersion)
		if err != nil {
			return nil, 0, fmt.Errorf("read initial CPU usage: %w", err)
		}
		startCounters[path] = value
	}
	var startNode uint64
	if includeNode {
		var err error
		startNode, err = nodeCPUState()
		if err != nil {
			return nil, 0, fmt.Errorf("read initial node CPU usage: %w", err)
		}
	}

	time.Sleep(time.Second)
	endTime := time.Now()
	duration := endTime.Sub(startTime)
	if duration <= 0 {
		return nil, 0, fmt.Errorf("CPU sampling interval is not positive")
	}
	usage := make(map[string]int64, len(paths))
	for _, path := range paths {
		value, err := readCPUUsage(path, cgroupVersion)
		if err != nil {
			return nil, 0, fmt.Errorf("read final CPU usage: %w", err)
		}
		if value < startCounters[path] {
			return nil, 0, fmt.Errorf("CPU counter decreased at %s", path)
		}
		usage[path] = (value - startCounters[path]) * 1000 / duration.Nanoseconds()
	}

	var nodeUsage int64
	if includeNode {
		endNode, err := nodeCPUState()
		if err != nil {
			return nil, 0, fmt.Errorf("read final node CPU usage: %w", err)
		}
		if endNode <= startNode {
			return nil, 0, fmt.Errorf("node CPU counter did not increase")
		}
		nodeUsage = int64((float64(endNode-startNode) / (float64(duration) / jiffies)) * 1000)
		klog.V(4).InfoS("Node CPU usage", "milliCPU", nodeUsage)
	}
	return usage, nodeUsage, nil
}

// readCPUUsage returns cumulative CPU time in nanoseconds for either version.
func readCPUUsage(cgroupRoot, cgroupVersion string) (int64, error) {
	switch cgroupVersion {
	case cgroup.CgroupV1:
		filePath := filepath.Join(cgroupRoot, cgroup.CPUUsageFile)
		value, err := file.ReadIntFromFile(filePath)
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", filePath, err)
		}
		return value, nil
	case cgroup.CgroupV2:
		return readCPUUsageV2(filepath.Join(cgroupRoot, cgroup.CPUUsageFileV2))
	default:
		return 0, fmt.Errorf("unsupported cgroup version %q for CPU usage at %s", cgroupVersion, cgroupRoot)
	}
}

func nodeCPUState() (uint64, error) {
	procStatFile := os.Getenv(procStatPathEnv)
	if procStatFile == "" {
		procStatFile = defaultProcPath
	}
	contents, err := os.ReadFile(procStatFile)
	if err != nil {
		return 0, fmt.Errorf("failed to read proc state file: %v", err)
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		total := uint64(0)
		// Guest time is already included in user/nice. Exclude idle and
		// iowait because neither represents busy CPU time.
		for i := 1; i < len(fields); i++ {
			val, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("failed to parse CPU field %q in %s: %w", fields[i], procStatFile, err)
			}
			if i == 4 || i == 5 || i == 9 || i == 10 {
				continue
			}
			total += val
		}
		return total, nil
	}
	return 0, fmt.Errorf("CPU aggregate line missing from %s", procStatFile)
}

func readCPUUsageV2(filePath string) (int64, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return 0, err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		if fields[0] == "usage_usec" {
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("failed to parse usage_usec in %s: %w", filePath, err)
			}
			return value * 1000, nil
		}
	}

	return 0, fmt.Errorf("usage_usec missing from %s", filePath)
}
