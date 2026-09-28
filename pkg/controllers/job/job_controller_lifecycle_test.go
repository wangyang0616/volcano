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
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	bus "volcano.sh/apis/pkg/apis/bus/v1alpha1"
	"volcano.sh/apis/pkg/apis/helpers"
	scheduling "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	"volcano.sh/volcano/pkg/controllers/apis"
	jobcache "volcano.sh/volcano/pkg/controllers/cache"
	"volcano.sh/volcano/pkg/controllers/job/state"
)

func newLifecycleController(t *testing.T) *jobcontroller {
	t.Helper()
	cc := newFakeController()
	cc.cache = jobcache.New(jobcache.NewInformerReader(cc.jobLister, cc.podInformer.Informer().GetIndexer()))
	t.Cleanup(func() {
		for _, q := range cc.queueList {
			q.ShutDown()
		}
		cc.errTasks.ShutDown()
		cc.commandQueue.ShutDown()
	})
	return cc
}

func storeLifecycleJob(t *testing.T, cc *jobcontroller, job *batch.Job) {
	t.Helper()
	if err := cc.jobInformer.Informer().GetIndexer().Add(job); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverCurrentJobAfterCacheMiss(t *testing.T) {
	controller := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "test",
		Name:      "same-name",
		UID:       "new-uid",
	}}
	if err := controller.jobInformer.Informer().GetIndexer().Add(job); err != nil {
		t.Fatalf("add job to informer: %v", err)
	}
	pod := buildPod(job.Namespace, "same-name-worker-0", v1.PodRunning, nil)
	pod.UID = "pod-uid"
	pod.OwnerReferences[0].UID = job.UID
	pod.OwnerReferences[0].Name = job.Name
	addPodAnnotation(pod, map[string]string{
		batch.JobNameKey:  job.Name,
		batch.JobVersion:  "0",
		batch.TaskSpecKey: "worker",
	})
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[batch.JobNameKey] = job.Name
	if err := controller.podInformer.Informer().GetIndexer().Add(pod); err != nil {
		t.Fatalf("add pod to informer: %v", err)
	}

	staleReq := apis.Request{
		Namespace: "test",
		JobName:   "same-name",
		JobUid:    "old-uid",
		Event:     bus.PodEvictedEvent,
	}
	queue := controller.getWorkerQueue(jobcache.JobKeyByReq(&staleReq))
	defer queue.ShutDown()
	if _, err := controller.vcClient.BatchV1alpha1().Jobs(job.Namespace).Create(context.Background(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	controller.recoverCurrentJob(queue, staleReq)

	jobInfo, err := controller.cache.Get(jobcache.JobKeyByName(job.Namespace, job.Name))
	if err != nil {
		t.Fatalf("cache was not recovered from informer: %v", err)
	}
	if jobInfo.UID != job.UID {
		t.Fatalf("got recovered uid %q, want %q", jobInfo.UID, job.UID)
	}
	if got := jobInfo.Pods["worker"][pod.Name]; got == nil || got.UID != pod.UID {
		t.Fatalf("existing Pod was not recovered: %#v", got)
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

func TestAlreadyExistsWaitsForInformerBeforeRestoringPod(t *testing.T) {
	controller := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same-name", UID: "job-uid"}}
	storeLifecycleJob(t, controller, job)
	if err := controller.cache.Add(job); err != nil {
		t.Fatalf("add job: %v", err)
	}
	pod := buildPod(job.Namespace, "same-name-worker-0", v1.PodRunning, nil)
	pod.UID = "pod-uid"
	pod.OwnerReferences[0].UID = job.UID
	pod.OwnerReferences[0].Name = job.Name
	addPodAnnotation(pod, map[string]string{
		batch.JobNameKey:  job.Name,
		batch.JobVersion:  "0",
		batch.TaskSpecKey: "worker",
	})
	if _, err := controller.kubeClient.CoreV1().Pods(job.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create existing pod: %v", err)
	}

	restored, err := controller.observeExistingJobPod(job, pod.Name)
	if err != nil {
		t.Fatalf("restore existing pod: %v", err)
	}
	if restored == nil || restored.UID != pod.UID {
		t.Fatalf("got restored Pod %#v", restored)
	}
	if controller.cache.HasPod(pod) {
		t.Fatal("API GET must not populate informer-derived cache")
	}
	if err := controller.podInformer.Informer().GetIndexer().Add(pod); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.cache.RebuildLifecycle(job.Namespace, job.Name, job.UID, []string{pod.Name}); err != nil {
		t.Fatal(err)
	}
	if !controller.cache.HasPod(pod) {
		t.Fatal("informer-visible Pod not restored")
	}
}

func TestRecoverCurrentJobReplacesOldLifecycle(t *testing.T) {
	controller := newLifecycleController(t)
	oldJob := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same-name", UID: "old-uid"}}
	newJob := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same-name", UID: "new-uid"}}
	storeLifecycleJob(t, controller, oldJob)
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
	controller := newLifecycleController(t)
	oldJob := &batch.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "test", Name: "same-name", UID: "old-uid", ResourceVersion: "10",
	}}
	newJob := &batch.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "test", Name: "same-name", UID: "new-uid", ResourceVersion: "11",
	}}
	storeLifecycleJob(t, controller, oldJob)
	if err := controller.cache.Add(oldJob); err != nil {
		t.Fatalf("add old job to cache: %v", err)
	}

	storeLifecycleJob(t, controller, newJob)
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
	controller := newLifecycleController(t)
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
	if _, err := controller.vcClient.BatchV1alpha1().Jobs(job.Namespace).Create(context.Background(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

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
	controller := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same-name", UID: "new-job-uid"}}
	storeLifecycleJob(t, controller, job)
	if err := controller.cache.Add(job); err != nil {
		t.Fatalf("add current job: %v", err)
	}

	newPod := buildPod("test", "same-name-worker-0", v1.PodPending, nil)
	newPod.UID = "new-pod-uid"
	newPod.OwnerReferences[0].UID = job.UID
	newPod.OwnerReferences[0].Name = job.Name
	addPodAnnotation(newPod, map[string]string{
		batch.JobNameKey:  job.Name,
		batch.JobVersion:  "0",
		batch.TaskSpecKey: "worker",
	})
	if _, err := controller.kubeClient.CoreV1().Pods(newPod.Namespace).Create(context.Background(), newPod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create current pod: %v", err)
	}
	if err := controller.podInformer.Informer().GetIndexer().Add(newPod); err != nil {
		t.Fatal(err)
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

func TestNewJobLifecycleReplacesSameNameDelayedAction(t *testing.T) {
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

	actions := controller.delayActionMap[jobKey]
	if len(actions) != 1 {
		t.Fatalf("got %d delayed actions, want 1", len(actions))
	}
	if got := actions[newAction.podName]; got != newAction {
		t.Fatalf("got delayed action %#v, want new lifecycle action %#v", got, newAction)
	}
}

func TestNewPodLifecycleReplacesSameNameDelayedAction(t *testing.T) {
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

	actions := controller.delayActionMap[jobKey]
	if len(actions) != 1 {
		t.Fatalf("got %d delayed actions, want 1", len(actions))
	}
	if got := actions[newAction.podName]; got != newAction {
		t.Fatalf("got delayed action %#v, want new Pod lifecycle action %#v", got, newAction)
	}
}

func lifecyclePod(job *batch.Job, name string, uid types.UID) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: name, UID: uid,
		Annotations:     map[string]string{batch.JobNameKey: job.Name, batch.TaskSpecKey: "worker", batch.JobVersion: "0"},
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, helpers.JobKind)}}, Status: v1.PodStatus{Phase: v1.PodRunning}}
}

