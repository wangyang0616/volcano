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
	schedulerCache schedcache.Cache
	volcanoClient  vcclientset.Interface
	config         Config

	informerFactory         vcinformers.SharedInformerFactory
	repackRunLister         repacklisters.RepackRunLister
	repackRunInformerSynced cache.InformerSynced
	workQueue               workqueue.TypedRateLimitingInterface[string]
	recorder                record.EventRecorder
	now                     func() time.Time

	tiers          []conf.Tier
	configurations []conf.Configuration
	// activeExecuteRunName is the Execute run currently holding the K=1 slot
	// ("" = none); lastExecuteFinishTime is when this engine last finished an Execute.
	// These values bridge informer-cache propagation and are protected by
	// executeStateMutex. The check and claim are performed in one critical section
	// so K=1 remains correct if the worker count is increased later.
	executeStateMutex     sync.Mutex
	activeExecuteRunName  string
	lastExecuteFinishTime time.Time
	// pendingTerminalStatuses retains the exact terminal projection across a
	// bounded write failure, so the queued retry never reruns side effects.
	terminalStatusMutex     sync.Mutex
	pendingTerminalStatuses map[string]*repackv1alpha1.RepackRunStatus

	// The PodGroup webhook is the primary placement-lease barrier. This timestamp
	// independently rate-limits the engine's namespace-wide fallback scan so the
	// two-second placement loop does not repeatedly list every PodGroup.
	placementLeaseRepairMutex       sync.Mutex
	placementLeaseRepairRunIdentity string
	lastPlacementLeaseRepairTime    time.Time
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
		klog.ErrorS(err, "repack: event recorder disabled (client build failed)")
		return nil
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(kubescheme.AddToScheme(scheme))
	utilruntime.Must(repackv1alpha1.AddToScheme(scheme))
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	return broadcaster.NewRecorder(scheme, v1.EventSource{Component: "volcano-repack-engine"})
}

// NewEngine builds the engine, wires the RepackRun informer, and applies defaults.
func NewEngine(config *rest.Config, engineConfig Config) (*Engine, error) {
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

	if engineConfig.Core == "" {
		engineConfig.Core = engineframework.CoreDrain
	}
	if len(engineConfig.Plugins) == 0 {
		engineConfig.Plugins = []string{"base", "node", "gang"}
	}
	if engineConfig.NominationTTL <= 0 {
		engineConfig.NominationTTL = defaultNominationTTL
	}
	volcanoClient := vcclientset.NewForConfigOrDie(config)
	informerFactory := vcinformers.NewSharedInformerFactory(volcanoClient, engineConfig.ResyncPeriod)
	informer := informerFactory.Repack().V1alpha1().RepackRuns()
	schedulerNames := schedoptions.ServerOpts.SchedulerNames
	if len(schedulerNames) == 0 {
		schedulerNames = []string{"volcano"}
	}
	e := &Engine{
		recorder: newEventRecorder(config),
		// Reuse the scheduler cache as a read-only cluster view. New is a pure
		// constructor (no queue bootstrap — that's the scheduler's startup job), so
		// the engine needs only queues get/list/watch, never create. nodeWorkers must
		// be > 0: with 0 workers the node queue is never drained and sc.Nodes stays
		// empty, so the engine would see a zero-node cluster.
		// schedulerNames must match the pods the engine plans to move (volcano Jobs
		// use schedulerName=volcano); an empty list skips Job/PodGroup indexing.
		schedulerCache: schedcache.New(config, schedulerNames, schedoptions.ServerOpts.DefaultQueue,
			schedoptions.ServerOpts.NodeSelector, repackNodeWorkers,
			schedoptions.ServerOpts.IgnoredCSIProvisioners, 0, 0),
		volcanoClient:           volcanoClient,
		config:                  engineConfig,
		informerFactory:         informerFactory,
		repackRunLister:         informer.Lister(),
		repackRunInformerSynced: informer.Informer().HasSynced,
		workQueue:               workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		now:                     time.Now,
		pendingTerminalStatuses: make(map[string]*repackv1alpha1.RepackRunStatus),
	}
	informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: e.enqueue,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldRun, oldOK := oldObj.(*repackv1alpha1.RepackRun)
			newRun, newOK := newObj.(*repackv1alpha1.RepackRun)
			if !oldOK || !newOK {
				return
			}
			// A placement Run is deliberately driven by status transitions from the
			// controller (Gated -> Nominated -> Placed), so it must be requeued on
			// a real update as well. Initial planning still ignores its own status
			// writes; a same-RV update remains the informer-resync safety net.
			if oldRun.ResourceVersion == newRun.ResourceVersion || isEvictionCandidate(newRun) ||
				isPlacementCandidate(newRun) || isPlacementCleanupCandidate(newRun) {
				e.enqueue(newRun)
			}
		},
	})
	return e, nil
}

