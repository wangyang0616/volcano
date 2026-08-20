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

// Package workloadscope turns the resolved RepackRun workload authorization boundary
// into the standard Movable extension point.
package workloadscope

import (
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/framework"
)

const Name = "workloadscope"

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory: func(framework.Arguments) framework.Plugin { return &workloadScopePlugin{} },
	})
}

type workloadScopePlugin struct{}

func (*workloadScopePlugin) Name() string { return Name }

func (*workloadScopePlugin) OnSessionOpen(ssn *framework.Session) {
	matcher := ssn.Scope()
	if matcher == nil {
		return
	}
	ssn.AddMovableFn(func(task *schedapi.TaskInfo) bool {
		return task != nil && matcher.InScope(task.Job)
	})
}

func (*workloadScopePlugin) OnSessionClose(*framework.Session) {}