func TestRebuildDoesNotConsumePodEvictedPolicy(t *testing.T) {
	cc := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "evicted", UID: "job", ResourceVersion: "1"},
		Spec:   batch.JobSpec{Policies: []batch.LifecyclePolicy{{Events: []bus.Event{bus.PodEvictedEvent}, Action: bus.RestartJobAction}}},
		Status: batch.JobStatus{State: batch.JobState{Phase: batch.Running}}}
	storeLifecycleJob(t, cc, job)
	pod := lifecyclePod(job, "evicted-worker-0", "pod")
	// Pod informer has already removed the Pod; its Delete callback is delayed.
	if _, err := cc.cache.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
		t.Fatal(err)
	}
	cc.deletePod(pod)
	q := cc.getWorkerQueue(jobcache.JobKey(job))
	if q.Len() != 1 {
		t.Fatalf("expected delete request, got %d", q.Len())
	}
	obj, _ := q.Get()
	q.Done(obj)
	req := obj.(apis.Request)
	if req.Event != bus.PodEvictedEvent {
		t.Fatalf("eviction was suppressed by cache projection: %+v", req)
	}
	if act := applyPolicies(job, &req); act.action != bus.RestartJobAction {
		t.Fatalf("lost RestartJob policy: %+v", act)
	}
	var executed bool
	previous := state.KillJob
	state.KillJob = func(info *apis.JobInfo, phases state.PhaseMap, update state.UpdateStatusFn) error {
		executed = true
		return nil
	}
	defer func() { state.KillJob = previous }()
	q.Add(req)
	cc.processNextReq(cc.genHash(jobcache.JobKey(job)) % cc.workers)
	if !executed {
		t.Fatal("recovered eviction did not execute the Job policy")
	}
}

