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

// Package repackengine is the driver of the standalone volcano-repack-engine
// (the analogue of pkg/scheduler/scheduler.go). It reuses the scheduler cache,
// the shared --scheduler-conf tiers/plugins and framework.OpenSession so it sees
// the cluster exactly as the scheduler does, then per cycle: selects one cleared
// RepackRun, opens a scheduler Session, resolves scope, wraps the Session as the
// engine Snapshot, opens an engine Session (running the configured capability
// plugins), runs the action pipeline (which runs the selected core), and writes
// the RepackRun status. The pure model/contracts live in api/ and framework/;
// the scheduler-coupled adapters in adapter/.
//
// The driver is split across a few files in this package:
//   - repackengine.go: engine struct/config, construction and the event loop
//     (Run, informer wiring, processNext, reconcile, orphan recovery).
//   - gate.go:         the K=1 Execute serialization gate (in-memory + cache).
//   - process.go:      one cleared run's plan/act path and its eviction hooks.
//   - status.go:       rendering the search outcome into RepackRun.status.
//   - translate.go:    reading spec knobs (goals/maxPerRun) into engine params.
//
// Logging convention (klog verbosity; logs are for human operators/triage):
//   - Error/Warning: always shown — real failures and misconfiguration.
//   - V(3): operator narrative, on by default. The story of each run: engine
//     started/stopped, per-run gate deferral, plan computed, evictions issued,
//     run finished (outcome), orphan recovery, GC delete, nomination written.
//   - V(4): troubleshooting detail — reconcile entry, slot acquired, retry count,
//     requeue counts, cooldown retention.
//   - V(5): deep debug — gate-state internals, no-match/skip decisions, per-item.
package repackengine

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcinformers "volcano.sh/apis/pkg/client/informers/externalversions"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	schedoptions "volcano.sh/volcano/cmd/scheduler/app/options"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
	"volcano.sh/volcano/pkg/scheduler"
	schedcache "volcano.sh/volcano/pkg/scheduler/cache"
	"volcano.sh/volcano/pkg/scheduler/conf"
	commonutil "volcano.sh/volcano/pkg/util"
)

// Config holds the engine's runtime parameters.
type Config struct {
	SchedulerConf   string        // shared --scheduler-conf (same as volcano-scheduler)
	ResyncPeriod    time.Duration // informer resync safety-net (0 = pure event-driven)
	Cooldown        time.Duration // min gap after an Execute before the next may start
	Core            string        // search strategy (framework.CoreDrain default)
	Plugins         []string      // capability plugins (default base,node,gang)
	Actions         []string      // action pipeline (default: repack)
	MinNodesFreed   int           // benefit gate
	DefaultResource string        // target when spec.goals is empty
	NominationTTL   time.Duration // how long a nomination keeps being re-asserted
}

// Engine drives RepackRuns against scheduler sessions, event-driven: each
// admitted RepackRun is reconciled once on arrival. A single worker + the Execute
// gate (one-at-a-time + cooldown) serialize eviction.
type Engine struct {
	cache schedcache.Cache
	vc    vcclientset.Interface
	cfg   Config

	factory vcinformers.SharedInformerFactory
	lister  repacklisters.RepackRunLister
	synced  cache.InformerSynced
	queue    workqueue.TypedRateLimitingInterface[string]
	recorder record.EventRecorder
	now      func() time.Time

	mu             sync.Mutex
	tiers          []conf.Tier
	configurations []conf.Configuration
	// execActive is the name of the Execute run currently holding the K=1 slot
	// ("" = none); lastExecFinish is when this engine last finished an Execute.
	// Both are authoritative (do not depend on informer-cache freshness), so the
	// gate is correct even before a status write propagates to the lister, and
	// safe if Workers is ever > 1. Guarded by mu.
	execActive     string
	lastExecFinish time.Time
}

const (
	// repackNodeWorkers is the number of scheduler-cache node workers the engine
	// runs to keep sc.Nodes in sync. Must be > 0 or the node queue is never drained.
	repackNodeWorkers = 4
	// defaultNominationTTL is how long an Execute nomination is re-asserted onto the
	// replacement pod before expiring, when the run does not override it.
	defaultNominationTTL = 10 * time.Minute
)

// newEventRecorder builds a recorder that emits Kubernetes events on RepackRun
// objects. It uses a scheme carrying both core (event) and repack types so the
// event references resolve. Nil-safe callers guard on e.recorder.
func newEventRecorder(config *rest.Config) record.EventRecorder {
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.ErrorS(err, "repack-engine: event recorder disabled (client build failed)")
		return nil
	}
	s := runtime.NewScheme()
	utilruntime.Must(kubescheme.AddToScheme(s))
	utilruntime.Must(repackv1alpha1.AddToScheme(s))
	b := record.NewBroadcaster()
	b.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	return b.NewRecorder(s, v1.EventSource{Component: "volcano-repack-engine"})
}

