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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/agent/utils/cgroup"
	"volcano.sh/volcano/pkg/agent/utils/file"
)

var memoryStatMetrics = map[string]bool{
	"total_cache": true,
	"total_rss":   true,
	"total_swap":  true,
}

const (
	defaultMemInfoPath = "/host/proc/meminfo"
	memInfoPathEnv     = "MEM_INFO_PATH_ENV"
)

type MemoryResourceCollector struct {
	cgroupManager cgroup.CgroupManager
}

func NewMemoryResourceCollector(cgroupManager cgroup.CgroupManager) (SubCollector, error) {
	return &MemoryResourceCollector{
		cgroupManager: cgroupManager,
	}, nil
}

func (c *MemoryResourceCollector) Run() {}

func (c *MemoryResourceCollector) CollectLocalMetrics(metricInfo *LocalMetricInfo, start time.Time, window metav1.Duration) ([]*prompb.TimeSeries, error) {
	var count int64
	var err error
	if metricInfo.IncludeSystemUsed {
		count, err = nodeMemoryUsage()
		if err != nil {
			return nil, err
		}
	} else {
		cgroupPath, pathErr := c.cgroupManager.GetRootCgroupPath(cgroup.CgroupMemorySubsystem)
		if pathErr != nil {
			return nil, pathErr
		}
		count, err = getMemoryUsage(cgroupPath, c.cgroupManager.GetCgroupVersion())
		if err != nil {
			return nil, err
		}
	}
	sample := prompb.TimeSeries{
		Samples: []prompb.Sample{
			{
				Timestamp: timestamp.FromTime(time.Now()),
				Value:     float64(count),
			},
		},
	}
	return []*prompb.TimeSeries{&sample}, nil
}

func getMemoryUsage(cgroupRoot string, cgroupVersion string) (int64, error) {
	switch cgroupVersion {
	case cgroup.CgroupV2:
		// memory.stat contains overlapping totals and breakdowns (e.g.
		// file includes shmem, kernel includes slab and pagetables).
		// memory.current accounts for the cgroup and its descendants once.
		filePath := filepath.Join(cgroupRoot, cgroup.MemoryUsageFileV2)
		value, err := file.ReadIntFromFile(filePath)
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", filePath, err)
		}
		return value, nil
	case cgroup.CgroupV1:
		// Preserve the v1 RSS + cache + swap accounting.
	default:
		return 0, fmt.Errorf("unsupported cgroup version %q for memory usage at %s", cgroupVersion, cgroupRoot)
	}
	cgroupMemory := filepath.Join(cgroupRoot, cgroup.MemoryUsageFile)
	data, err := os.ReadFile(cgroupMemory)
	if err != nil {
		return 0, err
	}
	usage := int64(0)
	var hasRSS, hasCache bool
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 || !memoryStatMetrics[fields[0]] {
			continue
		}
		if len(fields) != 2 {
			return 0, fmt.Errorf("invalid memory stat %q in %s", line, cgroupMemory)
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse %s in %s: %w", fields[0], cgroupMemory, err)
		}
		usage += value
		hasRSS = hasRSS || fields[0] == "total_rss"
		hasCache = hasCache || fields[0] == "total_cache"
	}
	if !hasRSS || !hasCache {
		return 0, fmt.Errorf("total_rss or total_cache missing from %s", cgroupMemory)
	}
	return usage, nil
}

// MemoryNodeSource returns the proc file used for host-wide memory accounting.
func MemoryNodeSource() string {
	memInfoFile := os.Getenv(memInfoPathEnv)
	if memInfoFile == "" {
		memInfoFile = defaultMemInfoPath
	}
	return memInfoFile
}

func nodeMemoryUsage() (int64, error) {
	memInfoFile := MemoryNodeSource()
	content, err := os.ReadFile(memInfoFile)
	if err != nil {
		return 0, fmt.Errorf("failed to read mem info, err: %v", err)
	}

	total, available := int64(0), int64(0)
	var foundTotal, foundAvailable bool
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.SplitN(line, ":", 2)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "MemTotal" {
			values := strings.Fields(fields[1])
			if len(values) == 0 {
				return 0, fmt.Errorf("MemTotal is empty in %s", memInfoFile)
			}
			total, err = strconv.ParseInt(values[0], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("failed to parse MemTotal in %s: %w", memInfoFile, err)
			}
			foundTotal = true
		}
		if fields[0] == "MemAvailable" {
			values := strings.Fields(fields[1])
			if len(values) == 0 {
				return 0, fmt.Errorf("MemAvailable is empty in %s", memInfoFile)
			}
			available, err = strconv.ParseInt(values[0], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("failed to parse MemAvailable in %s: %w", memInfoFile, err)
			}
			foundAvailable = true
		}
	}
	if !foundTotal || !foundAvailable || total <= 0 || available < 0 || available > total {
		return 0, fmt.Errorf("invalid MemTotal or MemAvailable in %s", memInfoFile)
	}

	// mem info unit KB.
	systemUsed := (total - available) * 1024
	klog.V(4).InfoS("System used memory", "value", systemUsed)
	return systemUsed, nil
}
