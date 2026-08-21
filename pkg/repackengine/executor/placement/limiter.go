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

package placement

import (
	"time"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

// RepairLimiter independently rate-limits the fallback scan that repairs
// placement leases on recreated PodGroups.
type RepairLimiter struct {
	runIdentity string
	lastRepair  time.Time
}

// Allow records an accepted repair scan. When rejected, next is the earliest
// time the same RepackRun may scan again.
func (limiter *RepairLimiter) Allow(run *repackv1alpha1.RepackRun, now time.Time, interval time.Duration) (allowed bool, next time.Time) {
	if run == nil {
		return false, time.Time{}
	}
	runIdentity := run.Name + "/" + string(run.UID)
	next = limiter.lastRepair.Add(interval)
	if limiter.runIdentity == runIdentity && now.Before(next) {
		return false, next
	}
	limiter.runIdentity = runIdentity
	limiter.lastRepair = now
	return true, now.Add(interval)
}
