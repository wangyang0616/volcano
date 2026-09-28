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
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/utils/ptr"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	bus "volcano.sh/apis/pkg/apis/bus/v1alpha1"
	"volcano.sh/apis/pkg/apis/helpers"
	scheduling "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	"volcano.sh/volcano/pkg/controllers/apis"
	jobcache "volcano.sh/volcano/pkg/controllers/cache"
	"volcano.sh/volcano/pkg/controllers/job/state"
	"volcano.sh/volcano/pkg/features"
)

// Use production informer projection and state actions; control only event
// delivery order, rather than timing a background informer listener.
func terminalObservationFixture(t *testing.T, replicas int32, policies []batch.LifecyclePolicy) (*jobcontroller, *batch.Job, []*v1.Pod) {
	t.Helper()
	cc := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "terminal", UID: "job", ResourceVersion: "1"},
		Spec:   batch.JobSpec{Queue: "default", MinAvailable: replicas, MaxRetry: 3, Tasks: []batch.TaskSpec{{Name: "worker", Replicas: replicas}}, Policies: policies},
		Status: batch.JobStatus{State: batch.JobState{Phase: batch.Running}, Running: replicas, MinAvailable: replicas}}
	storeLifecycleJob(t, cc, job)
	if _, err := cc.vcClient.BatchV1alpha1().Jobs(job.Namespace).Create(context.TODO(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := cc.queueInformer.Informer().GetIndexer().Add(&scheduling.Queue{ObjectMeta: metav1.ObjectMeta{Name: "default"}}); err != nil {
		t.Fatal(err)
	}
	pg := &scheduling.PodGroup{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: cc.generateRelatedPodGroupName(job), UID: "pg", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, helpers.JobKind)}}, Status: scheduling.PodGroupStatus{Phase: scheduling.PodGroupRunning}}
	if err := cc.pgInformer.Informer().GetIndexer().Add(pg); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.vcClient.SchedulingV1beta1().PodGroups(job.Namespace).Create(context.TODO(), pg, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pods := make([]*v1.Pod, replicas)
	for i := range pods {
		pod := lifecyclePod(job, fmt.Sprintf("terminal-worker-%d", i), types.UID(fmt.Sprintf("pod-%d", i)))
		pod.Status.Phase, pod.ResourceVersion = v1.PodRunning, "1"
		pods[i] = pod
		if err := cc.podInformer.Informer().GetIndexer().Add(pod); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cc.cache.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
		t.Fatal(err)
	}
	return cc, job, pods
}

func assertObservationPhase(t *testing.T, cc *jobcontroller, job *batch.Job, phase batch.JobPhase) *apis.JobInfo {
	t.Helper()
	info, err := cc.cache.GetForUID(jobcache.JobKey(job), job.UID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Job.Status.State.Phase != phase {
		t.Fatalf("phase=%s, want %s; status=%+v", info.Job.Status.State.Phase, phase, info.Job.Status)
	}
	return info
}

func TestTerminalProjectionWaitsForAllObservations(t *testing.T) {
	for _, phase := range []v1.PodPhase{v1.PodSucceeded, v1.PodFailed} {
		for _, initialList := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/initialList=%v", phase, initialList), func(t *testing.T) {
				cc, job, pods := terminalObservationFixture(t, 2, nil)
				worker := cc.genHash(jobcache.JobKey(job)) % cc.workers
				terminal := make([]*v1.Pod, len(pods))
				for i, pod := range pods {
					terminal[i] = pod.DeepCopy()
					terminal[i].Status.Phase, terminal[i].ResourceVersion = phase, "3"
					if err := cc.podInformer.Informer().GetIndexer().Update(terminal[i]); err != nil {
						t.Fatal(err)
					}
					// Earlier metadata callback sees latest terminal store objects.
					metadata := pod.DeepCopy()
					metadata.ResourceVersion = "2"
					cc.updatePod(pod, metadata)
					cc.processNextReq(worker)
					assertObservationPhase(t, cc, job, batch.Running)
				}
				for i, pod := range terminal {
					if initialList {
						cc.addPod(pod)
					} else {
						cc.updatePod(pods[i], pod)
					}
					cc.processNextReq(worker)
					if i == 0 {
						assertObservationPhase(t, cc, job, batch.Running)
					}
				}
				want := batch.Completed
				if phase == v1.PodFailed {
					want = batch.Failed
				}
				info := assertObservationPhase(t, cc, job, want)
				if len(info.HandledTerminalPods) != len(pods) {
					t.Fatal("successful observations were not acknowledged")
				}
			})
		}
	}
}

