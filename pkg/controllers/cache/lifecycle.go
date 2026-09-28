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
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	"volcano.sh/volcano/pkg/controllers/apis"
	controllermetrics "volcano.sh/volcano/pkg/controllers/metrics"
)

var (
	ErrLifecycleChanged = errors.New("job lifecycle changed")
	ErrNeedsRecovery    = errors.New("job cache needs initialization")
)

// SnapshotReader may only read local informer stores. Its methods are called
// with the cache lock held and must never call the cache or the API server.
type SnapshotReader interface {
	GetJob(namespace, name string) (*batch.Job, error)
	GetPod(namespace, name string) (*v1.Pod, error)
	ListPods(namespace, name string, uid types.UID) ([]*v1.Pod, error)
}

type Lifecycle struct {
	UID         types.UID
	Version     int32
	Initialized bool
}

type Observation int

const (
	Unchanged Observation = iota
	Applied
	NeedsRecovery
)

type recoveryWait struct {
	uid          types.UID
	delay        time.Duration
	checkedUntil time.Time
}

func (jc *jobCache) lifecycle(key string) (Lifecycle, error) {
	job := jc.jobs[key]
	if job == nil || job.Job == nil || job.Deleted {
		return Lifecycle{}, fmt.Errorf("job <%s> is not ready", key)
	}
	return Lifecycle{UID: job.UID, Version: job.Job.Status.Version, Initialized: jc.reader == nil || jc.initialized[key]}, nil
}

func (jc *jobCache) GetLifecycle(key string) (Lifecycle, error) {
	jc.Lock()
	defer jc.Unlock()
	return jc.lifecycle(key)
}

func (jc *jobCache) GetForUID(key string, uid types.UID) (*apis.JobInfo, error) {
	jc.Lock()
	defer jc.Unlock()
	current, err := jc.lifecycle(key)
	if err != nil {
		return nil, err
	}
	if uid != "" && current.UID != uid {
		return nil, ErrLifecycleChanged
	}
	if !current.Initialized {
		return nil, ErrNeedsRecovery
	}
	return jc.jobs[key].Clone(), nil
}

// RebuildLifecycle reads the snapshot AFTER acquiring the cache lock. A nil
// refresh is an initialization request; nonempty refresh names are evidence of
// missing Pods in an already initialized lifecycle. Recheck that evidence to
// avoid rebuilding once for every AlreadyExists response.
func (jc *jobCache) RebuildLifecycle(namespace, name string, expectedUID types.UID, refresh []string) (result Lifecycle, err error) {
	start := time.Now()
	jc.Lock()
	acquired := time.Now()
	mode, outcome, count := "ensure", "skipped", 0
	if len(refresh) != 0 {
		mode = "refresh"
	}
	defer func() {
		held := time.Since(acquired)
		jc.Unlock()
		if err != nil {
			outcome = "error"
		}
		controllermetrics.ObserveLifecycleRebuild(mode, outcome, count, acquired.Sub(start), held)
	}()
	if jc.reader == nil {
		return result, fmt.Errorf("lifecycle recovery requires informer stores")
	}
	job, err := jc.reader.GetJob(namespace, name)
	if err != nil {
		return result, err
	}
	if job.UID == "" {
		return result, fmt.Errorf("job <%s/%s> has no UID", namespace, name)
	}
	if expectedUID != "" && job.UID != expectedUID {
		return result, ErrLifecycleChanged
	}
	if _, retired := jc.retired[job.UID]; retired {
		return result, ErrLifecycleChanged
	}
	key := JobKey(job)
	current := jc.jobs[key]
	if current != nil && current.UID == job.UID && current.Deleted {
		return result, ErrLifecycleChanged
	}
	if current != nil && current.Job != nil && current.UID == job.UID && jc.initialized[key] {
		missing := false
		for _, name := range refresh {
			pod, getErr := jc.reader.GetPod(namespace, name)
			if apierrors.IsNotFound(getErr) {
				continue
			}
			if getErr != nil {
				return result, getErr
			}
			if jobUIDOfPod(pod) == job.UID && !current.HasPod(pod) {
				missing = true
				break
			}
		}
		if !missing {
			return jc.lifecycle(key)
		}
	}
	pods, err := jc.reader.ListPods(namespace, name, job.UID)
	if err != nil {
		return result, err
	}
	// Status writes can reach this cache before the Job informer. Never roll
	// them back while repairing the Pod projection of the same lifecycle.
	if current != nil && current.Job != nil && current.UID == job.UID {
		job = current.Job
	}
	replacement := newJobInfo(job)
	for _, pod := range pods {
		if pod.Namespace != job.Namespace || pod.Annotations[batch.JobNameKey] != job.Name || validateObservedPod(pod, job) != nil {
			return result, fmt.Errorf("invalid owned Pod <%s/%s> during recovery", pod.Namespace, pod.Name)
		}
		if err := replacement.AddPod(pod); err != nil {
			return result, err
		}
		if current != nil && current.UID == job.UID {
			if _, handled := current.HandledTerminalPods[pod.UID]; handled {
				replacement.MarkTerminalPodHandled(pod.Annotations[batch.TaskSpecKey], pod.Name, pod.UID)
			}
		}
		count++
	}
	observed, err := jc.reader.GetJob(namespace, name)
	if err != nil {
		return result, err
	}
	if observed.UID != job.UID {
		return result, ErrLifecycleChanged
	}
	if current != nil && current.UID != job.UID {
		jc.retireUID(current.UID)
	}
	if current != nil && current.Job != nil && current.UID != job.UID {
		controllermetrics.DeleteJobMetrics(key, current.Job.Spec.Queue)
	}
	jc.jobs[key] = replacement
	jc.initialized[key] = true
	delete(jc.recovery, key)
	outcome = "rebuilt"
	return jc.lifecycle(key)
}