// Run loads the shared scheduler config, starts the cache + informer, and serves
// RepackRun events with a single worker until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()
	defer e.workQueue.ShutDown()

	if err := e.loadConf(); err != nil {
		klog.ErrorS(err, "repack: load scheduler conf")
		return // fail closed: never plan/evict without the scheduler's filter stack
	}
	e.informerFactory.Start(ctx.Done())
	e.schedulerCache.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), e.repackRunInformerSynced) {
		klog.Error("repack: RepackRun cache failed to sync")
		return
	}
	e.recoverOrphans(ctx) // fail runs left Running by a crashed predecessor
	klog.V(3).InfoS("repack-engine started (event-driven)",
		"core", e.config.Core, "plugins", e.config.Plugins,
		"defaultResource", e.config.DefaultResource, "cooldown", e.config.Cooldown, "resyncPeriod", e.config.ResyncPeriod)
	// Single worker: Execute runs serialize naturally (one reconcile at a time).
	go func() {
		for e.processNext(ctx) {
		}
	}()
	<-ctx.Done()
	klog.V(3).InfoS("repack-engine shutting down")
}

func (e *Engine) loadConf() error {
	if e.config.SchedulerConf == "" {
		return fmt.Errorf("scheduler-conf is required")
	}
	raw, err := os.ReadFile(e.config.SchedulerConf)
	if err != nil {
		return err
	}
	_, tiers, configurations, _, err := scheduler.UnmarshalSchedulerConf(string(raw))
	if err != nil {
		return err
	}
	e.tiers, e.configurations = tiers, configurations
	return nil
}

// enqueue adds a planning or in-flight placement RepackRun to the workqueue.
func (e *Engine) enqueue(obj interface{}) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok || (!isCandidate(run) && !isPlacementCleanupCandidate(run)) {
		return
	}
	e.workQueue.Add(run.Name)
}

// isCandidate reports whether a run is ready for initial planning or for the
// post-eviction placement protocol. A Running Execute with a persisted plan is
// revisited only while it has durable placement records; it never repeats the
// eviction commit.
func isCandidate(run *repackv1alpha1.RepackRun) bool {
	if isEvictionCandidate(run) {
		return true
	}
	if isPlacementCandidate(run) {
		return true
	}
	p := run.Status.Phase
	return p == "" || p == repackv1alpha1.RepackPending ||
		(p == repackv1alpha1.RepackRunning && run.Status.Plan == nil)
}

func isEvictionCandidate(run *repackv1alpha1.RepackRun) bool {
	if run == nil || run.Spec.Mode != repackv1alpha1.RepackModeExecute ||
		run.Status.Phase != repackv1alpha1.RepackRunning || run.Status.Plan == nil {
		return false
	}
	evictionJournalPresent := false
	for index := range run.Status.Nominations {
		if run.Status.Nominations[index].EvictionPhase != "" {
			evictionJournalPresent = true
		}
		switch run.Status.Nominations[index].EvictionPhase {
		case repackv1alpha1.PodEvictionPending,
			repackv1alpha1.PodEvictionInProgress,
			repackv1alpha1.PodEvictionVictimNotFound:
			return true
		}
	}
	if !evictionJournalPresent {
		return false // compatibility with Runs created before the eviction journal
	}
	for index := range run.Status.Conditions {
		condition := &run.Status.Conditions[index]
		if condition.Type == state.CondProgressing &&
			condition.Status == metav1.ConditionTrue &&
			condition.Reason == state.ReasonAwaitingPlacement {
			return false
		}
	}
	// All per-Pod outcomes may be final while the aggregate accepted subset and
	// AwaitingPlacement barrier are not yet durable. Resume finalization.
	return true
}

