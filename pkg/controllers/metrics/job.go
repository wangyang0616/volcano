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

package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"volcano.sh/volcano/pkg/controllers/util"
)

var (
	// jobToPodCreationLatency is the per-pod latency from VCJob creation to pod created.
	jobToPodCreationLatency = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Subsystem: util.VolcanoSubSystemName,
			Name:      "controller_job_to_pod_creation_latency_milliseconds",
			Help:      "Latency from VCJob creation to pod created in milliseconds",
			Buckets:   prometheus.ExponentialBucketsRange(50, 60000, 30),
		},
	)
	jobControllerCacheMissCount = promauto.NewCounter(
		prometheus.CounterOpts{
			Subsystem: util.VolcanoSubSystemName,
			Name:      "controller_job_cache_miss_total",
			Help:      "Total number of VCJob controller worker cache misses.",
		},
	)
	jobControllerLifecycleMismatchCount = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: util.VolcanoSubSystemName,
			Name:      "controller_job_lifecycle_mismatch_total",
			Help:      "Total number of stale VCJob lifecycle operations rejected by UID validation.",
		},
		[]string{"operation"},
	)
	jobControllerStaleCleanupCount = promauto.NewCounter(
		prometheus.CounterOpts{
			Subsystem: util.VolcanoSubSystemName,
			Name:      "controller_job_stale_cleanup_total",
			Help:      "Total number of stale VCJob cache cleanup items ignored after name reuse.",
		},
	)
)

// DurationInMilliseconds converts a time.Duration to float64 milliseconds.
func DurationInMilliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

// ObserveJobToPodCreationLatency observes the latency from job creation to a single pod created.
func ObserveJobToPodCreationLatency(duration time.Duration) {
	jobToPodCreationLatency.Observe(DurationInMilliseconds(duration))
}

// IncJobControllerCacheMiss records a job controller worker cache miss.
func IncJobControllerCacheMiss() {
	jobControllerCacheMissCount.Inc()
}

// IncJobControllerLifecycleMismatch records a rejected operation from an old
// VCJob lifecycle. Operation must be a bounded controller-defined value.
func IncJobControllerLifecycleMismatch(operation string) {
	jobControllerLifecycleMismatchCount.WithLabelValues(operation).Inc()
}

// IncJobControllerStaleCleanup records a cleanup item that referred to an old
// VCJob lifecycle after the namespace/name had been reused.
func IncJobControllerStaleCleanup() {
	jobControllerStaleCleanupCount.Inc()
}
