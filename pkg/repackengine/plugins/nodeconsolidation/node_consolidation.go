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

// Package nodeconsolidation is the node-level defragmentation domain plugin: the freeable unit
// is a single node (weight 1). Enable it in repack-conf's plugins list for
// "free whole nodes" repack. A future hypernode plugin can contribute larger
// units through the same Domain extension point.
package nodeconsolidation

import (
	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the config name for this plugin.
const Name = "nodeconsolidation"

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory:  func(framework.Arguments) framework.Plugin { return &nodeConsolidationPlugin{} },
		Provides: []framework.PluginCapability{framework.CapabilityDomain},
	})
}

type nodeConsolidationPlugin struct{}

func (*nodeConsolidationPlugin) Name() string { return Name }

func (*nodeConsolidationPlugin) OnSessionOpen(ssn *framework.Session) {
	resourceName := ssn.Resource()
	ssn.AddDomainFn(func(snapshot framework.Snapshot) []api.FreeableUnit {
		nodes := snapshot.Nodes()
		out := make([]api.FreeableUnit, 0, len(nodes))
		for _, n := range nodes {
			if n == nil || !snapshot.NodeInScope(n) {
				continue // scope.nodes gates drain targets; out-of-scope = receiver only
			}
			// Empty target-resource nodes must remain idle, while fully occupied
			// nodes are already compact. Only fragmented, partially occupied nodes
			// are meaningful node-consolidation drain targets.
			if api.ClassifyTargetResourceNode(n, resourceName) != api.TargetResourceNodePartial {
				continue
			}
			out = append(out, api.FreeableUnit{Level: "node", Nodes: []string{n.Name}, Weight: 1})
		}
		return out
	})
}

func (*nodeConsolidationPlugin) OnSessionClose(*framework.Session) {}
