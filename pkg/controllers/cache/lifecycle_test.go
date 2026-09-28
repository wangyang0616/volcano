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
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

type countingReader struct {
	SnapshotReader
	lists      int
	beforeList func()
}

func (r *countingReader) ListPods(ns, name string, uid types.UID) ([]*v1.Pod, error) {
	r.lists++
	if r.beforeList != nil {
		r.beforeList()
	}
	return r.SnapshotReader.ListPods(ns, name, uid)
}

func lifecycleFixture(t testing.TB) (*jobCache, *batch.Job, *v1.Pod, toolscache.Indexer, toolscache.Indexer) {
	t.Helper()
	jc := New().(*jobCache)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same", UID: "job-uid", ResourceVersion: "10"}}
	job.Spec.Tasks = []batch.TaskSpec{{Name: "worker", Replicas: 1, PartitionPolicy: &batch.PartitionPolicySpec{TotalPartitions: 1, PartitionSize: 1}}}
	pod := controlledPod(job.Namespace, "same-worker-0", job.Name, job.UID, "pod-uid")
	pod.Labels = map[string]string{batch.TaskPartitionID: "0"}
	jobs, pods := attachInformerStores(t, jc, job, pod)
	if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
		t.Fatal(err)
	}
	return jc, job, pod, jobs, pods
}

func TestRebuildDoesNotResurrectCompletedDelete(t *testing.T) {
	jc, job, pod, _, pods := lifecycleFixture(t)
	if err := pods.Delete(pod); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.ObservePod(pod, true); err != nil {
		t.Fatal(err)
	}
	jc.initialized[JobKey(job)] = false
	if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
		t.Fatal(err)
	}
	// A queued Add callback must also consult the now-empty store.
	if _, err := jc.ObservePod(pod, false); err != nil {
		t.Fatal(err)
	}
	got, err := jc.GetForUID(JobKey(job), job.UID)
	if err != nil || len(got.Pods) != 0 {
		t.Fatalf("deleted Pod resurrected: %#v, %v", got, err)
	}
}

func TestLateCallbacksKeepCurrentPodAndPartition(t *testing.T) {
	jc, job, old, _, pods := lifecycleFixture(t)
	current := old.DeepCopy()
	current.UID = "replacement-pod"
	current.Labels[batch.TaskPartitionID] = "1"
	current.Status.Phase = v1.PodRunning
	if err := pods.Update(current); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, []string{old.Name}); err != nil {
		t.Fatal(err)
	}
	for _, deleted := range []bool{false, true} {
		if _, err := jc.ObservePod(old, deleted); err != nil {
			t.Fatal(err)
		}
	}
	if err := jc.DeletePod(old); err != nil {
		t.Fatal(err)
	}
	got, err := jc.GetForUID(JobKey(job), job.UID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pods["worker"][old.Name] != current {
		t.Fatal("late callback replaced current Pod")
	}
	if got.Partitions["worker"].Partition["1"][old.Name] != current || len(got.Partitions["worker"].Partition["0"]) != 0 {
		t.Fatal("partition projection inconsistent")
	}
}

func TestRefreshPreservesStatusAndCanRepairAgain(t *testing.T) {
	jc, job, pod, _, pods := lifecycleFixture(t)
	confirmed := job.DeepCopy()
	confirmed.ResourceVersion = "20"
	confirmed.Status.Version = 3
	if err := jc.Update(confirmed); err != nil {
		t.Fatal(err)
	}
	reader := &countingReader{SnapshotReader: jc.reader}
	jc.reader = reader
	for i := 1; i <= 2; i++ {
		current := pod.DeepCopy()
		current.UID = types.UID(fmt.Sprintf("replacement-%d", i))
		if err := pods.Update(current); err != nil {
			t.Fatal(err)
		}
		if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, []string{pod.Name}); err != nil {
			t.Fatal(err)
		}
		if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, []string{pod.Name}); err != nil {
			t.Fatal(err)
		}
		got, err := jc.GetForUID(JobKey(job), job.UID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Job.ResourceVersion != "20" || got.Job.Status.Version != 3 {
			t.Fatal("informer rolled back confirmed Job status")
		}
		if got.Pods["worker"][pod.Name].UID != current.UID {
			t.Fatal("second repair was suppressed")
		}
		got.Job.Status.Version = 99 // clone cannot mutate the shared cache
	}
	if reader.lists != 2 {
		t.Fatalf("listed %d times, want two actual repairs", reader.lists)
	}
}

func TestRebuildRechecksJobUIDBeforeCommit(t *testing.T) {
	jc, job, pod, jobs, pods := lifecycleFixture(t)
	replacement := pod.DeepCopy()
	replacement.UID = "new-pod"
	if err := pods.Update(replacement); err != nil {
		t.Fatal(err)
	}
	successor := job.DeepCopy()
	successor.UID = "successor-job"
	reader := &countingReader{SnapshotReader: jc.reader, beforeList: func() {
		if err := jobs.Update(successor); err != nil {
			t.Fatal(err)
		}
	}}
	jc.reader = reader
	if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, []string{pod.Name}); !errors.Is(err, ErrLifecycleChanged) {
		t.Fatalf("expected lifecycle change, got %v", err)
	}
	if jc.jobs[JobKey(job)].Pods["worker"][pod.Name].UID != pod.UID {
		t.Fatal("committed abandoned snapshot")
	}
}

