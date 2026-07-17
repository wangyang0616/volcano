/*
Copyright 2018 The Kubernetes Authors.
Copyright 2018-2023 The Volcano Authors.

Modifications made by Volcano authors:
- Enhanced session initialization with configuration support

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

package framework

import (
	"time"

	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/scheduler/cache"
	"volcano.sh/volcano/pkg/scheduler/conf"
	"volcano.sh/volcano/pkg/scheduler/metrics"
)

// OpenSession start the session
func OpenSession(cache cache.Cache, tiers []conf.Tier, configurations []conf.Configuration) *Session {
	ssn := openSession(cache)
	ssn.Tiers = tiers
	ssn.Configurations = configurations
	ssn.NodeMap = GenerateNodeMapAndSlice(ssn.Nodes)
	ssn.PodLister = NewPodLister(ssn)

	for _, tier := range tiers {
		for _, plugin := range tier.Plugins {
			if pb, found := GetPluginBuilder(plugin.Name); !found {
				klog.Errorf("Failed to get plugin %s.", plugin.Name)
			} else {
				plugin := pb(plugin.Arguments)
				ssn.plugins[plugin.Name()] = plugin
				onSessionOpenStart := time.Now()
				plugin.OnSessionOpen(ssn)
				metrics.UpdatePluginDuration(plugin.Name(), metrics.OnSessionOpen, metrics.Duration(onSessionOpenStart))
			}
		}
	}

	ssn.InitCycleState()

	return ssn
}

// CloseSession close the session
func CloseSession(ssn *Session) {
	for _, plugin := range ssn.plugins {
		onSessionCloseStart := time.Now()
		plugin.OnSessionClose(ssn)
		metrics.UpdatePluginDuration(plugin.Name(), metrics.OnSessionClose, metrics.Duration(onSessionCloseStart))
	}

	closeSession(ssn)
	ssn.cache.OnSessionClose()
}

// CloseSessionReadOnly releases a session opened purely for read-only planning
// (e.g. the repack engine) WITHOUT any cluster writes. The normal CloseSession
// writes PodGroup/Queue status back on close through three paths — plugin
// OnSessionClose (the gang plugin marks PodGroups Unschedulable),
// JobUpdater.UpdateAll (PodGroup status) and updateQueueStatus (Queue allocated).
// Those are the scheduler's responsibility; a second writer would race it and
// flap the conditions. This variant skips all three and only releases in-memory
// session state and the cache's per-session snapshot.
func CloseSessionReadOnly(ssn *Session) {
	closeSessionCleanup(ssn)
}
