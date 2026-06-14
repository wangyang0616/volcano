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

package perflog

import (
	"time"

	"k8s.io/klog/v2"
)

// LogHyperNodeGradientPlugin logs per-plugin HyperNode gradient evaluation cost.
func LogHyperNodeGradientPlugin(jobNamespace, jobName, pluginName string, latency time.Duration) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("hyperNode gradient perf: plugin gradient",
		"job", klog.KRef(jobNamespace, jobName),
		"plugin", pluginName,
		"latency", latency,
	)
}

// LogHyperNodeGradientForJob logs end-to-end HyperNodeGradientForJobFn cost.
func LogHyperNodeGradientForJob(jobNamespace, jobName string, pluginCount int, latency time.Duration) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("hyperNode gradient perf: gradient for job",
		"job", klog.KRef(jobNamespace, jobName),
		"plugins", pluginCount,
		"latency", latency,
	)
}

// LogHyperNodeGradientIntersect logs multi-plugin gradient intersection cost.
func LogHyperNodeGradientIntersect(pluginCount int, intersectedByTier map[int]int, latency time.Duration) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("hyperNode gradient perf: intersect gradients",
		"plugins", pluginCount,
		"intersectedHyperNodes", countHyperNodesByTier(intersectedByTier),
		"latency", latency,
	)
}

// LogHyperNodeGradientFilterByMinResource logs minResource pre-filter cost in allocate.
func LogHyperNodeGradientFilterByMinResource(inputHyperNodes, outputHyperNodes int, latency time.Duration) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("hyperNode gradient perf: filter by min resource",
		"inputHyperNodes", inputHyperNodes,
		"outputHyperNodes", outputHyperNodes,
		"latency", latency,
	)
}
