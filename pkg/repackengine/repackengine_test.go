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

package repackengine

import (
	"errors"
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedoptions "volcano.sh/volcano/cmd/scheduler/app/options"
	commonutil "volcano.sh/volcano/pkg/util"
)

func TestReconcileConflictDoesNotConsumePoisonPillRetryBudget(t *testing.T) {
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: "repack.volcano.sh", Resource: "repackruns"},
		"run", errors.New("concurrent status update"))
	for _, err := range []error{conflict, fmt.Errorf("persist placement: %w", conflict)} {
		if reconcileErrorConsumesRetryBudget(err) {
			t.Fatalf("conflict %v must not consume the poison-pill retry budget", err)
		}
	}
	if !reconcileErrorConsumesRetryBudget(errors.New("invalid scheduler configuration")) {
		t.Fatal("a deterministic reconcile error must consume the poison-pill retry budget")
	}
}

func TestPlacementCleanupCandidateRetainsLabelOnlyFailure(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{repackv1alpha1.PlacementActiveLabel: "true"},
		},
		Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackFailed,
		},
	}
	if !isPlacementCleanupCandidate(run) {
		t.Fatal("terminal Execute with placement-active label must retry cleanup even when relocations were cleared")
	}
	delete(run.Labels, repackv1alpha1.PlacementActiveLabel)
	if isPlacementCleanupCandidate(run) {
		t.Fatal("terminal Execute without relocations or active label needs no placement cleanup")
	}
}

func TestNewEngineDoesNotPanicWithoutSchedulerServerOpts(t *testing.T) {
	orig := schedoptions.ServerOpts
	t.Cleanup(func() { schedoptions.ServerOpts = orig })
	schedoptions.ServerOpts = nil

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewEngine panicked without scheduler ServerOpts: %v", r)
		}
	}()

	schedOpts := schedoptions.NewServerOption()
	schedOpts.ShardingMode = commonutil.NoneShardingMode
	schedOpts.RegisterOptions()

	cfg := &rest.Config{Host: "https://127.0.0.1:6443"}
	if _, err := NewEngine(cfg, Config{}); err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
}

// Only fully-qualified extended resources (a name with a domain "/") can be
// defragmented; core compute resources are rejected. This mirrors the CEL rule
// on spec.goals[0].resource and guards the --repack-default-resource fallback.
func TestSupportedTarget(t *testing.T) {
	supported := []v1.ResourceName{
		"nvidia.com/gpu",
		"huawei.com/Ascend910",
		"amd.com/gpu",
		"example.com/foo", // any extended resource is fine; the engine is name-agnostic
	}
	for _, r := range supported {
		if !supportedTarget(r) {
			t.Errorf("supportedTarget(%q) = false, want true", r)
		}
	}
	unsupported := []v1.ResourceName{
		v1.ResourceCPU,              // cpu
		v1.ResourceMemory,           // memory
		v1.ResourceEphemeralStorage, // ephemeral-storage
		v1.ResourcePods,             // pods
		"hugepages-2Mi",
		"",
	}
	for _, r := range unsupported {
		if supportedTarget(r) {
			t.Errorf("supportedTarget(%q) = true, want false", r)
		}
	}
}
