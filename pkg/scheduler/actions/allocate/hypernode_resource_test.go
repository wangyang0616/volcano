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

package allocate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"volcano.sh/apis/pkg/apis/scheduling"
	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/framework"
)

func TestFilterGradientsByMinResource(t *testing.T) {
	hn := api.NewHyperNodeInfo(api.BuildHyperNode("hn-1", 1, nil))
	gradients := [][]*api.HyperNodeInfo{{hn}}

	ssn := &framework.Session{
		HyperNodeResourceStatus: api.HyperNodeResourceStatusMap{
			"hn-1": {
				Idle:       &api.Resource{MilliCPU: 1000},
				FutureIdle: &api.Resource{MilliCPU: 1000},
			},
		},
	}
	alloc := &Action{session: ssn}

	newJob := func(cpu string) *api.JobInfo {
		q := resource.MustParse(cpu)
		return &api.JobInfo{
			UID: "job-1",
			PodGroup: &api.PodGroup{
				PodGroup: scheduling.PodGroup{
					Spec: scheduling.PodGroupSpec{
						MinResources: &v1.ResourceList{
							v1.ResourceCPU: q,
						},
					},
				},
			},
		}
	}

	t.Run("sufficient resources", func(t *testing.T) {
		got := alloc.filterGradientsByMinResource(newJob("500m"), nil, gradients)
		assert.Len(t, got, 1)
		assert.Equal(t, "hn-1", got[0][0].Name)
	})

	t.Run("insufficient resources", func(t *testing.T) {
		got := alloc.filterGradientsByMinResource(newJob("5"), nil, gradients)
		assert.Empty(t, got)
	})

	t.Run("skip when job partially allocated", func(t *testing.T) {
		job := newJob("5")
		job.AllocatedHyperNode = "hn-1"
		got := alloc.filterGradientsByMinResource(job, nil, gradients)
		assert.Len(t, got, 1)
	})
}