func TestTerminalInitialListSurvivesCacheRecovery(t *testing.T) {
	for _, phase := range []v1.PodPhase{v1.PodFailed, v1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			cc, job, pods := terminalObservationFixture(t, 1, []batch.LifecyclePolicy{{Event: bus.PodPendingEvent,
				Action: bus.RestartJobAction, Timeout: &metav1.Duration{Duration: time.Hour}}})
			// Restart/relist: the Pod Add arrives before the Job cache is ready.
			cc.cache = jobcache.New(jobcache.NewInformerReader(cc.jobLister, cc.podInformer.Informer().GetIndexer()))
			pod := pods[0].DeepCopy()
			pod.Status.Phase, pod.ResourceVersion = phase, "2"
			if err := cc.podInformer.Informer().GetIndexer().Update(pod); err != nil {
				t.Fatal(err)
			}
			cc.addPod(pod)
			worker := cc.genHash(jobcache.JobKey(job)) % cc.workers
			cc.processNextReq(worker) // recover, preserving the terminal flag
			if cc.queueList[worker].Len() != 1 {
				t.Fatal("recovery lost the terminal Add request")
			}
			cc.processNextReq(worker)
			want := batch.Completed
			if phase == v1.PodFailed {
				want = batch.Failed
			}
			assertObservationPhase(t, cc, job, want)
			cc.delayActionMapLock.RLock()
			pendingTimers := len(cc.delayActionMap)
			cc.delayActionMapLock.RUnlock()
			if pendingTimers != 0 {
				t.Fatal("terminal initial-list Pod incorrectly scheduled a Pending policy")
			}
		})
	}
}

func TestTerminalPolicyFenceSurvivesTimeoutAndMetadata(t *testing.T) {
	for _, phase := range []v1.PodPhase{v1.PodFailed, v1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			event := bus.PodFailedEvent
			if phase == v1.PodSucceeded {
				event = bus.TaskCompletedEvent
			}
			cc, job, pods := terminalObservationFixture(t, 1, []batch.LifecyclePolicy{{Event: event, Action: bus.RestartJobAction, Timeout: &metav1.Duration{Duration: time.Hour}}})
			job.Spec.MinSuccess = ptr.To(int32(1))
			if err := cc.cache.Update(job); err != nil {
				t.Fatal(err)
			}
			pod := pods[0].DeepCopy()
			pod.Status.Phase, pod.ResourceVersion = phase, "2"
			if err := cc.podInformer.Informer().GetIndexer().Update(pod); err != nil {
				t.Fatal(err)
			}
			cc.updatePod(pods[0], pod)
			key := jobcache.JobKey(job)
			worker := cc.genHash(key) % cc.workers
			cc.processNextReq(worker)
			cc.delayActionMapLock.RLock()
			delayed := cc.delayActionMap[key][pod.Name]
			cc.delayActionMapLock.RUnlock()
			if delayed == nil {
				t.Fatal("terminal policy timeout was not scheduled")
			}
			defer delayed.cancel()
			metadata := pod.DeepCopy()
			metadata.ResourceVersion = "3"
			if err := cc.podInformer.Informer().GetIndexer().Update(metadata); err != nil {
				t.Fatal(err)
			}
			cc.updatePod(pod, metadata)
			cc.processNextReq(worker)
			info := assertObservationPhase(t, cc, job, batch.Running)
			if len(info.HandledTerminalPods) != 0 {
				t.Fatal("metadata callback acknowledged a still-delayed policy")
			}
			cc.executeDelayAction(apis.Request{Namespace: job.Namespace, JobName: job.Name, JobUid: job.UID, TaskName: "worker", PodName: pod.Name, PodUID: pod.UID, Event: event, TerminalObservation: true}, delayed)
			info = assertObservationPhase(t, cc, job, batch.Restarting)
			if info.Job.Status.RetryCount != 1 || len(info.HandledTerminalPods) != 1 {
				t.Fatal("timeout did not execute and acknowledge its original policy")
			}
		})
	}
}

