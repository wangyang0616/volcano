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

package framework

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/sets"

	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/conf"
)

func TestHyperNodeGradientForJobFnMultiPluginIntersection(t *testing.T) {
	trueValue := true
	hnLow := api.NewHyperNodeInfo(api.BuildHyperNode("low", 1, nil))
	hnHigh := api.NewHyperNodeInfo(api.BuildHyperNode("high", 2, nil))
	root := api.NewHyperNodeInfo(api.BuildHyperNode("root", 3, nil))

	ssn := &Session{
		Tiers: []conf.Tier{{
			Plugins: []conf.PluginOption{
				{Name: "plugin-a", EnabledHyperNodeGradient: &trueValue},
				{Name: "plugin-b", EnabledHyperNodeGradient: &trueValue},
			},
		}},
		HyperNodes: api.HyperNodeInfoMap{
			"low":  hnLow,
			"high": hnHigh,
			"root": root,
		},
		hyperNodeGradientForJobFns: map[string]api.HyperNodeGradientForJobFn{},
	}

	ssn.AddHyperNodeGradientForJobFn("plugin-a", func(_ *api.JobInfo, _ *api.HyperNodeInfo) [][]*api.HyperNodeInfo {
		return [][]*api.HyperNodeInfo{{hnLow, hnHigh}}
	})
	ssn.AddHyperNodeGradientForJobFn("plugin-b", func(_ *api.JobInfo, _ *api.HyperNodeInfo) [][]*api.HyperNodeInfo {
		return [][]*api.HyperNodeInfo{{hnHigh}}
	})

	job := &api.JobInfo{UID: "job-1"}
	result := ssn.HyperNodeGradientForJobFn(job, root)
	assert.Len(t, result, 1)
	assert.Equal(t, "high", result[0][0].Name)

	names := sets.New[string]()
	for _, layer := range result {
		for _, hn := range layer {
			names.Insert(hn.Name)
		}
	}
	assert.Equal(t, sets.New("high"), names)
}
