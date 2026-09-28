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
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	bus "volcano.sh/apis/pkg/apis/bus/v1alpha1"
	"volcano.sh/apis/pkg/apis/helpers"
	"volcano.sh/volcano/pkg/controllers/apis"
	jobcache "volcano.sh/volcano/pkg/controllers/cache"
	"volcano.sh/volcano/pkg/controllers/job/state"
)

// Record observation waits without depending on a wall-clock timeout.
type observationQueue struct {
	workqueue.TypedRateLimitingInterface[any]
	waiting []any
}

func (q *observationQueue) AddAfter(item any, _ time.Duration) {
	q.waiting = append(q.waiting, item)
}

type retiringObservationCache struct {
	jobcache.Cache
	beforeCheck func()
}

func (c *retiringObservationCache) IsRetired(uid types.UID) bool {
	if c.beforeCheck != nil {
		fn := c.beforeCheck
		c.beforeCheck = nil
		fn()
	}
	return c.Cache.IsRetired(uid)
}

func TestRecoveryPreservesNewPolicyWhenObservationRetires(t *testing.T) {
	cc := newLifecycleController(t)
	old := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same", UID: "old", ResourceVersion: "1"}}
	current := old.DeepCopy()
	current.UID = "new"
	current.Status.State.Phase = batch.Running
	current.Spec.Policies = []batch.LifecyclePolicy{{ExitCode: ptr.To(int32(42)), Action: bus.RestartJobAction}}
	storeLifecycleJob(t, cc, old)
	if err := cc.cache.Add(old); err != nil {
		t.Fatal(err)
	}
	original := cc.cache
	cc.cache = &retiringObservationCache{Cache: original, beforeCheck: func() {
		// The Job informer and its handler advance after the worker's Get.
		storeLifecycleJob(t, cc, current)
		if err := original.Delete(old); err != nil {
			t.Fatal(err)
		}
	}}
	req := apis.Request{Namespace: current.Namespace, JobName: current.Name, JobUid: current.UID, Event: bus.PodFailedEvent, ExitCode: 42}
	q := &observationQueue{TypedRateLimitingInterface: cc.getWorkerQueue(jobcache.JobKey(current))}
	cc.recoverCurrentJob(q, req)
	if len(q.waiting) != 1 || q.waiting[0] != req {
		t.Fatal("new lifecycle's original policy was lost when the old observation retired")
	}
	cc.recoverCurrentJob(q, req)
	if q.Len() != 1 {
		t.Fatal("observation progress did not restore the policy request")
	}
	executed := false
	previous := state.KillJob
	state.KillJob = func(info *apis.JobInfo, _ state.PhaseMap, _ state.UpdateStatusFn) error {
		executed = info.UID == current.UID
		return nil
	}
	defer func() { state.KillJob = previous }()
	cc.processNextReq(cc.genHash(jobcache.JobKey(current)) % cc.workers)
	if !executed {
		t.Fatal("recovered request did not execute the successor's exit-code policy")
	}
}

func TestObservationWaitPreservesExecutionRetries(t *testing.T) {
	cc := newLifecycleController(t)
	cc.maxRequeueNum = 5
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "retries", UID: "job", ResourceVersion: "1"}, Status: batch.JobStatus{State: batch.JobState{Phase: batch.Running}}}
	storeLifecycleJob(t, cc, job)
	if _, err := cc.cache.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
		t.Fatal(err)
	}
	req := jobSyncRequest(job)
	worker := cc.genHash(jobcache.JobKey(job)) % cc.workers
	cc.queueList[worker].ShutDown()
	// Drive retries explicitly; actual rate-limit timers cannot race the test.
	q := &observationQueue{TypedRateLimitingInterface: workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedItemExponentialFailureRateLimiter[any](time.Hour, time.Hour))}
	cc.queueList[worker] = q
	previous := state.SyncJob
	defer func() { state.SyncJob = previous }()
	for _, step := range []struct {
		err   error
		count int
	}{
		{fmt.Errorf("delete denied"), 1},
		{errJobObservationPending, 1},
		{fmt.Errorf("delete still denied"), 2},
		{nil, 0},
	} {
		state.SyncJob = func(*apis.JobInfo, state.UpdateStatusFn) error { return step.err }
		q.Add(req)
		cc.processNextReq(worker)
		if got := q.NumRequeues(req); got != step.count {
			t.Fatalf("after %v: retries=%d, want %d", step.err, got, step.count)
		}
	}
	if len(q.waiting) != 1 || q.waiting[0] != req {
		t.Fatal("observation wait lost the original request")
	}
	q.AddRateLimited(req)
	pod := lifecyclePod(job, "retries-worker-0", "pod")
	if err := cc.syncTask(pod); err != nil {
		t.Fatal(err)
	}
	if q.NumRequeues(req) != 1 {
		t.Fatal("task recovery erased the Job worker's real failure")
	}
	cc.recoverCurrentJob(q, req)
	if q.NumRequeues(req) != 1 {
		t.Fatal("cache recovery acknowledged an execution that has not succeeded")
	}
}

type actionRecorder struct{ actions []state.Action }

func (r *actionRecorder) Execute(action state.Action) error {
	r.actions = append(r.actions, action)
	return nil
}