func TestFailedPolicyExecutionDoesNotAcknowledgeTerminalPod(t *testing.T) {
	cc, job, pods := terminalObservationFixture(t, 1, []batch.LifecyclePolicy{{Event: bus.PodFailedEvent, Action: bus.RestartJobAction}})
	cc.maxRequeueNum = 3
	worker := cc.genHash(jobcache.JobKey(job)) % cc.workers
	cc.queueList[worker].ShutDown()
	q := workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedItemExponentialFailureRateLimiter[any](time.Hour, time.Hour))
	cc.queueList[worker] = q
	pod := pods[0].DeepCopy()
	pod.Status.Phase, pod.ResourceVersion = v1.PodFailed, "2"
	if err := cc.podInformer.Informer().GetIndexer().Update(pod); err != nil {
		t.Fatal(err)
	}
	cc.updatePod(pods[0], pod)
	// Save the exact callback request for an explicit retry without timer races.
	item, _ := q.Get()
	q.Done(item)
	q.Add(item)
	previous := state.KillJob
	defer func() { state.KillJob = previous }()
	state.KillJob = func(*apis.JobInfo, state.PhaseMap, state.UpdateStatusFn) error { return errors.New("delete denied") }
	cc.processNextReq(worker)
	q.Add(jobSyncRequest(job))
	cc.processNextReq(worker)
	info := assertObservationPhase(t, cc, job, batch.Running)
	if len(info.HandledTerminalPods) != 0 || q.NumRequeues(item) != 1 {
		t.Fatal("failed policy released its fence or lost its retry count")
	}
	state.KillJob = previous
	q.Add(item)
	cc.processNextReq(worker)
	assertObservationPhase(t, cc, job, batch.Restarting)
}

func TestCoalescedTerminalTimeoutsDoNotLeaveCompletionBlocked(t *testing.T) {
	cc, job, pods := terminalObservationFixture(t, 2, []batch.LifecyclePolicy{{Event: bus.PodFailedEvent,
		Action: bus.ResumeJobAction, Timeout: &metav1.Duration{Duration: time.Hour}}})
	key := jobcache.JobKey(job)
	worker := cc.genHash(key) % cc.workers
	for _, initial := range pods {
		pod := initial.DeepCopy()
		pod.Status.Phase, pod.ResourceVersion = v1.PodFailed, "2"
		if err := cc.podInformer.Informer().GetIndexer().Update(pod); err != nil {
			t.Fatal(err)
		}
		cc.updatePod(initial, pod)
		cc.processNextReq(worker)
	}
	cc.delayActionMapLock.RLock()
	delayed := cc.delayActionMap[key][pods[0].Name]
	cc.delayActionMapLock.RUnlock()
	if delayed == nil {
		t.Fatal("terminal timeout missing")
	}
	// Resume is allowed in policies and falls back to sync for a Running Job.
	// The existing coalescing rule cancels the other Job-level timeout.
	cc.executeDelayAction(apis.Request{Namespace: job.Namespace, JobName: job.Name, JobUid: job.UID,
		TaskName: "worker", PodName: pods[0].Name, PodUID: pods[0].UID, Event: bus.PodFailedEvent}, delayed)
	if cc.queueList[worker].Len() == 0 {
		t.Fatal("coalesced observations did not request terminal reconciliation")
	}
	cc.processNextReq(worker)
	assertObservationPhase(t, cc, job, batch.Failed)
	cc.delayActionMapLock.RLock()
	remaining := len(cc.delayActionMap[key])
	cc.delayActionMapLock.RUnlock()
	if remaining != 0 {
		t.Fatal("coalesced timers leaked")
	}
}

