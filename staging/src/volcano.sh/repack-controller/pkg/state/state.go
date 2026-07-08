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

// Package state holds the pure lifecycle logic of a RepackRun controller:
// deriving phase from conditions, gating Execute runs (global K=1 + cooldown),
// admission validation, and TTL/GC decisions. Everything here is a pure function
// of its inputs (no clientset, no informers), so the state machine is fully
// unit-testable and the reconcile loop stays a thin shell around it.
//
// Authority rule (§4.6): conditions are the source of truth; phase is a derived
// projection. Writers update conditions first, then recompute phase from them.
package state

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

// Condition types (Job-style; §4.6.1). Admission is enforced at the apiserver
// (CEL on the CRD), so there is no controller-side Admitted condition.
const (
	CondQueued      = "Queued"
	CondProgressing = "Progressing"
	CondComplete    = "Complete"
	CondFailed      = "Failed"
	CondCancelled   = "Cancelled"
)

// Condition reasons (§4.6.1).
const (
	// ReasonSlotAcquired clears Queued once the K=1/cooldown gate admits the run.
	ReasonSlotAcquired = "SlotAcquired"
	// Queued reasons — only Execute is gated; DryRun never queues.
	ReasonAnotherRunActive   = "AnotherRunActive"  // K=1 occupied
	ReasonExecuteCoolingDown = "ExecuteCoolingDown" // cooldown not elapsed
	ReasonWaitingForLeader   = "WaitingForLeader"
	// Progressing sub-reasons.
	ReasonSimulating = "Simulating" // DryRun
	ReasonEvicting   = "Evicting"   // Execute
	// Terminal Complete reasons. These double as the "worth repacking?" verdict
	// (proposal §5.2.2), so there is no separate status.plan.summary.verdict.
	ReasonRepackRecommended  = "RepackRecommended"  // DryRun found a worthwhile plan
	ReasonExecuted           = "Executed"           // Execute performed the repack
	ReasonNoFragmentation    = "NoFragmentation"    // clean: nothing to defragment
	ReasonBelowGoalThreshold = "BelowGoalThreshold" // fragmented but below the benefit gate
	ReasonCancelledByUser    = "CancelledByUser"
	// ReasonExecuteFailed is terminal-Failed: a worthwhile plan was found but every
	// eviction was rejected (e.g. by PDBs), so the repack achieved nothing.
	ReasonExecuteFailed = "ExecuteFailed"
)

// DerivePhase projects conditions onto the coarse phase (§4.6.1). Precedence:
// Cancelled > Failed > Complete(=Succeeded) > Progressing(=Running) > Pending.
// Admitted/Queued never advance the phase past Pending.
func DerivePhase(conds []metav1.Condition) repackv1alpha1.RepackPhase {
	switch {
	case meta.IsStatusConditionTrue(conds, CondCancelled):
		return repackv1alpha1.RepackCancelled
	case meta.IsStatusConditionTrue(conds, CondFailed):
		return repackv1alpha1.RepackFailed
	case meta.IsStatusConditionTrue(conds, CondComplete):
		return repackv1alpha1.RepackSucceeded
	case meta.IsStatusConditionTrue(conds, CondProgressing):
		return repackv1alpha1.RepackRunning
	default:
		return repackv1alpha1.RepackPending
	}
}

// IsTerminal reports whether a phase is a final state.
func IsTerminal(p repackv1alpha1.RepackPhase) bool {
	switch p {
	case repackv1alpha1.RepackSucceeded,
		repackv1alpha1.RepackFailed,
		repackv1alpha1.RepackCancelled:
		return true
	default:
		return false
	}
}

// SetCondition upserts a condition and returns whether it changed. Convenience
// wrapper that stamps ObservedGeneration so stale conditions are detectable.
func SetCondition(conds *[]metav1.Condition, condType string, status metav1.ConditionStatus,
	reason, message string, observedGeneration int64) bool {
	return meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
	})
}

// Admission is enforced at the apiserver via CEL/markers on the CRD (mode enum,
// goals MaxItems, spec immutability), so there is no controller-side admission
// function here. Scope is optional in both modes; an omitted scope means
// whole-cluster and the engine's plan (maxPerRun/cooldown/K=1/PDB) bounds the blast
// radius, so there is no "Execute requires a scope" rule.