func TestRecoveryPreservesPolicyWhenCacheLags(t *testing.T) {
	cc := newLifecycleController(t)
	old := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "lag", UID: "old"}}
	storeLifecycleJob(t, cc, old)
	if err := cc.cache.Add(old); err != nil {
		t.Fatal(err)
	}
	current := old.DeepCopy()
	current.UID = "new"
	storeLifecycleJob(t, cc, current)
	req := apis.Request{Namespace: current.Namespace, JobName: current.Name, JobUid: current.UID, PodUID: "new-pod", Event: bus.PodFailedEvent, ExitCode: 42, TaskName: "worker"}
	q := cc.getWorkerQueue(jobcache.JobKey(current))
	cc.recoverCurrentJob(q, req)
	if q.Len() != 1 {
		t.Fatal("missing recovered policy request")
	}
	got, _ := q.Get()
	q.Done(got)
	if got.(apis.Request) != req {
		t.Fatalf("new lifecycle event was incorrectly converted to sync: %#v", got)
	}
}

func TestRecoveryWaitsWhenJobInformerLagsPod(t *testing.T) {
	cc := newLifecycleController(t)
	observed := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "lag", UID: "old"}}
	storeLifecycleJob(t, cc, observed)
	current := observed.DeepCopy()
	current.UID = "new"
	if _, err := cc.vcClient.BatchV1alpha1().Jobs(current.Namespace).Create(context.TODO(), current, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	req := apis.Request{Namespace: current.Namespace, JobName: current.Name, JobUid: current.UID, Event: bus.PodFailedEvent, ExitCode: 42}
	q := cc.getWorkerQueue(jobcache.JobKey(current))
	cc.recoverCurrentJob(q, req)
	if q.Len() != 0 || q.NumRequeues(req) != 0 || cc.cache.IsRetired(current.UID) {
		t.Fatal("observer lag was classified as stale or execution failure")
	}
	storeLifecycleJob(t, cc, current)
	cc.recoverCurrentJob(q, req)
	if q.Len() != 1 {
		t.Fatal("observation progress did not recover request")
	}
	got, _ := q.Get()
	q.Done(got)
	if got != req {
		t.Fatalf("policy was lost while waiting: %#v", got)
	}
}

