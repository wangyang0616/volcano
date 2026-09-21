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

package job

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	bus "volcano.sh/apis/pkg/apis/bus/v1alpha1"
	"volcano.sh/volcano/pkg/controllers/apis"
	jobcache "volcano.sh/volcano/pkg/controllers/cache"
)

func TestRecoverCurrentJobAfterCacheMiss(t *testing.T) {
	controller := newFakeController()
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "test",
		Name:      "same-name",
		UID:       "new-uid",
	}}
	if err := controller.jobInformer.Informer().GetIndexer().Add(job); err != nil {
		t.Fatalf("add job to informer: %v", err)
	}

	staleReq := apis.Request{
		Namespace: "test",
		JobName:   "same-name",
		JobUid:    "old-uid",
		Event:     bus.PodEvictedEvent,
	}
	queue := controller.getWorkerQueue(jobcache.JobKeyByReq(&staleReq))
	defer queue.ShutDown()
	controller.recoverCurrentJob(queue, staleReq)

	jobInfo, err := controller.cache.Get(jobcache.JobKeyByName(job.Namespace, job.Name))
	if err != nil {
		t.Fatalf("cache was not recovered from informer: %v", err)
	}
	if jobInfo.UID != job.UID {
		t.Fatalf("got recovered uid %q, want %q", jobInfo.UID, job.UID)
	}

	obj, shutdown := queue.Get()
	if shutdown {
		t.Fatal("worker queue unexpectedly shut down")
	}
	recoveredReq := obj.(apis.Request)
	queue.Done(obj)
	if recoveredReq.JobUid != job.UID {
		t.Fatalf("got queued uid %q, want %q", recoveredReq.JobUid, job.UID)
	}
	if recoveredReq.Event != bus.OutOfSyncEvent {
		t.Fatalf("got event %q, want %q", recoveredReq.Event, bus.OutOfSyncEvent)
	}
}

func TestRecoverCurrentJobReplacesOldLifecycle(t *testing.T) {
	controller := newFakeController()
	oldJob := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same-name", UID: "old-uid"}}
	newJob := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same-name", UID: "new-uid"}}
	if err := controller.cache.Add(oldJob); err != nil {
		t.Fatalf("add old job to cache: %v", err)
	}
	if err := controller.jobInformer.Informer().GetIndexer().Add(newJob); err != nil {
		t.Fatalf("add new job to informer: %v", err)
	}

	req := apis.Request{Namespace: newJob.Namespace, JobName: newJob.Name, JobUid: newJob.UID, Event: bus.OutOfSyncEvent}
	queue := controller.getWorkerQueue(jobcache.JobKeyByReq(&req))
	defer queue.ShutDown()
	controller.recoverCurrentJob(queue, req)

	got, err := controller.cache.Get(jobcache.JobKeyByName(newJob.Namespace, newJob.Name))
	if err != nil {
		t.Fatalf("get recovered job: %v", err)
	}
	if got.UID != newJob.UID {
		t.Fatalf("got recovered uid %q, want %q", got.UID, newJob.UID)
	}
}

func TestUpdateJobReplacesLifecycleReportedAsUpdate(t *testing.T) {
	controller := newFakeController()
	oldJob := &batch.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "test", Name: "same-name", UID: "old-uid", ResourceVersion: "10",
	}}
	newJob := &batch.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "test", Name: "same-name", UID: "new-uid", ResourceVersion: "11",
	}}
	if err := controller.cache.Add(oldJob); err != nil {
		t.Fatalf("add old job to cache: %v", err)
	}

	controller.updateJob(oldJob, newJob)

	got, err := controller.cache.Get(jobcache.JobKeyByName(newJob.Namespace, newJob.Name))
	if err != nil {
		t.Fatalf("get replacement job: %v", err)
	}
	if got.UID != newJob.UID {
		t.Fatalf("update event left uid %q in cache, want %q", got.UID, newJob.UID)
	}
	queue := controller.getWorkerQueue(jobcache.JobKeyByName(newJob.Namespace, newJob.Name))
	defer queue.ShutDown()
	obj, shutdown := queue.Get()
	if shutdown {
		t.Fatal("worker queue unexpectedly shut down")
	}
	defer queue.Done(obj)
	req := obj.(apis.Request)
	if req.JobUid != newJob.UID || req.Event != bus.OutOfSyncEvent {
		t.Fatalf("got replacement request %#v", req)
	}
}

