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

package resourceusage

import (
	"time"

	"github.com/prometheus/prometheus/prompb"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/metriccollect"
	"volcano.sh/volcano/pkg/metriccollect/local"
)

// Resource include the overSubscription resource on this node
type Resource map[v1.ResourceName]int64

// Getter is used to get cpu/memory usage of current node.
type Getter interface {
	// UsagesByValue return absolute resource usage of node
	UsagesByValue(includeGuaranteedPods bool, includeBestEffortPods bool) Resource
	// UsagesByPercentage return resource usage percentage of node
	UsagesByPercentage(node *v1.Node) Resource
}

// getter implements Getter.
type getter struct {
	collectorName string
	collector     *metriccollect.MetricCollectorManager
}

// NewUsageGetter create a usage getter
func NewUsageGetter(mgr *metriccollect.MetricCollectorManager, collectorName string) Getter {
	g := &getter{
		collectorName: collectorName,
		collector:     mgr,
	}
	return g
}

// UsagesByValue return absolute resource usage of node
func (g *getter) UsagesByValue(includeGuaranteedPods, includeBestEffortPods bool) Resource {
	return g.commonUsage(includeGuaranteedPods, includeBestEffortPods)
}

// UsagesByPercentage return resource usage percentage of node
func (g *getter) UsagesByPercentage(node *v1.Node) Resource {
	res := make(Resource)
	usage := g.commonUsage(true, true)

	cpuAllocatable := node.Status.Allocatable[v1.ResourceCPU]
	memoryAllocatable := node.Status.Allocatable[v1.ResourceMemory]
	cpuTotal := cpuAllocatable.MilliValue()
	memoryTotal := memoryAllocatable.Value()

	if cpuUsed, ok := usage[v1.ResourceCPU]; ok {
		if cpuTotal > 0 {
			res[v1.ResourceCPU] = cpuUsed * 100 / cpuTotal
		} else {
			klog.ErrorS(nil, "Cannot calculate CPU usage percentage without allocatable CPU", "node", klog.KObj(node))
		}
	}
	if memoryUsed, ok := usage[v1.ResourceMemory]; ok {
		if memoryTotal > 0 {
			res[v1.ResourceMemory] = memoryUsed * 100 / memoryTotal
		} else {
			klog.ErrorS(nil, "Cannot calculate memory usage percentage without allocatable memory", "node", klog.KObj(node))
		}
	}

	return res
}

func (g *getter) convertMetric(metric []*prompb.TimeSeries) (int64, bool) {
	ret := int64(0)
	hasSample := false
	for _, ts := range metric {
		if ts == nil {
			continue
		}
		for _, value := range ts.Samples {
			hasSample = true
			ret += int64(value.Value)
		}
	}
	return ret, hasSample
}

func (g *getter) commonUsage(includeGuaranteedPods, includeBestEffortPods bool) Resource {
	res := make(Resource)
	c, err := g.collector.GetPluginByName(g.collectorName)
	if err != nil {
		klog.ErrorS(err, "Failed to get resource collector", "name", g.collectorName)
		return res
	}

	includeSystemUsed := g.collector.Config.GenericConfiguration.IncludeSystemUsage
	metricInfos := []struct {
		resource v1.ResourceName
		info     *local.LocalMetricInfo
	}{
		{v1.ResourceCPU, &local.LocalMetricInfo{ResourceType: "cpu", IncludeGuaranteedPods: includeGuaranteedPods, IncludeBestEffortPods: includeBestEffortPods, IncludeSystemUsed: includeSystemUsed}},
		{v1.ResourceMemory, &local.LocalMetricInfo{ResourceType: "memory", IncludeSystemUsed: includeSystemUsed}},
	}

	for _, entry := range metricInfos {
		metric, err := c.CollectMetrics(entry.info, time.Time{}, metav1.Duration{})
		if err != nil {
			klog.ErrorS(err, "Failed to collect resource usage", "resource", entry.resource, "collector", g.collectorName)
			continue
		}
		sumUsage, hasSample := g.convertMetric(metric)
		if !hasSample {
			klog.ErrorS(nil, "Resource collector returned no samples", "resource", entry.resource, "collector", g.collectorName)
			continue
		}
		res[entry.resource] = sumUsage
	}

	return res
}
