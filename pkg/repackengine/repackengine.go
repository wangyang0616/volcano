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
// the scheduler-coupled adapters in session/.
package repackengine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcinformers "volcano.sh/apis/pkg/client/informers/externalversions"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/session"
	"volcano.sh/volcano/pkg/scheduler"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedcache "volcano.sh/volcano/pkg/scheduler/cache"
	"volcano.sh/volcano/pkg/scheduler/conf"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
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
	queue   workqueue.TypedRateLimitingInterface[string]
	now     func() time.Time

	mu             sync.Mutex
	tiers          []conf.Tier
	configurations []conf.Configuration
}

// NewEngine builds the engine, wires the RepackRun informer, and applies defaults.
func NewEngine(config *rest.Config, cfg Config) (*Engine, error) {
	if cfg.Core == "" {
		cfg.Core = engineframework.CoreDrain
	}
	if len(cfg.Plugins) == 0 {
		cfg.Plugins = []string{"base", "node", "gang"}
	}
	if cfg.NominationTTL <= 0 {
		cfg.NominationTTL = 10 * time.Minute
	}
	vc := vcclientset.NewForConfigOrDie(config)
	factory := vcinformers.NewSharedInformerFactory(vc, cfg.ResyncPeriod)
	informer := factory.Repack().V1alpha1().RepackRuns()
	e := &Engine{
		cache:   schedcache.New(config, nil, "", nil, 0, nil, 0, 0),
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
	klog.InfoS("repack-engine started (event-driven)", "core", e.cfg.Core, "plugins", e.cfg.Plugins)
	// Single worker: Execute runs serialize naturally (one reconcile at a time).
	go func() {
		for e.processNext(ctx) {
		}
	}()
	<-ctx.Done()
	klog.InfoS("repack-engine shutting down")
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
	if !ok || !candidate(run) {
		return
	}
	e.queue.Add(run.Name)
}

// candidate reports whether a run is ready for the engine: not yet processed
// (phase empty or Pending) and not terminal/Running. Admission is enforced by
// CEL at the apiserver, so any RepackRun that exists is already valid.
func candidate(run *repackv1alpha1.RepackRun) bool {
	p := run.Status.Phase
	return p == "" || p == repackv1alpha1.RepackPending
}

func (e *Engine) processNext(ctx context.Context) bool {
	key, shutdown := e.queue.Get()
	if shutdown {
		return false
	}
	defer e.queue.Done(key)
	if err := e.reconcile(ctx, key); err != nil {
		utilruntime.HandleError(fmt.Errorf("repack-engine reconcile %q: %w", key, err))
		e.queue.AddRateLimited(key)
		return true
	}
	e.queue.Forget(key)
	return true
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
	if !candidate(run) {
		return nil // already picked up / terminal
	}
	work := run.DeepCopy()

	// Acknowledge as Pending so `kubectl get repackrun` shows a phase before the
	// engine starts (deferred Execute runs also settle here via the gate below).
	if work.Status.Phase == "" {
		work.Status.Phase = repackv1alpha1.RepackPending
		e.updateStatus(work)
	}

	active, lastFinish := e.executeState(work.Name)
	gate := state.EvaluateGate(state.GateInputs{
		Mode:              work.Spec.Mode,
		ExecuteActive:     active,
		LastExecuteFinish: lastFinish,
		Cooldown:          e.cfg.Cooldown,
		Now:               e.now(),
	})
	if !gate.Admit {
		state.SetCondition(&work.Status.Conditions, state.CondQueued, metav1.ConditionTrue,
			gate.Reason, "waiting for an execute slot", work.Generation)
		work.Status.Phase = state.DerivePhase(work.Status.Conditions)
		e.updateStatus(work)
		if gate.RequeueAfter > 0 {
			e.queue.AddAfter(name, gate.RequeueAfter)
		}
		return nil
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
		e.updateStatus(work)
		klog.InfoS("recovered orphaned Running RepackRun -> Failed", "name", work.Name)
	}
}

// executeState scans for the Execute gate: whether another Execute is currently
// Running, and the most recent terminal Execute completion (cooldown anchor).
func (e *Engine) executeState(self string) (active bool, lastFinish time.Time) {
	runs, err := e.lister.List(labels.Everything())
	if err != nil {
		return true, time.Time{} // conservative: assume busy
	}
	for _, r := range runs {
		if r.Spec.Mode != repackv1alpha1.RepackModeExecute {
			continue
		}
		if r.Name != self && r.Status.Phase == repackv1alpha1.RepackRunning {
			active = true
		}
		if state.IsTerminal(r.Status.Phase) && r.Status.CompletionTime != nil {
			if t := r.Status.CompletionTime.Time; t.After(lastFinish) {
				lastFinish = t
			}
		}
	}
	return active, lastFinish
}

// process plans and acts on a cleared run (the gate already passed).
func (e *Engine) process(work *repackv1alpha1.RepackRun) {
	e.mu.Lock()
	tiers, cfgs := e.tiers, e.configurations
	e.mu.Unlock()

	sched := schedframework.OpenSession(e.cache, tiers, cfgs)
	defer schedframework.CloseSession(sched)

	gen := work.Generation
	res := e.resolveResource(work)

	reason := state.ReasonSimulating
	if work.Spec.Mode == repackv1alpha1.RepackModeExecute {
		reason = state.ReasonEvicting
	}
	state.SetCondition(&work.Status.Conditions, state.CondQueued, metav1.ConditionFalse, state.ReasonAdmitted, "cleared", gen)
	state.SetCondition(&work.Status.Conditions, state.CondProgressing, metav1.ConditionTrue, reason, "engine started", gen)
	work.Status.Phase = state.DerivePhase(work.Status.Conditions)
	e.updateStatus(work)

	inScope, nodeInScope, err := engineframework.ResolveScope(work.Spec.Scope, session.SessionGangInfo(sched))
	if err != nil {
		e.fail(work, gen, "ScopeError", err)
		return
	}

	snap := session.NewSessionSnapshot(sched, res, nodeInScope)
	maxPG, maxRes := maxPerRun(work, res)
	esn := engineframework.OpenSession(engineframework.SessionConfig{
		Snapshot:      snap,
		Run:           work,
		Resource:      res,
		Mode:          work.Spec.Mode,
		CoreName:      e.cfg.Core,
		MinNodesFreed: e.cfg.MinNodesFreed,
		MaxPodGroups:  maxPG,
		MaxResource:   maxRes,
		Hooks:         hooksFor(work.Spec.Mode, e.cache.Client()),
	}, e.cfg.Plugins)
	esn.AddMovableFn(func(t *schedapi.TaskInfo) bool { return inScope(t.Job) })
	defer engineframework.CloseSession(esn)

	// The repack action runs the core and (Execute) evicts via Hooks; open-loop —
	// a failed eviction is recorded, not fatal.
	engineframework.RunActions(e.cfg.Actions, esn)

	report, plan := esn.Report(), esn.Plan()
	// The Complete reason doubles as the "worth repacking?" verdict (§5.2.2);
	// there is no summary.verdict. worthwhile = the plan freed nodes; an empty
	// plan splits by whether fragmentation existed: none (clean) vs below the
	// benefit gate (fragmented but not worth acting on).
	worthwhile := report.NodesFreed > 0
	execute := work.Spec.Mode == repackv1alpha1.RepackModeExecute
	ttl := time.Duration(0)
	if execute {
		ttl = e.cfg.NominationTTL
	}
	applyPlan(work, report, plan, res, execute, ttl)
	var done string
	switch {
	case !worthwhile && report.FragRateBefore > 0:
		done = state.ReasonBelowGoalThreshold
	case !worthwhile:
		done = state.ReasonNoFragmentation
	case execute:
		done = state.ReasonExecuted
	default:
		done = state.ReasonRepackRecommended
	}
	state.SetCondition(&work.Status.Conditions, state.CondProgressing, metav1.ConditionFalse, done, "engine finished", gen)
	state.SetCondition(&work.Status.Conditions, state.CondComplete, metav1.ConditionTrue, done, "engine finished", gen)
	work.Status.Phase = state.DerivePhase(work.Status.Conditions)
	e.updateStatus(work)
}

// hooksFor returns the commit side effects. DryRun: none. Execute: evict each
// victim via the Eviction API (PDB-respecting; the workload controller then
// recreates the pod, steered by the nomination reconciler). No reservation/taint.
func hooksFor(mode repackv1alpha1.RepackMode, kube kubernetes.Interface) engineframework.CommitHooks {
	if mode != repackv1alpha1.RepackModeExecute {
		return engineframework.CommitHooks{}
	}
	return engineframework.CommitHooks{
		Evict: func(m *engineapi.Move) error {
			if m == nil || m.Task == nil || m.Task.Pod == nil {
				return nil
			}
			pod := m.Task.Pod
			return kube.PolicyV1().Evictions(pod.Namespace).Evict(context.Background(), &policyv1.Eviction{
				ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
			})
		},
	}
}

func (e *Engine) resolveResource(run *repackv1alpha1.RepackRun) v1.ResourceName {
	if len(run.Spec.Goals) > 0 && run.Spec.Goals[0].Resource != "" {
		return run.Spec.Goals[0].Resource
	}
	return v1.ResourceName(e.cfg.DefaultResource)
}

func (e *Engine) fail(run *repackv1alpha1.RepackRun, gen int64, reason string, err error) {
	klog.ErrorS(err, "repack-engine: run failed", "run", run.Name, "reason", reason)
	state.SetCondition(&run.Status.Conditions, state.CondProgressing, metav1.ConditionFalse, reason, err.Error(), gen)
	state.SetCondition(&run.Status.Conditions, state.CondFailed, metav1.ConditionTrue, reason, err.Error(), gen)
	run.Status.Phase = state.DerivePhase(run.Status.Conditions)
	e.updateStatus(run)
}

func (e *Engine) updateStatus(run *repackv1alpha1.RepackRun) {
	stampLifecycle(run, time.Now())
	if _, err := e.vc.RepackV1alpha1().RepackRuns().UpdateStatus(context.Background(), run, metav1.UpdateOptions{}); err != nil {
		klog.ErrorS(err, "repack-engine: update status", "run", run.Name)
	}
}

// stampLifecycle records StartTime on first Running and CompletionTime on first
// terminal — the anchors the controller's TTL-GC and Execute cooldown (K=1) key
// off. Both stamps are nil-guarded so the engine and the controller (which guards
// the same fields) never clobber each other: whoever reaches the state first wins.
func stampLifecycle(run *repackv1alpha1.RepackRun, now time.Time) {
	if run.Status.Phase == repackv1alpha1.RepackRunning && run.Status.StartTime == nil {
		t := metav1.NewTime(now)
		run.Status.StartTime = &t
	}
	if state.IsTerminal(run.Status.Phase) && run.Status.CompletionTime == nil {
		t := metav1.NewTime(now)
		run.Status.CompletionTime = &t
	}
}

// applyPlan maps the search outcome onto status.plan — the SAME shape for both
// modes: DryRun = predicted plan, Execute = executed plan. Each move carries the
// planned target node (fromNode -> toNode), visible in DryRun too. Execute also
// writes the durable status.nominations[] (consumed by the controller's nomination
// reconciler) and marks freed nodes as actuallyFreed. Per-move actual-landing /
// drift (outcome/actualNode) is filled later by the reconciler as replacement pods
// land; the engine's initial write leaves it empty.
func applyPlan(run *repackv1alpha1.RepackRun, report engineframework.Report, plan *engineapi.RepackPlan, res v1.ResourceName, execute bool, ttl time.Duration) {
	moves := movesOf(plan, res)
	summary := summaryOf(report)
	if summary != nil {
		var cards int64
		for _, m := range moves {
			cards += m.Cards
		}
		summary.MovedCardCount = cards
	}
	run.Status.Plan = &repackv1alpha1.RepackPlan{
		Summary:    summary,
		Moves:      moves,
		FreedNodes: freedNodesOf(plan),
	}
	if execute {
		run.Status.Nominations = nominationsOf(plan, ttl)
	}
}

// movesOf groups the plan's per-task relocations into per-PodGroup status moves;
// fromNode/toNode live per-pod in pods[] (a gang's pods may spread across nodes).
// moves is a pure plan (identical in DryRun/Execute). Deterministic order.
func movesOf(plan *engineapi.RepackPlan, res v1.ResourceName) []repackv1alpha1.RepackMove {
	if plan == nil {
		return nil
	}
	idx := map[string]int{} // JobID ("ns/name") -> index in out
	out := []repackv1alpha1.RepackMove{}
	for _, m := range plan.Moves {
		if m == nil || m.Task == nil || m.To == m.From {
			continue
		}
		job := string(m.Task.Job)
		i, ok := idx[job]
		if !ok {
			i = len(out)
			idx[job] = i
			ns, name := splitJobID(job)
			out = append(out, repackv1alpha1.RepackMove{Namespace: ns, PodGroupName: name})
		}
		var cards int64
		if m.Task.Resreq != nil {
			cards = int64(m.Task.Resreq.Get(res))
		}
		out[i].Cards += cards
		out[i].Pods = append(out[i].Pods, repackv1alpha1.PodMove{
			Name:     m.Task.Name,
			FromNode: m.From,
			ToNode:   m.To,
			Cards:    cards,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].PodGroupName < out[j].PodGroupName
	})
	for k := range out {
		pods := out[k].Pods
		sort.Slice(pods, func(a, b int) bool {
			switch {
			case pods[a].Name != pods[b].Name:
				return pods[a].Name < pods[b].Name
			case pods[a].FromNode != pods[b].FromNode:
				return pods[a].FromNode < pods[b].FromNode
			default:
				return pods[a].ToNode < pods[b].ToNode
			}
		})
	}
	return out
}

// splitJobID splits a "namespace/name" JobID; missing "/" -> ("", id).
func splitJobID(id string) (ns, name string) {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[:i], id[i+1:]
	}
	return "", id
}