func TestWorkerRejectsActionFromOldJobLifecycle(t *testing.T) {
	controller := newFakeController()
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "test",
		Name:      "same-name",
		UID:       "new-uid",
	}}
	if err := controller.jobInformer.Informer().GetIndexer().Add(job); err != nil {
		t.Fatalf("add job to informer: %v", err)
	}
	if err := controller.cache.Add(job); err != nil {
		t.Fatalf("add current job to cache: %v", err)
	}

	staleReq := apis.Request{
		Namespace: "test",
		JobName:   "same-name",
		JobUid:    "old-uid",
		Event:     bus.CommandIssuedEvent,
		Action:    bus.TerminateJobAction,
	}
	key := jobcache.JobKeyByReq(&staleReq)
	worker := controller.genHash(key) % controller.workers
	queue := controller.queueList[worker]
	defer queue.ShutDown()
	queue.Add(staleReq)

	if !controller.processNextReq(worker) {
		t.Fatal("worker unexpectedly stopped")
	}
	obj, shutdown := queue.Get()
	if shutdown {
		t.Fatal("worker queue unexpectedly shut down")
	}
	defer queue.Done(obj)
	recoveredReq := obj.(apis.Request)
	if recoveredReq.JobUid != job.UID || recoveredReq.Event != bus.OutOfSyncEvent || recoveredReq.Action != "" {
		t.Fatalf("old action was not replaced with a current sync request: %#v", recoveredReq)
	}
}

func TestPodResyncRejectsSameNameNewPod(t *testing.T) {
	controller := newFakeController()
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same-name", UID: "new-job-uid"}}
	if err := controller.cache.Add(job); err != nil {
		t.Fatalf("add current job: %v", err)
	}

	newPod := buildPod("test", "same-name-worker-0", v1.PodPending, nil)
	newPod.UID = "new-pod-uid"
	newPod.OwnerReferences[0].UID = job.UID
	addPodAnnotation(newPod, map[string]string{
		batch.JobNameKey:  job.Name,
		batch.JobVersion:  "0",
		batch.TaskSpecKey: "worker",
	})
	if _, err := controller.kubeClient.CoreV1().Pods(newPod.Namespace).Create(context.Background(), newPod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create current pod: %v", err)
	}
	if err := controller.cache.AddPod(newPod); err != nil {
		t.Fatalf("add current pod to cache: %v", err)
	}

	oldPod := newPod.DeepCopy()
	oldPod.UID = "old-pod-uid"
	oldPod.OwnerReferences[0].UID = "old-job-uid"
	if err := controller.syncTask(oldPod); err != nil {
		t.Fatalf("stale pod resync should be ignored: %v", err)
	}

	jobInfo, err := controller.cache.Get(jobcache.JobKeyByName(job.Namespace, job.Name))
	if err != nil {
		t.Fatalf("get current job: %v", err)
	}
	if got := jobInfo.Pods["worker"][newPod.Name].UID; got != newPod.UID {
		t.Fatalf("stale resync replaced current pod uid %q with %q", newPod.UID, got)
	}
}