func isPlacementCandidate(run *repackv1alpha1.RepackRun) bool {
	return run != nil && run.Spec.Mode == repackv1alpha1.RepackModeExecute &&
		run.Status.Phase == repackv1alpha1.RepackRunning && run.Status.Plan != nil &&
		len(run.Status.Nominations) > 0 && !isEvictionCandidate(run)
}

// isPlacementCleanupCandidate admits an already-terminal Execute Run only to
// retry idempotent removal of its gate-owner markers and PodGroup leases. It
// never re-enters planning or eviction.
func isPlacementCleanupCandidate(run *repackv1alpha1.RepackRun) bool {
	if run == nil || run.Spec.Mode != repackv1alpha1.RepackModeExecute {
		return false
	}
	// A failure before the first eviction clears nominations but can still leave
	// the admission discovery label or an original PodGroup lease behind. The
	// metadata label therefore also makes a terminal Run cleanup-retryable.
	if len(run.Status.Nominations) == 0 &&
		run.Labels[repackv1alpha1.PlacementActiveLabel] != "true" {
		return false
	}
	switch run.Status.Phase {
	case repackv1alpha1.RepackSucceeded, repackv1alpha1.RepackFailed, repackv1alpha1.RepackCancelled:
		return true
	default:
		return false
	}
}

// maxReconcileRetries caps how many times a failing RepackRun is retried before
// it is treated as a poison pill: the engine gives up and marks it Failed rather
// than retrying forever (which would also keep re-panicking on a bad object).
const maxReconcileRetries = 5

// statusPersistenceRequeueInterval is the outer retry delay after bounded local
// status retries are exhausted. Status contention and terminal persistence
// failures must yield the worker without consuming the poison-pill budget.
const statusPersistenceRequeueInterval = time.Second

func (e *Engine) processNext(ctx context.Context) bool {
	key, shutdown := e.workQueue.Get()
	if shutdown {
		return false
	}
	defer e.workQueue.Done(key)

	if err := e.reconcileSafely(ctx, key); err != nil {
		if !reconcileErrorConsumesRetryBudget(err) {
			// AddAfter does not advance the rate-limiter counter, so status contention
			// cannot turn into ReconcileGaveUp. RetryOnConflict already performed
			// exponential backoff inside the status mutation; this delayed retry spans
			// reconcile attempts while preserving any prior real-failure count.
			klog.V(4).InfoS("requeueing RepackRun after retryable status persistence error",
				"run", key, "retryAfter", statusPersistenceRequeueInterval, "error", err)
			e.workQueue.AddAfter(key, statusPersistenceRequeueInterval)
			return true
		}
		utilruntime.HandleError(fmt.Errorf("repack-engine reconcile %q: %w", key, err))
		if e.workQueue.NumRequeues(key) < maxReconcileRetries {
			klog.V(4).InfoS("requeueing RepackRun after error", "run", key, "retries", e.workQueue.NumRequeues(key)+1)
			e.workQueue.AddRateLimited(key)
			return true
		}
		// Poison pill: stop retrying and fail the run so it does not loop forever
		// (and its Execute slot, if any, was already released by process's defer).
		e.workQueue.Forget(key)
		e.failByName(ctx, key, "ReconcileGaveUp", fmt.Errorf("gave up after %d retries: %w", maxReconcileRetries, err))
		return true
	}
	e.workQueue.Forget(key)
	return true
}

func reconcileErrorConsumesRetryBudget(err error) bool {
	return err != nil && !apierrors.IsConflict(err) && !isTerminalStatusPersistenceError(err)
}

