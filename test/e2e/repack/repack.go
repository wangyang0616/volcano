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

package repack

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// These are smoke tests: they check the RepackRun CRD admits/rejects specs
// correctly and that a DryRun run is driven to a terminal phase. They require the
// repack CRDs installed, and the DryRun case additionally requires the
// volcano-repack-engine running (helm custom.repack_enable=true).
var _ = Describe("Repack E2E", func() {
	deleteRun := func(name string) {
		_ = e2eutil.VcClient.RepackV1alpha1().RepackRuns().Delete(
			context.TODO(), name, metav1.DeleteOptions{})
	}

	It("rejects a RepackRun whose goal targets a core resource (cpu)", func() {
		// CEL on spec.goals[0].resource requires a fully-qualified extended
		// resource; core resources like cpu/memory must be rejected by the
		// apiserver. This needs only the CRD (no engine).
		run := &repackv1alpha1.RepackRun{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-repack-badresource-"},
			Spec: repackv1alpha1.RepackRunSpec{
				Mode:  repackv1alpha1.RepackModeDryRun,
				Goals: []repackv1alpha1.RepackGoal{{Resource: "cpu"}},
			},
		}
		created, err := e2eutil.VcClient.RepackV1alpha1().RepackRuns().Create(
			context.TODO(), run, metav1.CreateOptions{})
		if err == nil {
			deleteRun(created.Name)
		}
		Expect(err).To(HaveOccurred(), "cpu goal should be rejected by CEL validation")
	})

	It("drives a DryRun RepackRun to a terminal phase", func() {
		run := &repackv1alpha1.RepackRun{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-repack-dryrun-"},
			Spec: repackv1alpha1.RepackRunSpec{
				Mode:  repackv1alpha1.RepackModeDryRun,
				Goals: []repackv1alpha1.RepackGoal{{Resource: "nvidia.com/gpu"}},
			},
		}
		created, err := e2eutil.VcClient.RepackV1alpha1().RepackRuns().Create(
			context.TODO(), run, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(created.Name)

		// The engine picks it up and writes a terminal status. With no matching
		// accelerator nodes it settles as Succeeded (NoFragmentation).
		var last repackv1alpha1.RepackPhase
		err = wait.PollUntilContextTimeout(context.TODO(), 2*time.Second, e2eutil.TwoMinute, false,
			func(ctx context.Context) (bool, error) {
				got, err := e2eutil.VcClient.RepackV1alpha1().RepackRuns().Get(ctx, created.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				last = got.Status.Phase
				switch last {
				case repackv1alpha1.RepackSucceeded, repackv1alpha1.RepackFailed, repackv1alpha1.RepackCancelled:
					return true, nil
				default:
					return false, nil
				}
			})
		Expect(err).NotTo(HaveOccurred(), "DryRun should reach a terminal phase (last=%s); is the repack engine running?", last)
		Expect(last).To(Equal(repackv1alpha1.RepackSucceeded))
	})
})
