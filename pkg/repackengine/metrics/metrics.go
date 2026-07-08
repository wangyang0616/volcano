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

// Package metrics defines the volcano-repack-engine Prometheus metrics. They are
// registered on the default registry (via promauto), which the engine's /metrics
// endpoint serves. Emit points live in the driver (pkg/repackengine).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const subsystem = "volcano_repack"

var (
	// RunsTotal counts finished RepackRuns by mode (DryRun/Execute) and terminal
	// outcome (the Complete/Failed condition reason: RepackRecommended, Executed,
	// NoFragmentation, BelowGoalThreshold, ExecuteFailed, ...).
	RunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "runs_total",
		Help:      "Number of finished RepackRuns by mode and terminal outcome.",
	}, []string{"mode", "outcome"})

	// EvictionsTotal counts pod evictions attempted during Execute, by result.
	EvictionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "evictions_total",
		Help:      "Number of pod evictions attempted during Execute, by result (evicted/rejected).",
	}, []string{"result"})

	// CycleDurationSeconds observes how long one reconcile's plan/act took.
	CycleDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "cycle_duration_seconds",
		Help:      "Wall time of one RepackRun reconcile (plan + optional evict), by mode.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"mode"})

	// GateRejectionsTotal counts Execute runs the K=1/cooldown gate deferred, by
	// reason (AnotherRunActive, ExecuteCoolingDown, ...).
	GateRejectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "gate_rejections_total",
		Help:      "Number of times the Execute serialization gate deferred a run, by reason.",
	}, []string{"reason"})
)

// ObserveRun records a finished run's mode+outcome.
func ObserveRun(mode, outcome string) { RunsTotal.WithLabelValues(mode, outcome).Inc() }

// ObserveEvictions records eviction results for one Execute commit.
func ObserveEvictions(evicted, rejected int) {
	if evicted > 0 {
		EvictionsTotal.WithLabelValues("evicted").Add(float64(evicted))
	}
	if rejected > 0 {
		EvictionsTotal.WithLabelValues("rejected").Add(float64(rejected))
	}
}

// ObserveCycle records reconcile wall time for a mode.
func ObserveCycle(mode string, seconds float64) {
	CycleDurationSeconds.WithLabelValues(mode).Observe(seconds)
}

// ObserveGateRejection records one gate deferral by reason.
func ObserveGateRejection(reason string) { GateRejectionsTotal.WithLabelValues(reason).Inc() }
