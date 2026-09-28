/*
Copyright 2019 The Volcano Authors.

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
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"volcano.sh/apis/pkg/apis/batch/v1alpha1"
	"volcano.sh/volcano/pkg/controllers/apis"
)

// Cache Interface.
type Cache interface {
	Run(stopCh <-chan struct{})

	Get(key string) (*apis.JobInfo, error)
	GetForUID(key string, uid types.UID) (*apis.JobInfo, error)
	GetLifecycle(key string) (Lifecycle, error)
	RebuildLifecycle(namespace, name string, expectedUID types.UID, refresh []string) (Lifecycle, error)
	ObservePod(pod *v1.Pod, deleted bool) (Observation, error)
	AcknowledgeTerminalPod(req apis.Request)
	RetireUID(uid types.UID)
	IsRetired(uid types.UID) bool
	RecoveryDelay(key string, uid types.UID) time.Duration
	AllowIdentityCheck(key string, uid types.UID) bool
	ResetRecovery(key string, uid types.UID)
	RecordJobPhase(job *v1alpha1.Job, phase v1alpha1.JobPhase)
	GetStatus(key string) (*v1alpha1.JobStatus, error)
	Add(obj *v1alpha1.Job) error
	Update(obj *v1alpha1.Job) error
	Delete(obj *v1alpha1.Job) error

	AddPod(pod *v1.Pod) error
	UpdatePod(pod *v1.Pod) error
	DeletePod(pod *v1.Pod) error
	HasPod(pod *v1.Pod) bool

	TaskCompleted(jobKey, taskName string, uid ...types.UID) bool
	TaskFailed(jobKey, taskName string, uid ...types.UID) bool
}
