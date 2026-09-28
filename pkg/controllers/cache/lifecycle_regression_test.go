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
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	"volcano.sh/volcano/pkg/controllers/apis"
)

func TestTerminalAcknowledgementsFollowPodAndJobUID(t *testing.T) {
	jc, job, initial, jobs, pods := lifecycleFixture(t)
	defer jc.deletedJobs.ShutDown()
	pod := initial.DeepCopy()
	pod.Status.Phase = v1.PodFailed
	if err := pods.Update(pod); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.ObservePod(initial, false); err != nil {
		t.Fatal(err)
	}
	req := apis.Request{Namespace: job.Namespace, JobName: job.Name, JobUid: job.UID, TaskName: "worker", PodName: pod.Name, PodUID: pod.UID}
	jc.AcknowledgeTerminalPod(req)
	assertHandled := func(uid types.UID, count int) {
		t.Helper()
		info, err := jc.GetForUID(JobKey(job), uid)
		if err != nil || len(info.HandledTerminalPods) != count {
			t.Fatalf("terminal acknowledgement count, want %d: info=%+v, err=%v", count, info, err)
		}
		// Clones must not share the acknowledgement map with the cache.
		clear(info.HandledTerminalPods)
	}
	assertHandled(job.UID, 1)
	metadata := pod.DeepCopy()
	metadata.ResourceVersion = "11"
	if err := pods.Update(metadata); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.ObservePod(metadata, false); err != nil {
		t.Fatal(err)
	}
	assertHandled(job.UID, 1)
	jc.initialized[JobKey(job)] = false
	if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
		t.Fatal(err)
	}
	assertHandled(job.UID, 1)
	moved := metadata.DeepCopy()
	moved.Annotations[batch.TaskSpecKey] = "renamed-task"
	if err := pods.Update(moved); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.ObservePod(moved, false); err != nil {
		t.Fatal(err)
	}
	jc.jobs[JobKey(job)].HandledTerminalPods = nil
	jc.AcknowledgeTerminalPod(req) // request still has the original task name
	assertHandled(job.UID, 1)
	replacement := metadata.DeepCopy()
	replacement.UID = "replacement"
	if err := pods.Update(replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.ObservePod(pod, true); err != nil {
		t.Fatal(err)
	}
	jc.AcknowledgeTerminalPod(req) // delayed execution of the old Pod
	assertHandled(job.UID, 0)
	req.PodUID = replacement.UID
	jc.AcknowledgeTerminalPod(req)
	assertHandled(job.UID, 1)
	if err := pods.Delete(replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.ObservePod(replacement, true); err != nil {
		t.Fatal(err)
	}
	assertHandled(job.UID, 0)
	currentJob := job.DeepCopy()
	currentJob.UID = "new-job"
	if err := jobs.Update(currentJob); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, currentJob.UID, nil); err != nil {
		t.Fatal(err)
	}
	jc.AcknowledgeTerminalPod(req)
	assertHandled(currentJob.UID, 0)
}

func TestJobCacheOwnsJobCopies(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(fmt.Sprintf("update=%v", update), func(t *testing.T) {
			jc := New().(*jobCache)
			defer jc.deletedJobs.ShutDown()
			job := &batch.Job{
				ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "copies", UID: "job", ResourceVersion: "1", Annotations: map[string]string{"value": "original"}},
				Spec: batch.JobSpec{Tasks: []batch.TaskSpec{{Name: "worker", Template: v1.PodTemplateSpec{Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "worker", Command: []string{"original"}}},
				}}}}},
				Status: batch.JobStatus{ControlledResources: map[string]string{"plugin": "original"}},
			}
			if err := jc.Add(job); err != nil {
				t.Fatal(err)
			}
			if update {
				job = job.DeepCopy()
				job.ResourceVersion = "2"
				if err := jc.Update(job); err != nil {
					t.Fatal(err)
				}
			}
			job.Annotations["value"] = "mutated"
			job.Spec.Tasks[0].Template.Spec.Containers[0].Command[0] = "mutated"
			job.Status.ControlledResources["plugin"] = "mutated"
			status, err := jc.GetStatus(JobKey(job))
			if err != nil {
				t.Fatal(err)
			}
			status.ControlledResources["plugin"] = "mutated by reader"
			got, err := jc.GetForUID(JobKey(job), job.UID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Job.Annotations["value"] != "original" || got.Job.Spec.Tasks[0].Template.Spec.Containers[0].Command[0] != "original" || got.Job.Status.ControlledResources["plugin"] != "original" {
				t.Fatal("caller modified the cached Job without a cache write")
			}
		})
	}
}

func TestDeletedEntryRemainsTerminalAfterRetirementEviction(t *testing.T) {
	jc, job, pod, jobs, _ := lifecycleFixture(t)
	defer jc.deletedJobs.ShutDown()
	if err := jobs.Delete(job); err != nil {
		t.Fatal(err)
	}
	if err := jc.Delete(job); err != nil {
		t.Fatal(err)
	}
	// The old Pod keeps this entry alive while other Job lifecycles retire.
	for i := range 4096 {
		jc.RetireUID(types.UID(fmt.Sprintf("other-job-%d", i)))
	}
	if jc.IsRetired(job.UID) {
		t.Fatal("test did not evict the old proof")
	}
	confirmed := job.DeepCopy()
	confirmed.ResourceVersion = "20"
	if err := jc.Update(confirmed); !errors.Is(err, ErrLifecycleChanged) {
		t.Fatalf("late API response reactivated deleted entry: %v", err)
	}
	if _, err := jc.GetForUID(JobKey(job), job.UID); err == nil {
		t.Fatal("deleted entry became runnable")
	}
	if !jc.HasPod(pod) {
		t.Fatal("lost old Pods before cleanup")
	}
	// Independently exercise Add/Rebuild with a stale reader: correctness of
	// an existing tombstone must not depend on the bounded proof lookup.
	if err := jobs.Add(job); err != nil {
		t.Fatal(err)
	}
	if err := jc.Add(job); !errors.Is(err, ErrLifecycleChanged) {
		t.Fatalf("Add reactivated deleted entry: %v", err)
	}
	if _, err := jc.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); !errors.Is(err, ErrLifecycleChanged) {
		t.Fatalf("Rebuild reactivated deleted entry: %v", err)
	}
	current := job.DeepCopy()
	current.UID = "successor"
	if err := jobs.Update(current); err != nil {
		t.Fatal(err)
	}
	if err := jc.Add(current); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.RebuildLifecycle(current.Namespace, current.Name, current.UID, nil); err != nil {
		t.Fatal(err)
	}
	jc.processCleanupJob()
	got, err := jc.GetForUID(JobKey(current), current.UID)
	if err != nil || got.Deleted || len(got.Pods) != 0 {
		t.Fatalf("tombstone or cleanup damaged successor: %#v, %v", got, err)
	}
}