// reconcileSafely runs reconcile with panic recovery so a single bad RepackRun
// (e.g. a plugin/snapshot panic) cannot crash the engine's worker goroutine. The
// panic is converted to an error; process's own defers (slot release, session
// close) still run during unwinding before it reaches here.
func (e *Engine) reconcileSafely(ctx context.Context, name string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in reconcile: %v", r)
			klog.ErrorS(err, "repack: recovered panic", "run", name, "stack", string(debug.Stack()))
		}
	}()
	return e.reconcile(ctx, name)
}

// failByName marks a run Failed by name (poison-pill path); best-effort.
func (e *Engine) failByName(ctx context.Context, name, reason string, cause error) {
	run, err := e.repackRunLister.Get(name)
	if err != nil {
		return // gone or lister error; nothing to write
	}
	work := run.DeepCopy()
	if err := e.fail(ctx, work, work.Generation, reason, cause); err != nil {
		klog.ErrorS(err, "repack: persist poison-pill failure", "run", name)
		if isTerminalStatusPersistenceError(err) {
			e.workQueue.AddAfter(name, statusPersistenceRequeueInterval)
		}
	}
}

// reconcile processes one RepackRun: re-check it's still a candidate, apply the
// Execute serialization gate (one-at-a-time + cooldown — it lives here, in the
// worker that actually evicts), then plan/act.
func (e *Engine) reconcile(ctx context.Context, name string) error {
	run, err := e.repackRunLister.Get(name)
	if apierrors.IsNotFound(err) {
		e.forgetPendingTerminalStatus(name)
		return nil
	}
	if err != nil {
		return err
	}
	if desired, found := e.pendingTerminalStatus(name); found {
		work := run.DeepCopy()
		desired.DeepCopyInto(&work.Status)
		if err := e.updateStatusTerminal(ctx, work); err != nil {
			return err
		}
		if work.Spec.Mode == repackv1alpha1.RepackModeExecute {
			e.markExecuteDone(work.Name)
			e.requeueGatedRuns()
			return e.cleanupPlacement(ctx, work)
		}
		return nil
	}
	if isPlacementCleanupCandidate(run) {
		return e.cleanupPlacement(ctx, run.DeepCopy())
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
		if err := e.updateStatus(ctx, work); err != nil {
			return err
		}
	}

	active, lastFinish := false, time.Time{}
	gate := state.GateDecision{Admit: true} // DryRun is never serialized by Execute.
	if work.Spec.Mode == repackv1alpha1.RepackModeExecute {
		gate, active, lastFinish = e.tryAcquireExecute(work.Name, e.now())
	}
	klog.V(4).InfoS("repack: execute gate evaluated", "run", work.Name, "mode", work.Spec.Mode,
		"executeActive", active, "lastExecuteFinish", lastFinish, "cooldown", e.config.Cooldown,
		"admit", gate.Admit, "reason", gate.Reason, "requeueAfter", gate.RequeueAfter)
	if !gate.Admit {
		metrics.ObserveGateRejection(gate.Reason)
		klog.V(3).InfoS("RepackRun deferred by execute gate",
			"run", name, "reason", gate.Reason, "requeueAfter", gate.RequeueAfter)
		message := "Waiting to execute: another Execute RepackRun is active; this run will be retried when the active run finishes."
		if gate.Reason == state.ReasonExecuteCoolingDown {
			message = fmt.Sprintf(
				"Waiting to execute: the previous Execute RepackRun is cooling down; retrying after %s.",
				gate.RequeueAfter.Round(time.Second))
		}
		conditionChanged := state.SetCondition(&work.Status.Conditions, state.CondQueued, metav1.ConditionTrue,
			gate.Reason, message, work.Generation)
		work.Status.Phase = state.DerivePhase(work.Status.Conditions)
		if err := e.updateStatus(ctx, work); err != nil {
			return err
		}
		if conditionChanged {
			e.recordRunEvent(work, v1.EventTypeNormal, gate.Reason, message)
		}
		if gate.RequeueAfter > 0 {
			e.workQueue.AddAfter(name, gate.RequeueAfter)
		}
		return nil
	}
	if work.Spec.Mode == repackv1alpha1.RepackModeExecute {
		klog.V(3).InfoS("repack: Execute slot acquired", "run", work.Name, "cooldown", e.config.Cooldown)
	}
	if isEvictionCandidate(work) {
		// The prepared status may have been persisted immediately before a crash,
		// while lease publication was still in progress. Re-establish both halves
		// of the admission barrier idempotently before resuming any API call.
		if err := e.preparePlacementLeases(ctx, work); err != nil {
			return fmt.Errorf("resume placement leases before eviction: %w", err)
		}
		if err := e.setPlacementActive(ctx, work, true); err != nil {
			return fmt.Errorf("resume placement discovery before eviction: %w", err)
		}
		return e.executePreparedEvictions(ctx, work, work.Generation, e.resolveResource(work))
	}
	if isPlacementCandidate(work) {
		// A crash can occur after the accepted nomination subset becomes durable
		// but before leases for rejected PodGroups are released. Reconcile that
		// one-way cleanup before placement; retained groups remain protected.
		groupsToRelease := placementGroupsDifference(plannedPodGroups(work), placementPodGroups(work))
		if err := e.releasePlacementLeases(ctx, work, groupsToRelease); err != nil {
			return fmt.Errorf("release unused placement leases before placement recovery: %w", err)
		}
		return e.reconcilePlacement(ctx, work)
	}
	return e.process(ctx, work)
}