func TestMissingJobRequestsRetireUnknownUIDOnce(t *testing.T) {
	cc := newLifecycleController(t)
	req := apis.Request{Namespace: "test", JobName: "missing", JobUid: "unseen", Event: bus.PodEvictedEvent}
	q := cc.getWorkerQueue(jobcache.JobKeyByReq(&req))
	for i := range 5000 {
		req.PodName = fmt.Sprintf("missing-worker-%d", i)
		req.PodUID = types.UID(fmt.Sprintf("pod-%d", i))
		cc.recoverCurrentJob(q, req)
	}
	gets := 0
	for _, a := range cc.vcClient.(*vcfake.Clientset).Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "jobs" {
			gets++
		}
	}
	if gets != 1 {
		t.Fatalf("5000 requests for one deleted unknown UID made %d live GETs, want 1", gets)
	}
}

func TestPendingInitializationPersistsCompletedPluginsBeforeLaterError(t *testing.T) {
	cc := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "partial", UID: "job", ResourceVersion: "1"},
		Spec:   batch.JobSpec{Plugins: map[string][]string{"svc": {}}, Volumes: []batch.VolumeSpec{{VolumeClaimName: "missing-pvc"}}},
		Status: batch.JobStatus{State: batch.JobState{Phase: batch.Pending}}}
	storeLifecycleJob(t, cc, job)
	if err := cc.cache.Add(job); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.vcClient.BatchV1alpha1().Jobs(job.Namespace).Create(context.TODO(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.initiateJob(job.DeepCopy()); err == nil {
		t.Fatal("expected missing PVC error after plugin success")
	}
	live, err := cc.vcClient.BatchV1alpha1().Jobs(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{})
	if err != nil || live.Status.ControlledResources["plugin-svc"] != "svc" {
		t.Fatalf("completed plugin was lost on later initialization error: %v", err)
	}
	if err := cc.pluginOnJobDelete(live); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.kubeClient.CoreV1().Services(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("partial initialization cleanup leaked Service: %v", err)
	}
}

func TestServiceInitializationDoesNotPersistPrematureCompletion(t *testing.T) {
	cc := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "svc-retry", UID: "job", ResourceVersion: "1"},
		Spec: batch.JobSpec{Plugins: map[string][]string{"svc": {}}}, Status: batch.JobStatus{State: batch.JobState{Phase: batch.Pending}}}
	storeLifecycleJob(t, cc, job)
	if err := cc.cache.Add(job); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.vcClient.BatchV1alpha1().Jobs(job.Namespace).Create(context.TODO(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deny := true
	cc.kubeClient.(*kubefake.Clientset).PrependReactor("create", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		if deny {
			return true, nil, errors.New("network policy denied")
		}
		return false, nil, nil
	})
	if _, err := cc.initiateJob(job.DeepCopy()); err == nil {
		t.Fatal("expected NetworkPolicy create failure")
	}
	live, err := cc.vcClient.BatchV1alpha1().Jobs(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{})
	if err != nil || live.Status.ControlledResources["plugin-svc"] != "" {
		t.Fatalf("partially completed plugin was marked ready: %v", err)
	}
	if err := cc.pluginOnJobDelete(live.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.kubeClient.CoreV1().Services(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("partial plugin cleanup left Service behind: %v", err)
	}
	deny = false
	ready, err := cc.initiateJob(live)
	if err != nil || ready.Status.ControlledResources["plugin-svc"] != "svc" {
		t.Fatalf("plugin retry did not complete: %v", err)
	}
	if _, err := cc.kubeClient.NetworkingV1().NetworkPolicies(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("plugin retry skipped missing NetworkPolicy: %v", err)
	}
	client := cc.vcClient.(*vcfake.Clientset)
	client.ClearActions()
	if _, err := cc.initiateJob(ready); err != nil {
		t.Fatal(err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" && action.GetSubresource() == "status" {
			t.Fatal("unchanged ControlledResources caused a redundant status write")
		}
	}
}

func TestPendingInitializationStatusFailureIsNotHidden(t *testing.T) {
	cc := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "status-error", UID: "job", ResourceVersion: "1"},
		Spec:   batch.JobSpec{Plugins: map[string][]string{"svc": {}}, Volumes: []batch.VolumeSpec{{VolumeClaimName: "missing"}}},
		Status: batch.JobStatus{State: batch.JobState{Phase: batch.Pending}}}
	storeLifecycleJob(t, cc, job)
	if err := cc.cache.Add(job); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.vcClient.BatchV1alpha1().Jobs(job.Namespace).Create(context.TODO(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("status write denied")
	cc.vcClient.(*vcfake.Clientset).PrependReactor("update", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" {
			return true, nil, writeErr
		}
		return false, nil, nil
	})
	if _, err := cc.initiateJob(job.DeepCopy()); !errors.Is(err, writeErr) {
		t.Fatalf("status write error was hidden by later initialization error: %v", err)
	}
	status, err := cc.cache.GetStatus(jobcache.JobKey(job))
	if err != nil || status.ControlledResources["plugin-svc"] != "" {
		t.Fatalf("unsaved plugin marker escaped into cache: %v", err)
	}
}