// NewEngine builds the engine, wires the RepackRun informer, and applies defaults.
func NewEngine(config *rest.Config, cfg Config) (*Engine, error) {
	// The engine reuses scheduler machinery (cache, plugins, predicates) that reads
	// the scheduler's global options.ServerOpts — several accesses are unguarded
	// (e.g. volume-binding, predicate/sharding helpers). The scheduler binary fills
	// it via RegisterOptions during flag parsing; the repack-engine binary does not,
	// so ServerOpts would be nil and constructing the cache panics. Initialize a safe
	// default (sharding disabled) if unset. MUST run before schedcache.New below.
	if schedoptions.ServerOpts == nil {
		opt := schedoptions.NewServerOption()
		opt.ShardingMode = commonutil.NoneShardingMode
		opt.RegisterOptions()
	}

	if cfg.Core == "" {
		cfg.Core = engineframework.CoreDrain
	}
	if len(cfg.Plugins) == 0 {
		cfg.Plugins = []string{"base", "node", "gang"}
	}
	if cfg.NominationTTL <= 0 {
		cfg.NominationTTL = defaultNominationTTL
	}
	vc := vcclientset.NewForConfigOrDie(config)
	factory := vcinformers.NewSharedInformerFactory(vc, cfg.ResyncPeriod)
	informer := factory.Repack().V1alpha1().RepackRuns()
	e := &Engine{
		recorder: newEventRecorder(config),
		// Reuse the scheduler cache as a read-only cluster view. New is a pure
		// constructor (no queue bootstrap — that's the scheduler's startup job), so
		// the engine needs only queues get/list/watch, never create. nodeWorkers must
		// be > 0: with 0 workers the node queue is never drained and sc.Nodes stays
		// empty, so the engine would see a zero-node cluster.
		cache:   schedcache.New(config, nil, "", nil, repackNodeWorkers, nil, 0, 0),
		vc:      vc,
		cfg:     cfg,
		factory: factory,
		lister:  informer.Lister(),
		synced:  informer.Informer().HasSynced,
		queue:   workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		now:     time.Now,
	}
	informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    e.enqueue,
		UpdateFunc: func(_, n interface{}) { e.enqueue(n) },
	})
	return e, nil
}

// Run loads the shared scheduler config, starts the cache + informer, and serves
// RepackRun events with a single worker until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()
	defer e.queue.ShutDown()

	if err := e.loadConf(); err != nil {
		klog.ErrorS(err, "repack-engine: load scheduler conf")
	}
	e.factory.Start(ctx.Done())
	e.cache.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), e.synced) {
		klog.Error("repack-engine: RepackRun cache failed to sync")
		return
	}
	e.recoverOrphans() // fail runs left Running by a crashed predecessor
	klog.V(3).InfoS("repack-engine started (event-driven)",
		"core", e.cfg.Core, "plugins", e.cfg.Plugins,
		"defaultResource", e.cfg.DefaultResource, "cooldown", e.cfg.Cooldown, "resyncPeriod", e.cfg.ResyncPeriod)
	// Single worker: Execute runs serialize naturally (one reconcile at a time).
	go func() {
		for e.processNext(ctx) {
		}
	}()
	<-ctx.Done()
	klog.V(3).InfoS("repack-engine shutting down")
}

func (e *Engine) loadConf() error {
	if e.cfg.SchedulerConf == "" {
		return fmt.Errorf("scheduler-conf is required")
	}
	raw, err := os.ReadFile(e.cfg.SchedulerConf)
	if err != nil {
		return err
	}
	_, tiers, configurations, _, err := scheduler.UnmarshalSchedulerConf(string(raw))
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.tiers, e.configurations = tiers, configurations
	e.mu.Unlock()
	return nil
}

// enqueue adds a candidate RepackRun (Admitted + Pending) to the workqueue.
func (e *Engine) enqueue(obj interface{}) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok || !isCandidate(run) {
		return
	}
	e.queue.Add(run.Name)
}

// isCandidate reports whether a run is ready for the engine: not yet processed
// (phase empty or Pending) and not terminal/Running. Admission is enforced by
// CEL at the apiserver, so any RepackRun that exists is already valid.
func isCandidate(run *repackv1alpha1.RepackRun) bool {
	p := run.Status.Phase
	return p == "" || p == repackv1alpha1.RepackPending
}

// maxReconcileRetries caps how many times a failing RepackRun is retried before
// it is treated as a poison pill: the engine gives up and marks it Failed rather
// than retrying forever (which would also keep re-panicking on a bad object).
const maxReconcileRetries = 5