func (e *Engine) pendingTerminalStatus(name string) (*repackv1alpha1.RepackRunStatus, bool) {
	e.terminalStatusMutex.Lock()
	defer e.terminalStatusMutex.Unlock()
	status, found := e.pendingTerminalStatuses[name]
	if !found {
		return nil, false
	}
	return status.DeepCopy(), true
}

func (e *Engine) rememberPendingTerminalStatus(name string, status *repackv1alpha1.RepackRunStatus) {
	if status == nil {
		return
	}
	e.terminalStatusMutex.Lock()
	if e.pendingTerminalStatuses == nil {
		e.pendingTerminalStatuses = make(map[string]*repackv1alpha1.RepackRunStatus)
	}
	e.pendingTerminalStatuses[name] = status.DeepCopy()
	e.terminalStatusMutex.Unlock()
}

func (e *Engine) forgetPendingTerminalStatus(name string) {
	e.terminalStatusMutex.Lock()
	delete(e.pendingTerminalStatuses, name)
	e.terminalStatusMutex.Unlock()
}

// recoverOrphans fails an interrupted planning/eviction run. Placement runs are
// intentionally recoverable: their durable lease, replacement identity, and
// deadline let a new engine instance safely resume receiver selection without
// repeating any eviction.
func (e *Engine) recoverOrphans(ctx context.Context) {
	runs, err := e.repackRunLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "repack: list for orphan recovery")
		return
	}
	for _, r := range runs {
		if r.Status.Phase != repackv1alpha1.RepackRunning {
			continue
		}
		if isEvictionCandidate(r) || isPlacementCandidate(r) {
			e.workQueue.Add(r.Name)
			klog.V(3).InfoS("recovered in-progress Execute run",
				"run", r.Name, "evictionRecovery", isEvictionCandidate(r))
			continue
		}
		work := r.DeepCopy()
		generation := work.Generation
		const reason = "Interrupted"
		cause := fmt.Errorf("engine restarted while this run was in progress")
		msg := failureStatusMessage(e.resolveResource(work), reason, cause)
		work.Status.Message = msg
		state.SetCondition(&work.Status.Conditions, state.CondProgressing, metav1.ConditionFalse, reason, msg, generation)
		state.SetCondition(&work.Status.Conditions, state.CondFailed, metav1.ConditionTrue, reason, msg, generation)
		work.Status.Phase = state.DerivePhase(work.Status.Conditions)
		stampLifecycle(work, e.now())
		e.rememberPendingTerminalStatus(work.Name, work.Status.DeepCopy())
		e.workQueue.Add(work.Name)
		klog.V(3).InfoS("queued orphaned Running RepackRun for terminal recovery", "run", work.Name)
	}
}