func TestPendingPluginResourcesAreCleaned(t *testing.T) {
	cc := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "pending-svc", UID: "job", ResourceVersion: "1"},
		Spec: batch.JobSpec{Queue: "default", MinAvailable: 1, MaxRetry: 3, Plugins: map[string][]string{"svc": {}}, Tasks: []batch.TaskSpec{{Name: "worker", Replicas: 1}}}}
	storeLifecycleJob(t, cc, job)
	if _, err := cc.cache.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.vcClient.BatchV1alpha1().Jobs(job.Namespace).Create(context.TODO(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := cc.queueInformer.Informer().GetIndexer().Add(&scheduling.Queue{ObjectMeta: metav1.ObjectMeta{Name: "default"}}); err != nil {
		t.Fatal(err)
	}
	info, err := cc.cache.GetForUID(jobcache.JobKey(job), job.UID)
	if err != nil {
		t.Fatal(err)
	}
	// The scheduler has not yet admitted the PodGroup. No status transition
	// occurs after initJobStatus changes the empty phase to Pending.
	if err := state.NewState(info).Execute(state.Action{Action: bus.SyncJobAction}); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.kubeClient.CoreV1().Services(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err = cc.cache.GetForUID(jobcache.JobKey(job), job.UID)
	if err != nil {
		t.Fatal(err)
	}
	live, err := cc.vcClient.BatchV1alpha1().Jobs(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Job.Status.ControlledResources["plugin-svc"] != "svc" || live.Status.ControlledResources["plugin-svc"] != "svc" {
		t.Fatal("successful plugin initialization was not persisted in cache and API")
	}
	if err := state.NewState(info).Execute(state.Action{Action: bus.TerminateJobAction}); err != nil {
		t.Fatal(err)
	}
	_, err = cc.kubeClient.CoreV1().Services(job.Namespace).Get(context.TODO(), job.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("terminated Pending Job left its plugin Service behind: %v", err)
	}
}

func TestRetiredPodRequestsDoNotReadDeletedJob5000Times(t *testing.T) {
	cc := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "deleted", UID: "old", ResourceVersion: "1"}}
	storeLifecycleJob(t, cc, job)
	if err := cc.cache.Add(job); err != nil {
		t.Fatal(err)
	}
	if err := cc.jobInformer.Informer().GetIndexer().Delete(job); err != nil {
		t.Fatal(err)
	}
	cc.deleteJob(job)
	q := cc.getWorkerQueue(jobcache.JobKey(job))
	for i := range 5000 {
		cc.recoverCurrentJob(q, apis.Request{Namespace: job.Namespace, JobName: job.Name, JobUid: job.UID,
			PodName: fmt.Sprintf("deleted-worker-%d", i), PodUID: types.UID(fmt.Sprintf("pod-%d", i)), Event: bus.PodEvictedEvent})
	}
	gets := 0
	for _, a := range cc.vcClient.(*vcfake.Clientset).Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "jobs" {
			gets++
		}
	}
	if gets != 0 {
		t.Fatalf("already-confirmed retired UID generated %d live GETs for 5000 Pod events", gets)
	}
}