func TestDelayedActionsAreIsolatedByJobUID(t *testing.T) {
	controller := newFakeController()
	jobKey := jobcache.JobKeyByName("test", "same-name")

	oldAction := &delayAction{
		jobKey:  jobKey,
		jobUID:  types.UID("old-uid"),
		podName: "same-name-worker-0",
		action:  bus.TerminateJobAction,
		delay:   time.Hour,
	}
	newAction := &delayAction{
		jobKey:  jobKey,
		jobUID:  types.UID("new-uid"),
		podName: "same-name-worker-0",
		action:  bus.TerminateJobAction,
		delay:   time.Hour,
	}
	controller.AddDelayActionForJob(apis.Request{Namespace: "test", JobName: "same-name", JobUid: oldAction.jobUID, PodName: oldAction.podName}, oldAction)
	controller.AddDelayActionForJob(apis.Request{Namespace: "test", JobName: "same-name", JobUid: newAction.jobUID, PodName: newAction.podName}, newAction)
	defer oldAction.cancel()
	defer newAction.cancel()

	if len(controller.delayActionMap) != 2 {
		t.Fatalf("got %d delayed-action lifecycles, want 2", len(controller.delayActionMap))
	}
	if _, found := controller.delayActionMap[oldAction.lifecycleKey()]; !found {
		t.Fatalf("old lifecycle action is missing")
	}
	if _, found := controller.delayActionMap[newAction.lifecycleKey()]; !found {
		t.Fatalf("new lifecycle action is missing")
	}
}

func TestDelayedActionsAreIsolatedByPodUID(t *testing.T) {
	controller := newFakeController()
	jobKey := jobcache.JobKeyByName("test", "same-name")
	oldAction := &delayAction{
		jobKey:  jobKey,
		jobUID:  "job-uid",
		podName: "same-name-worker-0",
		podUID:  "old-pod-uid",
		action:  bus.RestartJobAction,
		delay:   time.Hour,
	}
	newAction := &delayAction{
		jobKey:  jobKey,
		jobUID:  "job-uid",
		podName: "same-name-worker-0",
		podUID:  "new-pod-uid",
		action:  bus.RestartJobAction,
		delay:   time.Hour,
	}
	controller.AddDelayActionForJob(apis.Request{Namespace: "test", JobName: "same-name", JobUid: oldAction.jobUID, PodName: oldAction.podName, PodUID: oldAction.podUID}, oldAction)
	controller.AddDelayActionForJob(apis.Request{Namespace: "test", JobName: "same-name", JobUid: newAction.jobUID, PodName: newAction.podName, PodUID: newAction.podUID}, newAction)
	defer oldAction.cancel()
	defer newAction.cancel()

	actions := controller.delayActionMap[oldAction.lifecycleKey()]
	if len(actions) != 2 {
		t.Fatalf("got %d pod lifecycle actions, want 2", len(actions))
	}
	if _, found := actions[oldAction.targetKey()]; !found {
		t.Fatal("old pod lifecycle action is missing")
	}
	if _, found := actions[newAction.targetKey()]; !found {
		t.Fatal("new pod lifecycle action is missing")
	}
}

func TestExpiredDelayedActionReentersWorkerWithUID(t *testing.T) {
	controller := newFakeController()
	action := &delayAction{
		jobKey:  jobcache.JobKeyByName("test", "same-name"),
		jobUID:  "job-uid",
		podName: "same-name-worker-0",
		action:  bus.RestartJobAction,
		delay:   time.Millisecond,
	}
	req := apis.Request{
		Namespace: "test",
		JobName:   "same-name",
		JobUid:    action.jobUID,
		PodName:   action.podName,
		PodUID:    "pod-uid",
		Event:     bus.PodFailedEvent,
	}
	queue := controller.getWorkerQueue(action.jobKey)
	defer queue.ShutDown()
	controller.AddDelayActionForJob(req, action)

	result := make(chan apis.Request, 1)
	go func() {
		obj, shutdown := queue.Get()
		if shutdown {
			return
		}
		queue.Done(obj)
		result <- obj.(apis.Request)
	}()

	select {
	case delayedReq := <-result:
		if delayedReq.JobUid != action.jobUID {
			t.Fatalf("got delayed request uid %q, want %q", delayedReq.JobUid, action.jobUID)
		}
		if delayedReq.Action != action.action {
			t.Fatalf("got delayed action %q, want %q", delayedReq.Action, action.action)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delayed action to re-enter worker queue")
	}
}