func TestPodMutationPreconditions(t *testing.T) {
	for _, operation := range []string{"patch", "delete"} {
		for _, replaced := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/replaced=%v", operation, replaced), func(t *testing.T) {
				cc := newFakeController()
				job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "job", UID: "job"}}
				old := lifecyclePod(job, "pod", "old-pod")
				current := old.DeepCopy()
				if replaced {
					current.UID = "new-pod"
				}
				client := cc.kubeClient.(*kubefake.Clientset)
				if _, err := client.CoreV1().Pods(current.Namespace).Create(context.TODO(), current, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				client.PrependReactor(operation, "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
					if operation == "delete" {
						opts := a.(clienttesting.DeleteAction).GetDeleteOptions()
						if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != old.UID {
							t.Error("delete lost target UID")
						}
						return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, old.Name, fmt.Errorf("UID precondition"))
					}
					var patch []map[string]interface{}
					if err := json.Unmarshal(a.(clienttesting.PatchAction).GetPatch(), &patch); err != nil {
						t.Error(err)
					}
					if len(patch) < 2 || patch[0]["op"] != "test" || patch[0]["path"] != "/metadata/uid" || patch[0]["value"] != string(old.UID) {
						t.Error("patch lost target UID test")
					}
					return true, nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, old.Name, field.ErrorList{field.Invalid(field.NewPath("metadata", "uid"), old.UID, "test failed")})
				})
				var err error
				if operation == "patch" {
					err = cc.markPodOutOfSync(old)
				} else {
					err = cc.deleteJobPod(job.Name, old)
				}
				if (err == nil) != replaced {
					t.Fatalf("must ignore only confirmed replacement: replaced=%v, err=%v", replaced, err)
				}
				got, err := client.CoreV1().Pods(current.Namespace).Get(context.TODO(), current.Name, metav1.GetOptions{})
				if err != nil || got.UID != current.UID {
					t.Fatal("mutated replacement Pod")
				}
			})
		}
	}
}

func TestRestartPodRetainsTriggerUID(t *testing.T) {
	cc := newFakeController()
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "job", UID: "job", ResourceVersion: "1"}}
	if err := cc.cache.Add(job); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.vcClient.BatchV1alpha1().Jobs(job.Namespace).Create(context.TODO(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	current := lifecyclePod(job, "pod", "new-pod")
	info := &apis.JobInfo{Namespace: job.Namespace, Name: job.Name, UID: job.UID, Job: job, Pods: map[string]map[string]*v1.Pod{"worker": {current.Name: current}}}
	action := GetStateAction(&delayAction{action: bus.RestartPodAction, taskName: "worker", podName: current.Name, podUID: "old-pod"})
	if err := cc.killTarget(info, action.Target, nil); err != nil {
		t.Fatal(err)
	}
	for _, a := range cc.kubeClient.(*kubefake.Clientset).Actions() {
		if a.GetResource().Resource == "pods" {
			t.Fatalf("old Pod action operated on replacement: %#v", a)
		}
	}
	q := cc.getWorkerQueue(jobcache.JobKey(job))
	defer q.ShutDown()
	if q.Len() != 1 {
		t.Fatal("stale target consumed the only replica-repair wakeup")
	}
	queued, _ := q.Get()
	q.Done(queued)
	if queued != jobSyncRequest(job) {
		t.Fatalf("expected canonical replica repair, got %#v", queued)
	}
}

type countedJobReader struct {
	jobcache.SnapshotReader
	lists atomic.Int32
}

func (r *countedJobReader) ListPods(ns, name string, uid types.UID) ([]*v1.Pod, error) {
	r.lists.Add(1)
	return r.SnapshotReader.ListPods(ns, name, uid)
}

func TestAlreadyExistsBatchRebuildsOnceFor5000Pods(t *testing.T) {
	cc := newLifecycleController(t)
	reader := &countedJobReader{SnapshotReader: jobcache.NewInformerReader(cc.jobLister, cc.podInformer.Informer().GetIndexer())}
	cc.cache = jobcache.New(reader)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "large", UID: "job", ResourceVersion: "1"},
		Spec:   batch.JobSpec{Queue: "default", MinAvailable: 5000, Tasks: []batch.TaskSpec{{Name: "worker", Replicas: 5000}}},
		Status: batch.JobStatus{State: batch.JobState{Phase: batch.Running}}}
	storeLifecycleJob(t, cc, job)
	if _, err := cc.cache.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
		t.Fatal(err)
	}
	info, err := cc.cache.GetForUID(jobcache.JobKey(job), job.UID)
	if err != nil {
		t.Fatal(err)
	}
	reader.lists.Store(0)
	for i := range 5000 {
		pod := lifecyclePod(job, fmt.Sprintf("large-worker-%d", i), types.UID(fmt.Sprintf("pod-%d", i)))
		if err := cc.podInformer.Informer().GetIndexer().Add(pod); err != nil {
			t.Fatal(err)
		}
	}
	queue := &scheduling.Queue{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	if err := cc.queueInformer.Informer().GetIndexer().Add(queue); err != nil {
		t.Fatal(err)
	}
	pg := &scheduling.PodGroup{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: cc.generateRelatedPodGroupName(job), UID: "pg", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, helpers.JobKind)}},
		Status: scheduling.PodGroupStatus{Phase: scheduling.PodGroupRunning}}
	if err := cc.pgInformer.Informer().GetIndexer().Add(pg); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.vcClient.SchedulingV1beta1().PodGroups(job.Namespace).Create(context.TODO(), pg, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var creates atomic.Int32
	cc.kubeClient.(*kubefake.Clientset).PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		creates.Add(1)
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, a.(clienttesting.CreateAction).GetObject().(*v1.Pod).Name)
	})
	if err := cc.syncJob(info, nil); !errors.Is(err, errJobObservationPending) {
		t.Fatalf("expected observation wait, got %v", err)
	}
	if creates.Load() != 5000 || reader.lists.Load() != 1 {
		t.Fatalf("creates=%d rebuilds=%d", creates.Load(), reader.lists.Load())
	}
	got, err := cc.cache.GetForUID(jobcache.JobKey(job), job.UID)
	if err != nil || len(got.Pods["worker"]) != 5000 {
		t.Fatal("batch recovery incomplete", err)
	}
	for _, a := range cc.vcClient.(*vcfake.Clientset).Actions() {
		if a.GetSubresource() == "status" {
			t.Fatal("published status from incomplete snapshot")
		}
	}
	// A subsequent real GET error must not reset the canonical request's
	// execution retry counter through the observation-wait path.
	for _, pod := range got.Pods["worker"] {
		if err := cc.podInformer.Informer().GetIndexer().Delete(pod); err != nil {
			t.Fatal(err)
		}
	}
	cc.kubeClient.(*kubefake.Clientset).PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, a.(clienttesting.GetAction).GetName(), fmt.Errorf("denied"))
	})
	req := jobSyncRequest(job)
	q := cc.getWorkerQueue(jobcache.JobKey(job))
	q.AddRateLimited(req)
	if err := cc.syncJob(info, nil); err == nil {
		t.Fatal("GET permission failure was swallowed")
	}
	if q.NumRequeues(req) != 1 {
		t.Fatal("observation wait erased real execution retries")
	}
}