// ObservePod refreshes a SINGLE position from the informer, not from a queued
// callback or an API GET. Cache changes do not acknowledge policy events.
func (jc *jobCache) ObservePod(event *v1.Pod, deleted bool) (Observation, error) {
	if jc.reader == nil {
		// Standalone caches are used by state/action unit tests without informers.
		if deleted {
			return Applied, jc.DeletePod(event)
		}
		if jc.HasPod(event) {
			return Applied, jc.UpdatePod(event)
		}
		return Applied, jc.AddPod(event)
	}
	jc.Lock()
	var cleanup *apis.JobInfo
	defer func() {
		jc.Unlock()
		if cleanup != nil {
			jc.deleteJob(cleanup)
		}
	}()
	key, err := jobKeyOfPod(event)
	if err != nil {
		return Unchanged, err
	}
	job := jc.jobs[key]
	if job == nil {
		return NeedsRecovery, nil
	}
	pod, err := jc.reader.GetPod(event.Namespace, event.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return Unchanged, err
	}
	belongs := err == nil && jobUIDOfPod(pod) == job.UID
	if job.Job == nil && err == nil {
		return Unchanged, nil
	}
	if belongs {
		podKey, keyErr := jobKeyOfPod(pod)
		if keyErr != nil || podKey != key {
			return Unchanged, fmt.Errorf("Pod <%s/%s> job identity changed", pod.Namespace, pod.Name)
		}
		if err := validateObservedPod(pod, job.Job); err != nil {
			return Unchanged, err
		}
	}
	// A task/partition label can change as well as the Pod UID. Remove the old
	// position using its own metadata before installing the observed object.
	var terminalHandled bool
	if belongs {
		_, terminalHandled = job.HandledTerminalPods[pod.UID]
	}
	for _, taskPods := range job.Pods {
		if previous := taskPods[event.Name]; previous != nil {
			if belongs && previous == pod {
				return Unchanged, nil
			}
			if deleteErr := job.DeletePod(previous); deleteErr != nil {
				return Unchanged, deleteErr
			}
		}
	}
	if belongs {
		if err := job.AddPod(pod); err != nil {
			return Unchanged, err
		}
		if terminalHandled {
			job.MarkTerminalPodHandled(pod.Annotations[batch.TaskSpecKey], pod.Name, pod.UID)
		}
	}
	if jobTerminated(job) {
		cleanup = job
	}
	if err == nil && !belongs {
		return NeedsRecovery, nil
	}
	return Applied, nil
}