func TestRebuildAndQueuedCallbackConverge(t *testing.T) {
	jc, job, pod, _, pods := lifecycleFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	jc.initialized[JobKey(job)] = false
	jc.reader = &countingReader{SnapshotReader: jc.reader, beforeList: func() { close(entered); <-release }}
	rebuilt := make(chan error, 1)
	go func() { _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); rebuilt <- err }()
	<-entered
	if err := pods.Delete(pod); err != nil {
		t.Fatal(err)
	}
	observed := make(chan error, 1)
	go func() { _, err := jc.ObservePod(pod, true); observed <- err }()
	close(release)
	if err := <-rebuilt; err != nil {
		t.Fatal(err)
	}
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
	got, err := jc.GetForUID(JobKey(job), job.UID)
	if err != nil || len(got.Pods) != 0 {
		t.Fatalf("non-convergent result: %#v, %v", got, err)
	}
}

func TestOwnerIndexIgnoresMutableLabelsAndUnmanagedPods(t *testing.T) {
	jc, job, pod, _, pods := lifecycleFixture(t)
	pod = pod.DeepCopy()
	pod.Labels[batch.JobNameKey] = "wrong-name"
	if err := pods.Update(pod); err != nil {
		t.Fatal(err)
	}
	unmanaged := pod.DeepCopy()
	unmanaged.Name = "deployment-pod"
	unmanaged.OwnerReferences[0].Kind = "ReplicaSet"
	unmanaged.OwnerReferences[0].APIVersion = "apps/v1"
	if err := pods.Add(unmanaged); err != nil {
		t.Fatal(err)
	}
	jc.initialized[JobKey(job)] = false
	if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := jc.GetForUID(JobKey(job), job.UID)
	if len(got.Pods["worker"]) != 1 || !got.HasPod(pod) {
		t.Fatal("owner index used labels or selected unmanaged Pod")
	}
	malformed := pod.DeepCopy()
	malformed.UID = "malformed"
	delete(malformed.Annotations, batch.TaskSpecKey)
	if err := pods.Update(malformed); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, []string{pod.Name}); err == nil {
		t.Fatal("accepted invalid task metadata")
	}
	if _, err := jc.ObservePod(malformed, false); err == nil {
		t.Fatal("accepted malformed incremental observation")
	}
	if !jc.HasPod(pod) {
		t.Fatal("failed transaction replaced prior cache")
	}
}

func TestGetForUIDChecksReadinessAndIdentity(t *testing.T) {
	jc, job, _, _, _ := lifecycleFixture(t)
	if _, err := jc.GetForUID(JobKey(job), "retired"); !errors.Is(err, ErrLifecycleChanged) {
		t.Fatal(err)
	}
	jc.initialized[JobKey(job)] = false
	if _, err := jc.GetForUID(JobKey(job), job.UID); !errors.Is(err, ErrNeedsRecovery) {
		t.Fatal(err)
	}
	if err := jc.Delete(job); err != nil {
		t.Fatal(err)
	}
	if err := jc.Add(job); !errors.Is(err, ErrLifecycleChanged) {
		t.Fatalf("resurrected retired Job: %v", err)
	}
	if err := jc.Update(job); !errors.Is(err, ErrLifecycleChanged) {
		t.Fatalf("resurrected retired Job through Update: %v", err)
	}
}

func TestRecoveryIdentityChecksAreRateLimitedPerName(t *testing.T) {
	jc := New().(*jobCache)
	defer jc.deletedJobs.ShutDown()
	key := "test/lagging"
	if !jc.AllowIdentityCheck(key, "new-job") {
		t.Fatal("first check suppressed")
	}
	for range 10 {
		jc.RecoveryDelay(key, "new-job")
		if jc.AllowIdentityCheck(key, "new-job") {
			t.Fatal("Pod requests each perform a live GET")
		}
	}
	jc.Lock()
	entry := jc.recovery[key]
	entry.checkedUntil = time.Now().Add(-time.Second)
	jc.recovery[key] = entry
	jc.Unlock()
	if !jc.AllowIdentityCheck(key, "new-job") {
		t.Fatal("rate gate permanently blocked progress")
	}
	jc.ResetRecovery(key, "new-job")
	if len(jc.recovery) != 0 {
		t.Fatal("completed lifecycle leaked wait state")
	}
}

func TestConcurrentLifecycleReaders(t *testing.T) {
	jc, job, pod, _, _ := lifecycleFixture(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				if _, err := jc.ObservePod(pod, false); err != nil {
					t.Error(err)
				}
				if _, err := jc.GetForUID(JobKey(job), job.UID); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}

func TestStaleDeleteAndMetricUpdateKeepCurrentLifecycleCounters(t *testing.T) {
	jc, old, _, jobs, _ := lifecycleFixture(t)
	jc.RecordJobPhase(old, batch.Completed)
	current := old.DeepCopy()
	current.UID = "new-job"
	if err := jobs.Update(current); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.RebuildLifecycle(current.Namespace, current.Name, current.UID, nil); err != nil {
		t.Fatal(err)
	}
	jc.RecordJobPhase(current, batch.Completed)
	if err := jc.Delete(old); err != nil {
		t.Fatal(err)
	}
	jc.RecordJobPhase(old, batch.Completed)
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if !strings.HasSuffix(family.GetName(), "job_completed_phase_count") {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "job_name" && label.GetValue() == JobKey(current) {
					if metric.GetCounter().GetValue() != 1 {
						t.Fatalf("stale lifecycle changed counter: %v", metric.GetCounter().GetValue())
					}
					return
				}
			}
		}
	}
	t.Fatal("stale delete removed current lifecycle metric")
}