func TestNewTimerSurvivesOldTimerCleanup(t *testing.T) {
	cc := newFakeController()
	old := &delayAction{jobKey: "test/job", jobUID: "job", taskName: "worker", podName: "pod", podUID: "old", action: bus.RestartJobAction, cancel: func() {}}
	current := &delayAction{jobKey: old.jobKey, jobUID: old.jobUID, taskName: old.taskName, podName: old.podName, podUID: "new", action: old.action, cancel: func() { t.Error("new timer canceled by old timer") }}
	cc.delayActionMap[old.jobKey] = map[string]*delayAction{old.podName: current}
	cc.removeDelayAction(old)
	cc.cleanupDelayActions(old)
	if cc.delayActionMap[old.jobKey][old.podName] != current {
		t.Fatal("old timer removed its replacement")
	}
}

func TestReplacementRunningKeepsExistingDelayedPolicyCancellation(t *testing.T) {
	for _, event := range []bus.Event{bus.PodPendingEvent, bus.PodFailedEvent, bus.PodEvictedEvent} {
		t.Run(string(event), func(t *testing.T) {
			cc := newFakeController()
			canceled := false
			old := &delayAction{jobKey: "test/job", jobUID: "job", podName: "pod", podUID: "old", event: event, action: bus.RestartJobAction, cancel: func() { canceled = true }}
			cc.delayActionMap[old.jobKey] = map[string]*delayAction{old.podName: old}
			current := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "pod", UID: "new"}, Status: v1.PodStatus{Phase: v1.PodRunning}}
			if err := cc.podInformer.Informer().GetIndexer().Add(current); err != nil {
				t.Fatal(err)
			}
			cc.CleanPodDelayActionsIfNeed(apis.Request{Namespace: "test", JobName: "job", JobUid: old.jobUID, PodName: old.podName, PodUID: "new", Event: bus.PodRunningEvent})
			if canceled != (event != bus.PodPendingEvent) {
				t.Fatalf("changed cancellation semantics for %s", event)
			}
		})
	}
}

