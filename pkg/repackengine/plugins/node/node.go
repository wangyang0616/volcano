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

// Package node is the node-level defragmentation domain plugin: the freeable unit
// is a single node (weight 1). Enable it (tiers in --scheduler-conf) for "free
// whole nodes" repack. The hypernode plugin contributes larger units; with both
// enabled the core optimizes their combined benefit.
package node

import (
	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the config name for this plugin.
const Name = "node"

func init() {
	framework.RegisterPlugin(Name, func() framework.Plugin { return &nodePlugin{} })
}

type nodePlugin struct{}

func (*nodePlugin) Name() string { return Name }

func (*nodePlugin) OnSessionOpen(ssn *framework.Session) {
	ssn.AddDomainFn(func(snapshot framework.Snapshot) []api.FreeableUnit {
		nodes := snapshot.Nodes()
		out := make([]api.FreeableUnit, 0, len(nodes))
		for _, n := range nodes {
			if n == nil || !snapshot.NodeInScope(n) {
				continue // scope.nodes gates drain targets; out-of-scope = receiver only
			}
			out = append(out, api.FreeableUnit{Level: "node", Nodes: []string{n.Name}, Weight: 1})
		}
		return out
	})
}

func (*nodePlugin) OnSessionClose(*framework.Session) {}