// freedNodesOf lists the names of nodes the plan empties (sorted).
func freedNodesOf(plan *engineapi.RepackPlan) []string {
	if plan == nil {
		return nil
	}
	out := append([]string(nil), plan.FreedNodes...)
	sort.Strings(out)
	return out
}

// maxPerRun reads the run's blast-radius caps for the target resource (0 = unset
// = unlimited), feeding the core's disruption budget.
func maxPerRun(run *repackv1alpha1.RepackRun, res v1.ResourceName) (maxPG int, maxRes int64) {
	if run.Spec.MaxPerRun == nil {
		return 0, 0
	}
	if run.Spec.MaxPerRun.PodGroups != nil {
		maxPG = int(*run.Spec.MaxPerRun.PodGroups)
	}
	if q, ok := run.Spec.MaxPerRun.Resources[res]; ok {
		maxRes = q.Value()
	}
	return maxPG, maxRes
}

// summaryOf renders the flat metrics layer. "Worth repacking?" is not here — it
// is folded into the terminal condition's reason. MovedCardCount is filled by
// applyPlan from moves; FragBefore/After come from the report (absolute rate).
func summaryOf(r engineframework.Report) *repackv1alpha1.RepackSummary {
	return &repackv1alpha1.RepackSummary{
		FragBeforePercent: pct(r.FragRateBefore),
		FragAfterPercent:  pct(r.FragRateAfter),
		FreedNodeCount:    int32(r.NodesFreed),
	}
}

// pct rounds a 0-1 fraction to an integer percentage point, clamped to [0,100].
func pct(f float64) int32 {
	p := int32(f*100 + 0.5)
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// nominationsOf renders per-pod landing-steering intents (Execute-only). Claiming
// follows the landing-identity contract (proposal §5.2.2): victimPodName exact
// match, then identityLabels (label-superset match), then fungible. IdentityLabels
// are resolved from the victim pod's own well-known labels by the framework.
func nominationsOf(plan *engineapi.RepackPlan, ttl time.Duration) []repackv1alpha1.PodNomination {
	if plan == nil {
		return nil
	}
	expire := metav1.NewTime(time.Now().Add(ttl))
	intents := engineframework.NominationIntents(plan)
	out := make([]repackv1alpha1.PodNomination, 0, len(intents))
	for _, in := range intents {
		_, pgName := splitJobID(string(in.Gang))
		out = append(out, repackv1alpha1.PodNomination{
			Namespace:      in.Namespace,
			PodGroupName:   pgName,
			VictimPodName:  in.PodName,
			IdentityLabels: in.IdentityLabels,
			NodeName:       in.Node,
			Phase:          "Pending",
			ExpirationTime: &expire,
		})
	}
	return out
}