// GateInputs are the facts needed to decide whether a Pending run may claim a
// worker slot now. They come from the controller's view of the world.
type GateInputs struct {
	Mode repackv1alpha1.RepackMode
	// ExecuteActive is true when another Execute run currently holds the global
	// K=1 slot (Running, or claimed). DryRun ignores this.
	ExecuteActive bool
	// LastExecuteFinish is when the most recent Execute run reached terminal; the
	// zero value means "never".
	LastExecuteFinish time.Time
	// Cooldown is the minimum gap after an Execute before the next may start.
	Cooldown time.Duration
	Now      time.Time
}

// GateDecision is the outcome of EvaluateGate.
type GateDecision struct {
	Admit  bool   // may claim a slot and proceed to Running
	Reason string // when !Admit, the Queued condition reason
	// RequeueAfter, when >0 and !Admit, is how long until the cooldown elapses
	// (the controller can requeue precisely instead of polling).
	RequeueAfter time.Duration
}

// EvaluateGate implements the queue gate (§4.5/§4.6.1): DryRun is never gated;
// Execute is serialized by a global K=1 slot and a post-Execute cooldown.
func EvaluateGate(in GateInputs) GateDecision {
	if in.Mode != repackv1alpha1.RepackModeExecute {
		return GateDecision{Admit: true} // DryRun: free to run
	}
	if in.ExecuteActive {
		return GateDecision{Admit: false, Reason: ReasonAnotherRunActive}
	}
	if in.Cooldown > 0 && !in.LastExecuteFinish.IsZero() {
		ready := in.LastExecuteFinish.Add(in.Cooldown)
		if in.Now.Before(ready) {
			return GateDecision{Admit: false, Reason: ReasonExecuteCoolingDown, RequeueAfter: ready.Sub(in.Now)}
		}
	}
	return GateDecision{Admit: true}
}

// TTLExpired reports whether a finished run is past its TTL and should be
// deleted by RunGC (§4.5.3). Unset TTL = never auto-delete; TTL=0 = delete as
// soon as completionTime is set.
func TTLExpired(run *repackv1alpha1.RepackRun, now time.Time) bool {
	if run == nil || run.Spec.TTLSecondsAfterFinished == nil {
		return false
	}
	if !IsTerminal(run.Status.Phase) || run.Status.CompletionTime == nil {
		return false
	}
	deadline := run.Status.CompletionTime.Time.Add(time.Duration(*run.Spec.TTLSecondsAfterFinished) * time.Second)
	return !now.Before(deadline)
}

// DefaultExecuteCooldown mirrors the engine's --repack-execute-cooldown default.
// GC keeps a finished Execute run alive for at least this long (see
// CooldownRetained) so the engine can still read its status.completionTime as the
// cooldown anchor — including after a restart, where the engine's in-memory anchor
// is gone. Keep in sync with the engine flag default.
const DefaultExecuteCooldown = 10 * time.Minute

// CooldownRetained reports whether a terminal run must be kept to preserve the
// Execute cooldown anchor: an Execute run whose completionTime + cooldown has not
// yet passed. Without this, a short TTL (TTL < cooldown) could delete the most
// recent finished Execute before the window ends, and a restart-recovered engine
// — which rebuilds the anchor solely from persisted completionTime — would forget
// the cooldown and admit the next Execute too early. Non-Execute runs, a
// non-positive cooldown, or a missing completionTime are never retained here.
func CooldownRetained(run *repackv1alpha1.RepackRun, cooldown time.Duration, now time.Time) bool {
	if run == nil || cooldown <= 0 || run.Spec.Mode != repackv1alpha1.RepackModeExecute {
		return false
	}
	if !IsTerminal(run.Status.Phase) || run.Status.CompletionTime == nil {
		return false
	}
	return now.Before(run.Status.CompletionTime.Time.Add(cooldown))
}

// CooldownRemaining is how long until the cooldown-retention floor lifts (0 when
// not retained), so the controller can requeue precisely at expiry.
func CooldownRemaining(run *repackv1alpha1.RepackRun, cooldown time.Duration, now time.Time) time.Duration {
	if !CooldownRetained(run, cooldown, now) {
		return 0
	}
	return run.Status.CompletionTime.Time.Add(cooldown).Sub(now)
}