func TestStaleRunningEventDoesNotCancelNewFailureTimeout(t *testing.T) {
	cc := newFakeController()
	current := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "pod", UID: "new"}, Status: v1.PodStatus{Phase: v1.PodFailed}}
	if err := cc.podInformer.Informer().GetIndexer().Add(current); err != nil {
		t.Fatal(err)
	}
	act := &delayAction{jobKey: "test/job", jobUID: "job", podName: "pod", podUID: "new", event: bus.PodFailedEvent, action: bus.RestartJobAction, cancel: func() { t.Error("stale Running canceled a new failure timeout") }}
	cc.delayActionMap[act.jobKey] = map[string]*delayAction{act.podName: act}
	for _, uid := range []types.UID{"old", "new"} {
		cc.CleanPodDelayActionsIfNeed(apis.Request{Namespace: "test", JobName: "job", JobUid: "job", PodName: "pod", PodUID: uid, Event: bus.PodRunningEvent})
		if cc.delayActionMap[act.jobKey][act.podName] != act {
			t.Fatal("failure timeout removed")
		}
	}
}

func TestDelayActionLifecycleAndVersionValidation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		uid           types.UID
		version       int32
		event         bus.Event
		podUID        types.UID
		execute, fail bool
	}{
		{name: "old Job", uid: "old", version: 2, event: bus.PodFailedEvent},
		{name: "old version", uid: "current", version: 1, event: bus.PodFailedEvent},
		{name: "replaced pending Pod", uid: "current", version: 2, event: bus.PodPendingEvent, podUID: "old-pod"},
		{name: "current pending Pod", uid: "current", version: 2, event: bus.PodPendingEvent, podUID: "pod", execute: true},
		{name: "failure survives Pod removal", uid: "current", version: 2, event: bus.PodFailedEvent, podUID: "removed", execute: true},
		{name: "failed action retains retry", uid: "current", version: 2, event: bus.PodFailedEvent, execute: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc := newFakeController()
			cc.maxRequeueNum = 2
			t.Cleanup(func() {
				for _, q := range cc.queueList {
					q.ShutDown()
				}
			})
			job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "job", UID: "current", ResourceVersion: "1"}, Status: batch.JobStatus{State: batch.JobState{Phase: batch.Running}, Version: 2}}
			if err := cc.cache.Add(job); err != nil {
				t.Fatal(err)
			}
			pod := lifecyclePod(job, "pod", "pod")
			pod.Status.Phase = v1.PodPending
			if err := cc.cache.AddPod(pod); err != nil {
				t.Fatal(err)
			}
			act := &delayAction{jobKey: "test/job", jobUID: tc.uid, jobVersion: tc.version, taskName: "worker", podName: "pod", podUID: tc.podUID, event: tc.event, action: bus.RestartJobAction, cancel: func() {}}
			cc.delayActionMap[act.jobKey] = map[string]*delayAction{act.podName: act}
			req := apis.Request{Namespace: "test", JobName: "job", JobUid: tc.uid, JobVersion: tc.version, TaskName: "worker", PodName: "pod", PodUID: tc.podUID, Event: tc.event}
			var executed bool
			previous := state.KillJob
			state.KillJob = func(info *apis.JobInfo, phases state.PhaseMap, update state.UpdateStatusFn) error {
				executed = true
				if tc.fail {
					return fmt.Errorf("temporary execution failure")
				}
				return nil
			}
			defer func() { state.KillJob = previous }()
			cc.executeDelayAction(req, act)
			if executed != tc.execute {
				t.Fatalf("executed=%v, want %v", executed, tc.execute)
			}
			if tc.fail && cc.getWorkerQueue(act.jobKey).NumRequeues(req) != 1 {
				t.Fatal("timer forgot a real execution failure")
			}
		})
	}
}
