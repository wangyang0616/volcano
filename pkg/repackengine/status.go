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

package repackengine

import (
	"context"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
)

func (e *Engine) fail(run *repackv1alpha1.RepackRun, generation int64, reason string, err error) {
	klog.ErrorS(err, "repack: run failed", "run", run.Name, "reason", reason)
	state.SetCondition(&run.Status.Conditions, state.CondProgressing, metav1.ConditionFalse, reason, err.Error(), generation)
	state.SetCondition(&run.Status.Conditions, state.CondFailed, metav1.ConditionTrue, reason, err.Error(), generation)
	run.Status.Phase = state.DerivePhase(run.Status.Conditions)
	e.updateStatusTerminal(run)
}

func (e *Engine) updateStatus(run *repackv1alpha1.RepackRun) {
	stampLifecycle(run, time.Now())
	desired := run.Status.DeepCopy()
	name := run.Name
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := e.vc.RepackV1alpha1().RepackRuns().Get(context.Background(), name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil // deleted (e.g. TTL GC); nothing to write
		}
		if err != nil {
			return err
		}
		// Re-apply the intended status onto the freshest object so a concurrent
		// write (e.g. the controller, or our own later update) does not 409 us.
		desired.DeepCopyInto(&latest.Status)
		_, err = e.vc.RepackV1alpha1().RepackRuns().UpdateStatus(context.Background(), latest, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		klog.ErrorS(err, "repack: update status", "run", name)
	}
}

// updateStatusTerminal writes a terminal status with conflict retry. Intermediate
// writes are best-effort (a later reconcile re-derives them), but losing the
// TERMINAL write would strand the run in Running until an orphan recovery — so on
// conflict re-read the latest object and re-apply the computed status.
func (e *Engine) updateStatusTerminal(run *repackv1alpha1.RepackRun) {
	stampLifecycle(run, time.Now())
	outcome := terminalOutcome(run)
	metrics.ObserveRun(string(run.Spec.Mode), outcome)
	if e.recorder != nil {
		etype := v1.EventTypeNormal
		if run.Status.Phase == repackv1alpha1.RepackFailed {
			etype = v1.EventTypeWarning
		}
		e.recorder.Event(run, etype, outcome, "repack run reached a terminal state")
	}
	desired := run.Status.DeepCopy()
	name := run.Name
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := e.vc.RepackV1alpha1().RepackRuns().Get(context.Background(), name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil // deleted (e.g. TTL GC); nothing to write
		}
		if err != nil {
			return err
		}
		desired.DeepCopyInto(&latest.Status)
		_, err = e.vc.RepackV1alpha1().RepackRuns().UpdateStatus(context.Background(), latest, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		klog.ErrorS(err, "repack: write terminal status", "run", name)
	}
}

// terminalOutcome is the reason of the True Complete/Failed/Cancelled condition
// (the run's terminal verdict) for the runs_total metric; "Unknown" if none.
func terminalOutcome(run *repackv1alpha1.RepackRun) string {
	for _, c := range run.Status.Conditions {
		if c.Status == metav1.ConditionTrue &&
			(c.Type == state.CondComplete || c.Type == state.CondFailed || c.Type == state.CondCancelled) {
			return c.Reason
		}
	}
	return "Unknown"
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
			// Report whole devices to users; Resreq is stored in milli-units.
			cards = engineapi.Cards(m.Task.Resreq, res)
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
		_, podGroupName := splitJobID(string(in.Gang))
		out = append(out, repackv1alpha1.PodNomination{
			Namespace:      in.Namespace,
			PodGroupName:   podGroupName,
			VictimPodName:  in.PodName,
			IdentityLabels: in.IdentityLabels,
			NodeName:       in.Node,
			Phase:          "Pending",
			ExpirationTime: &expire,
		})
	}
	return out
}