func (e *Engine) processNext(ctx context.Context) bool {
	key, shutdown := e.queue.Get()
	if shutdown {
		return false
	}
	defer e.queue.Done(key)

	if err := e.reconcileSafely(ctx, key); err != nil {
		utilruntime.HandleError(fmt.Errorf("repack-engine reconcile %q: %w", key, err))
		if e.queue.NumRequeues(key) < maxReconcileRetries {
			klog.V(4).InfoS("requeueing RepackRun after error", "run", key, "retries", e.queue.NumRequeues(key)+1)
			e.queue.AddRateLimited(key)
			return true
		}
		// Poison pill: stop retrying and fail the run so it does not loop forever
		// (and its Execute slot, if any, was already released by process's defer).
		e.queue.Forget(key)
		e.failByName(key, "ReconcileGaveUp", fmt.Errorf("gave up after %d retries: %w", maxReconcileRetries, err))
		return true
	}
	e.queue.Forget(key)
	return true
}

// reconcileSafely runs reconcile with panic recovery so a single bad RepackRun
// (e.g. a plugin/snapshot panic) cannot crash the engine's worker goroutine. The
// panic is converted to an error; process's own defers (slot release, session
// close) still run during unwinding before it reaches here.
func (e *Engine) reconcileSafely(ctx context.Context, name string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in reconcile: %v", r)
			klog.ErrorS(err, "repack-engine: recovered panic", "run", name, "stack", string(debug.Stack()))
		}
	}()
	return e.reconcile(ctx, name)
}

// failByName marks a run Failed by name (poison-pill path); best-effort.
func (e *Engine) failByName(name, reason string, cause error) {
	run, err := e.lister.Get(name)
	if err != nil {
		return // gone or lister error; nothing to write
	}
	work := run.DeepCopy()
	e.fail(work, work.Generation, reason, cause)
}

// reconcile processes one RepackRun: re-check it's still a candidate, apply the
// Execute serialization gate (one-at-a-time + cooldown — it lives here, in the
// worker that actually evicts), then plan/act.
func (e *Engine) reconcile(_ context.Context, name string) error {
	run, err := e.lister.Get(name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !isCandidate(run) {
		return nil // already picked up / terminal
	}
	klog.V(4).InfoS("reconciling RepackRun", "run", name, "mode", run.Spec.Mode)
	work := run.DeepCopy()

	// Acknowledge as Pending so `kubectl get repackrun` shows a phase before the
	// engine starts (deferred Execute runs also settle here via the gate below).
	if work.Status.Phase == "" {
		work.Status.Phase = repackv1alpha1.RepackPending
		e.updateStatus(work)
	}

	active, lastFinish := e.executeGateState(work.Name)
	gate := state.EvaluateGate(state.GateInputs{
		Mode:              work.Spec.Mode,
		ExecuteActive:     active,
		LastExecuteFinish: lastFinish,
		Cooldown:          e.cfg.Cooldown,
		Now:               e.now(),
	})
	if !gate.Admit {
		metrics.ObserveGateRejection(gate.Reason)
		klog.V(3).InfoS("RepackRun deferred by execute gate",
			"run", name, "reason", gate.Reason, "requeueAfter", gate.RequeueAfter)
		state.SetCondition(&work.Status.Conditions, state.CondQueued, metav1.ConditionTrue,
			gate.Reason, "waiting for an execute slot", work.Generation)
		work.Status.Phase = state.DerivePhase(work.Status.Conditions)
		e.updateStatus(work)
		if gate.RequeueAfter > 0 {
			e.queue.AddAfter(name, gate.RequeueAfter)
		}
		return nil
	}
	if work.Spec.Mode == repackv1alpha1.RepackModeExecute {
		e.markExecuteActive(work.Name) // hold the K=1 slot across this synchronous process
		klog.V(4).InfoS("acquired execute slot", "run", work.Name)
	}
	e.process(work)
	return nil
}

// recoverOrphans fails any run left in Running by a crashed predecessor. With a
// single leader-elected engine, a Running run at startup cannot be in progress
// (this instance just started), so it is orphaned: mark it Failed to release the
// Execute slot and let TTL GC collect it. Conservative on purpose — we do not
// re-run, since a mid-eviction Execute must not be blindly repeated.
func (e *Engine) recoverOrphans() {
	runs, err := e.lister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "repack-engine: list for orphan recovery")
		return
	}
	for _, r := range runs {
		if r.Status.Phase != repackv1alpha1.RepackRunning {
			continue
		}
		work := r.DeepCopy()
		gen := work.Generation
		const reason = "Interrupted"
		msg := "engine restarted while this run was in progress"
		state.SetCondition(&work.Status.Conditions, state.CondProgressing, metav1.ConditionFalse, reason, msg, gen)
		state.SetCondition(&work.Status.Conditions, state.CondFailed, metav1.ConditionTrue, reason, msg, gen)
		work.Status.Phase = state.DerivePhase(work.Status.Conditions)
		e.updateStatusTerminal(work)
		klog.V(3).InfoS("recovered orphaned Running RepackRun -> Failed", "run", work.Name)
	}
}