func TestDisabledJobSupportDoesNotRequeueForever(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.VolcanoJobSupport, false)
	cc := newFakeController()
	defer func() {
		for _, q := range cc.queueList {
			q.ShutDown()
		}
		cc.errTasks.ShutDown()
		cc.commandQueue.ShutDown()
	}()
	if cc.jobLister != nil {
		t.Fatal("expected Job informer to be disabled")
	}
	// Native workloads still send PodGroup phase updates to this controller.
	oldPG := &scheduling.PodGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "normal-pg"}, Status: scheduling.PodGroupStatus{Phase: scheduling.PodGroupPending}}
	newPG := oldPG.DeepCopy()
	newPG.Status.Phase = scheduling.PodGroupRunning
	worker := cc.genHash(jobcache.JobKeyByName(newPG.Namespace, newPG.Name)) % cc.workers
	q := &observationQueue{TypedRateLimitingInterface: cc.queueList[worker]}
	cc.queueList[worker] = q
	cc.updatePodGroup(oldPG, newPG)
	cc.processNextReq(worker)
	if len(q.waiting) != 0 {
		t.Fatal("disabled Job support permanently reschedules a request that cannot ever get a Job observation")
	}
}

func TestTaskFailurePolicyAfterProjectionLooksAhead(t *testing.T) {
	cc := newLifecycleController(t)
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "lookahead", UID: "job", ResourceVersion: "1"},
		Spec:   batch.JobSpec{Queue: "default", MinAvailable: 1, MaxRetry: 3, Tasks: []batch.TaskSpec{{Name: "worker", Replicas: 1}}, Policies: []batch.LifecyclePolicy{{Event: bus.PodFailedEvent, Action: bus.RestartJobAction}}},
		Status: batch.JobStatus{State: batch.JobState{Phase: batch.Running}, Running: 1, MinAvailable: 1}}
	storeLifecycleJob(t, cc, job)
	if _, err := cc.vcClient.BatchV1alpha1().Jobs(job.Namespace).Create(context.TODO(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := cc.queueInformer.Informer().GetIndexer().Add(&scheduling.Queue{ObjectMeta: metav1.ObjectMeta{Name: "default"}}); err != nil {
		t.Fatal(err)
	}
	pg := &scheduling.PodGroup{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: cc.generateRelatedPodGroupName(job), UID: "pg", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, helpers.JobKind)}}, Status: scheduling.PodGroupStatus{Phase: scheduling.PodGroupRunning}}
	if err := cc.pgInformer.Informer().GetIndexer().Add(pg); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.vcClient.SchedulingV1beta1().PodGroups(job.Namespace).Create(context.TODO(), pg, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	initial := lifecyclePod(job, "lookahead-worker-0", "pod")
	initial.Status.Phase, initial.ResourceVersion = v1.PodRunning, "1"
	running := initial.DeepCopy()
	running.Status.Phase, running.ResourceVersion = v1.PodRunning, "2"
	failed := running.DeepCopy()
	failed.Status.Phase, failed.ResourceVersion = v1.PodFailed, "3"
	failed.Status.ContainerStatuses = []v1.ContainerStatus{{State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{ExitCode: 42}}}}
	if err := cc.podInformer.Informer().GetIndexer().Add(initial); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.cache.RebuildLifecycle(job.Namespace, job.Name, job.UID, nil); err != nil {
		t.Fatal(err)
	}
	// Shared informer's store has advanced; its callback listener has not.
	if err := cc.podInformer.Informer().GetIndexer().Update(failed); err != nil {
		t.Fatal(err)
	}
	cc.updatePod(initial, running)
	worker := cc.genHash(jobcache.JobKey(job)) % cc.workers
	cc.processNextReq(worker)
	cc.updatePod(running, failed)
	cc.processNextReq(worker)
	current, err := cc.cache.GetForUID(jobcache.JobKey(job), job.UID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Job.Status.State.Phase != batch.Restarting || current.Job.Status.RetryCount != 1 {
		t.Fatalf("PodFailed policy was consumed after projection preempted it: phase=%s retry=%d", current.Job.Status.State.Phase, current.Job.Status.RetryCount)
	}
}
