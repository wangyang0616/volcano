package aggregator

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	reconcileTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "volcano",
			Subsystem: "ranktable_aggregator",
			Name:      "reconcile_total",
			Help:      "RankTable reconcile attempts labeled by result: success, failure, or skipped.",
		},
		[]string{"result"},
	)
	reconcileDurationSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "volcano",
			Subsystem: "ranktable_aggregator",
			Name:      "reconcile_duration_seconds",
			Help:      "Latency of a full ReconcileOnce (success or failure).",
			Buckets:   prometheus.DefBuckets,
		},
	)
	shardFetchTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "volcano",
			Subsystem: "ranktable_aggregator",
			Name:      "shard_fetch_total",
			Help:      "Per-shard ConfigMap GET+decode labeled by result.",
		},
		[]string{"result"},
	)
)

func observeReconcileOutcome(start time.Time, err error, skipped bool) {
	reconcileDurationSeconds.Observe(time.Since(start).Seconds())
	switch {
	case skipped:
		reconcileTotal.WithLabelValues("skipped").Inc()
	case err != nil:
		reconcileTotal.WithLabelValues("failure").Inc()
	default:
		reconcileTotal.WithLabelValues("success").Inc()
	}
}

func observeShardFetch(err error) {
	if err != nil {
		shardFetchTotal.WithLabelValues("failure").Inc()
		return
	}
	shardFetchTotal.WithLabelValues("success").Inc()
}