func TestSSHConflictWaitsWithoutTerminatingJob(t *testing.T) {
	cc := newLifecycleController(t)
	cc.maxRequeueNum = 0
	old := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same", UID: "old"}}
	current := old.DeepCopy()
	current.UID = "new"
	current.Spec.Plugins = map[string][]string{"ssh": {}}
	secret := &v1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: old.Namespace, Name: old.Name + "-ssh", UID: "old-secret", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(old, helpers.JobKind)}}}
	if _, err := cc.kubeClient.CoreV1().Secrets(old.Namespace).Create(context.TODO(), secret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	err := cc.pluginOnJobAdd(current)
	var conflict *helpers.JobResourceConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("SSH lost the conflict type: %v", err)
	}
	req := jobSyncRequest(current)
	q := &observationQueue{TypedRateLimitingInterface: cc.getWorkerQueue(jobcache.JobKey(current))}
	st := &actionRecorder{}
	cc.handleJobError(q, req, st, err, bus.SyncJobAction)
	if len(st.actions) != 0 || q.NumRequeues(req) != 0 || len(q.waiting) != 1 || q.waiting[0] != req {
		t.Fatal("name conflict exhausted execution retries or lost recovery")
	}
	if err := cc.kubeClient.CoreV1().Secrets(old.Namespace).Delete(context.TODO(), secret.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := cc.pluginOnJobAdd(current); err != nil {
		t.Fatal(err)
	}
	got, err := cc.kubeClient.CoreV1().Secrets(current.Namespace).Get(context.TODO(), secret.Name, metav1.GetOptions{})
	if err != nil || !helpers.IsControlledByJob(got, current) {
		t.Fatalf("SSH did not recover after name release: %v", err)
	}
}

func TestPluginInitializationDoesNotMutateCachedJob(t *testing.T) {
	cc := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "initializing", UID: "job", ResourceVersion: "1"},
		Spec: batch.JobSpec{Plugins: map[string][]string{"svc": {}}}, Status: batch.JobStatus{ControlledResources: map[string]string{}}}
	storeLifecycleJob(t, cc, job)
	if err := cc.cache.Add(job); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.vcClient.BatchV1alpha1().Jobs(job.Namespace).Create(context.TODO(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	instance, err := cc.initJobStatus(job.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}
	// PodGroup callbacks / delayed actions may read the cache during plugin
	// initialization. Under -race this also protects the API-response boundary.
	var readers sync.WaitGroup
	readers.Go(func() {
		for range 1000 {
			if _, err := cc.cache.Get(jobcache.JobKey(job)); err != nil {
				t.Error(err)
			}
		}
	})
	defer readers.Wait()
	for range 10 {
		if err := cc.pluginOnJobAdd(instance); err != nil {
			t.Fatal(err)
		}
		cached, err := cc.cache.Get(jobcache.JobKey(job))
		if err != nil {
			t.Fatal(err)
		}
		if len(cached.Job.Status.ControlledResources) != 0 {
			t.Fatalf("plugin wrote cache without UpdateStatus: %#v", cached.Job.Status.ControlledResources)
		}
		if err := cc.pluginOnJobDelete(instance); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRayServiceLifecycleIsolation(t *testing.T) {
	cc := newLifecycleController(t)
	old := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same", UID: "old"},
		Spec: batch.JobSpec{Plugins: map[string][]string{"ray": {}}}, Status: batch.JobStatus{ControlledResources: map[string]string{"plugin-ray": "ray"}}}
	current := old.DeepCopy()
	current.UID = "new"
	current.Status.ControlledResources = nil
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: old.Namespace, Name: old.Name + "-head-svc", UID: "old-service", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(old, helpers.JobKind)}}}
	if _, err := cc.kubeClient.CoreV1().Services(old.Namespace).Create(context.TODO(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var conflict *helpers.JobResourceConflictError
	if err := cc.pluginOnJobAdd(current); !errors.As(err, &conflict) {
		t.Fatalf("Ray reused an old lifecycle's Service: %v", err)
	}
	if err := cc.kubeClient.CoreV1().Services(old.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := cc.pluginOnJobAdd(current); err != nil {
		t.Fatal(err)
	}
	created, err := cc.kubeClient.CoreV1().Services(current.Namespace).Get(context.TODO(), svc.Name, metav1.GetOptions{})
	if err != nil || !helpers.IsControlledByJob(created, current) {
		t.Fatalf("Ray did not recover after name release: %v", err)
	}
	// The fake API does not assign UIDs. Assign one to verify the delete guard.
	created.UID = "new-service"
	if _, err := cc.kubeClient.CoreV1().Services(current.Namespace).Update(context.TODO(), created, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	cc.kubeClient.(*kubefake.Clientset).PrependReactor("delete", "services", func(a clienttesting.Action) (bool, runtime.Object, error) {
		deletes++
		preconditions := a.(clienttesting.DeleteAction).GetDeleteOptions().Preconditions
		if preconditions == nil || preconditions.UID == nil || *preconditions.UID != created.UID {
			t.Fatal("Ray delete is not conditional on the observed Service UID")
		}
		return false, nil, nil
	})
	if err := cc.pluginOnJobDelete(old); err != nil {
		t.Fatal(err)
	}
	if deletes != 0 {
		t.Fatal("old lifecycle deleted the new head Service")
	}
	if err := cc.pluginOnJobDelete(current); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.kubeClient.CoreV1().Services(current.Namespace).Get(context.TODO(), svc.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) || deletes != 1 {
		t.Fatal("current lifecycle could not delete its Service")
	}
}