func (jc *jobCache) AcknowledgeTerminalPod(req apis.Request) {
	jc.Lock()
	defer jc.Unlock()
	job := jc.jobs[JobKeyByReq(&req)]
	if job == nil || job.Job == nil || job.Deleted || job.UID != req.JobUid {
		return
	}
	job.MarkTerminalPodHandled(req.TaskName, req.PodName, req.PodUID)
}

func validateObservedPod(pod *v1.Pod, job *batch.Job) error {
	owner := volcanoJobOwner(pod)
	if owner == nil || owner.Name != job.Name || owner.UID != job.UID || pod.UID == "" {
		return fmt.Errorf("invalid owner or UID of Pod %s/%s", pod.Namespace, pod.Name)
	}
	if pod.Annotations[batch.TaskSpecKey] == "" {
		return fmt.Errorf("Pod %s/%s has no task", pod.Namespace, pod.Name)
	}
	if _, err := strconv.ParseInt(pod.Annotations[batch.JobVersion], 10, 32); err != nil {
		return fmt.Errorf("Pod %s/%s has invalid JobVersion: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

func (jc *jobCache) retireUID(uid types.UID) {
	if uid == "" {
		return
	}
	if jc.retired == nil {
		jc.retired = make(map[types.UID]struct{})
	}
	// Eviction only loses an optimization: unknown UIDs are verified again.
	if len(jc.retired) >= 4096 {
		clear(jc.retired)
	}
	jc.retired[uid] = struct{}{}
}

func (jc *jobCache) RetireUID(uid types.UID) { jc.Lock(); defer jc.Unlock(); jc.retireUID(uid) }
func (jc *jobCache) IsRetired(uid types.UID) bool {
	jc.Lock()
	defer jc.Unlock()
	_, ok := jc.retired[uid]
	return ok
}

func (jc *jobCache) RecoveryDelay(key string, uid types.UID) time.Duration {
	jc.Lock()
	defer jc.Unlock()
	if jc.recovery == nil {
		jc.recovery = make(map[string]recoveryWait)
	}
	entry := jc.recovery[key]
	if entry.uid != uid || entry.delay == 0 {
		entry = recoveryWait{uid: uid, delay: time.Second, checkedUntil: entry.checkedUntil}
	} else {
		entry.delay = min(2*entry.delay, 30*time.Second)
	}
	jc.recovery[key] = entry
	return wait.Jitter(entry.delay, 0.1)
}

// AllowIdentityCheck coalesces live Job GETs while informers are misaligned.
// It is only a per-key rate gate, never cached proof that a UID is current.
func (jc *jobCache) AllowIdentityCheck(key string, uid types.UID) bool {
	jc.Lock()
	defer jc.Unlock()
	if jc.recovery == nil {
		jc.recovery = make(map[string]recoveryWait)
	}
	entry := jc.recovery[key]
	now := time.Now()
	if now.Before(entry.checkedUntil) {
		return false
	}
	if entry.uid != uid {
		entry.delay = 0
	}
	entry.uid, entry.checkedUntil = uid, now.Add(time.Second)
	jc.recovery[key] = entry
	return true
}

func (jc *jobCache) ResetRecovery(key string, uid types.UID) {
	jc.Lock()
	defer jc.Unlock()
	if jc.recovery[key].uid == uid {
		delete(jc.recovery, key)
	}
}

func (jc *jobCache) RecordJobPhase(job *batch.Job, phase batch.JobPhase) {
	jc.Lock()
	defer jc.Unlock()
	key := JobKey(job)
	current := jc.jobs[key]
	if current == nil || current.Job == nil || current.UID != job.UID {
		return
	}
	switch phase {
	case batch.Completed:
		controllermetrics.UpdateJobCompleted(key, job.Spec.Queue)
	case batch.Failed:
		controllermetrics.UpdateJobFailed(key, job.Spec.Queue)
	}
}
