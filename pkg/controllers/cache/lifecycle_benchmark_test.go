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
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

// This is an indexed informer/cache microbenchmark, not a real-cluster SLO.
// Run with -bench Lifecycle -benchmem; compare GOMAXPROCS=1 and production CPU.
func BenchmarkLifecycle(b *testing.B) {
	for _, count := range []int{8, 1000, 5000} {
		for _, namespacePods := range []int{5000, 50000} {
			for _, partitioned := range []bool{false, true} {
				b.Run(fmt.Sprintf("pods=%d/ns=%d/partition=%v", count, namespacePods, partitioned), func(b *testing.B) {
					jc := New().(*jobCache)
					job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "bench", Name: "large", UID: "job-uid"}}
					task := batch.TaskSpec{Name: "worker", Replicas: int32(count)}
					if partitioned {
						task.PartitionPolicy = &batch.PartitionPolicySpec{PartitionSize: 8}
					}
					job.Spec.Tasks = []batch.TaskSpec{task}
					pods := make([]*v1.Pod, 0, namespacePods)
					for i := range namespacePods {
						name, uid := job.Name, job.UID
						if i >= count {
							name, uid = "other", "other-job"
						}
						pod := controlledPod(job.Namespace, fmt.Sprintf("worker-%d", i), name, uid, types.UID(fmt.Sprintf("pod-%d", i)))
						pod.Labels = map[string]string{batch.TaskPartitionID: fmt.Sprintf("%d", i/8)}
						pods = append(pods, pod)
					}
					_, podStore := attachInformerStores(b, jc, job, pods...)
					if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
						b.Fatal(err)
					}
					b.Run("rebuild", func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							jc.Lock()
							jc.initialized[JobKey(job)] = false
							jc.Unlock()
							if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
								b.Fatal(err)
							}
						}
					})
					b.Run("stale-request", func(b *testing.B) {
						b.ReportAllocs()
						key := JobKey(job)
						for b.Loop() {
							if _, err := jc.GetForUID(key, "old-job"); err != ErrLifecycleChanged {
								b.Fatal(err)
							}
						}
					})
					b.Run("clone", func(b *testing.B) {
						b.ReportAllocs()
						key := JobKey(job)
						for b.Loop() {
							if _, err := jc.GetForUID(key, job.UID); err != nil {
								b.Fatal(err)
							}
						}
					})
					b.Run("observe", func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							if _, err := jc.ObservePod(pods[0], false); err != nil {
								b.Fatal(err)
							}
						}
					})
					first, second := pods[0].DeepCopy(), pods[0].DeepCopy()
					second.ResourceVersion = "2"
					second.Status.Phase = v1.PodRunning
					b.Run("index-update", func(b *testing.B) {
						b.ReportAllocs()
						current := first
						for b.Loop() {
							if current == first {
								current = second
							} else {
								current = first
							}
							if err := podStore.Update(current); err != nil {
								b.Fatal(err)
							}
						}
					})
					b.Run("index-and-observe-update", func(b *testing.B) {
						b.ReportAllocs()
						current := first
						for b.Loop() {
							if current == first {
								current = second
							} else {
								current = first
							}
							if err := podStore.Update(current); err != nil {
								b.Fatal(err)
							}
							if _, err := jc.ObservePod(current, false); err != nil {
								b.Fatal(err)
							}
						}
					})
				})
			}
		}
	}
}
