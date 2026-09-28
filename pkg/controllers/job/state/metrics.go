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

package state

import batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"

// RecordJobPhase is installed alongside SyncJob/KillJob. The controller cache
// checks UID and updates counters under the same lock as lifecycle replacement.
var RecordJobPhase = func(job *batch.Job, phase batch.JobPhase) {}

func UpdateJobCompleted(job *batch.Job) { RecordJobPhase(job, batch.Completed) }
func UpdateJobFailed(job *batch.Job)    { RecordJobPhase(job, batch.Failed) }
