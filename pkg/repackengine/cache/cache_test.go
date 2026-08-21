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

package cache

import (
	"testing"

	"k8s.io/client-go/rest"

	schedoptions "volcano.sh/volcano/cmd/scheduler/app/options"
)

func TestNewClusterInitializesSchedulerOptions(t *testing.T) {
	original := schedoptions.ServerOpts
	t.Cleanup(func() { schedoptions.ServerOpts = original })
	schedoptions.ServerOpts = nil

	cluster := NewCluster(&rest.Config{Host: "https://127.0.0.1:6443"})
	if cluster == nil || cluster.scheduler == nil {
		t.Fatal("NewCluster() must construct a scheduler-backed cache")
	}
	if schedoptions.ServerOpts == nil {
		t.Fatal("NewCluster() did not initialize scheduler options")
	}
}
