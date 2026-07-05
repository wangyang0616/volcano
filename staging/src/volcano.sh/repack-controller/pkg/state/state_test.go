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

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

func cond(t string, s metav1.ConditionStatus) metav1.Condition {
	return metav1.Condition{Type: t, Status: s}
}

// Phase is derived from conditions with the documented precedence.
func TestDerivePhase(t *testing.T) {
	tt := metav1.ConditionTrue
	ff := metav1.ConditionFalse
	cases := []struct {
		name  string
		conds []metav1.Condition
		want  repackv1alpha1.RepackPhase
	}{
		{"empty -> Pending", nil, repackv1alpha1.RepackPending},
		{"admitted+queued still Pending",
			[]metav1.Condition{cond(CondAdmitted, tt), cond(CondQueued, tt)},
			repackv1alpha1.RepackPending},
		{"progressing -> Running",
			[]metav1.Condition{cond(CondAdmitted, tt), cond(CondProgressing, tt)},
			repackv1alpha1.RepackRunning},
		{"complete -> Succeeded",
			[]metav1.Condition{cond(CondProgressing, ff), cond(CondComplete, tt)},
			repackv1alpha1.RepackSucceeded},
		{"failed beats complete",
			[]metav1.Condition{cond(CondComplete, tt), cond(CondFailed, tt)},
			repackv1alpha1.RepackFailed},
		{"cancelled beats all",
			[]metav1.Condition{cond(CondComplete, tt), cond(CondFailed, tt), cond(CondCancelled, tt)},
			repackv1alpha1.RepackCancelled},
	}
	for _, c := range cases {
		if got := DerivePhase(c.conds); got != c.want {
			t.Errorf("%s: DerivePhase=%v want %v", c.name, got, c.want)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	term := []repackv1alpha1.RepackPhase{
		repackv1alpha1.RepackSucceeded, repackv1alpha1.RepackFailed, repackv1alpha1.RepackCancelled,
	}
	for _, p := range term {
		if !IsTerminal(p) {
			t.Errorf("%v should be terminal", p)
		}
	}
	for _, p := range []repackv1alpha1.RepackPhase{repackv1alpha1.RepackPending, repackv1alpha1.RepackRunning} {
		if IsTerminal(p) {
			t.Errorf("%v should NOT be terminal", p)
		}
	}
}

func TestEvaluateGate(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)

	// DryRun: never gated, even when an Execute is active.
	if d := EvaluateGate(GateInputs{Mode: repackv1alpha1.RepackModeDryRun, ExecuteActive: true, Now: now}); !d.Admit {
		t.Error("DryRun must never be gated")
	}
	// Execute with K=1 slot busy -> AnotherRunActive.
	if d := EvaluateGate(GateInputs{Mode: repackv1alpha1.RepackModeExecute, ExecuteActive: true, Now: now}); d.Admit || d.Reason != ReasonAnotherRunActive {
		t.Errorf("expected AnotherRunActive, got %+v", d)
	}
	// Execute within cooldown -> ExecuteCoolingDown + precise requeue.
	d := EvaluateGate(GateInputs{
		Mode: repackv1alpha1.RepackModeExecute, Cooldown: 10 * time.Minute,
		LastExecuteFinish: now.Add(-4 * time.Minute), Now: now,
	})
	if d.Admit || d.Reason != ReasonExecuteCoolingDown {
		t.Errorf("expected ExecuteCoolingDown, got %+v", d)
	}
	if d.RequeueAfter != 6*time.Minute {
		t.Errorf("expected RequeueAfter 6m, got %v", d.RequeueAfter)
	}
	// Execute after cooldown elapsed, slot free -> admit.
	if d := EvaluateGate(GateInputs{
		Mode: repackv1alpha1.RepackModeExecute, Cooldown: 10 * time.Minute,
		LastExecuteFinish: now.Add(-30 * time.Minute), Now: now,
	}); !d.Admit {
		t.Errorf("cooldown elapsed should admit, got %+v", d)
	}
}

func TestTTLExpired(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	ttl := int64(3600)
	mk := func(ttlSec *int64, phase repackv1alpha1.RepackPhase, completed *time.Time) *repackv1alpha1.RepackRun {
		r := &repackv1alpha1.RepackRun{}
		r.Spec.TTLSecondsAfterFinished = ttlSec
		r.Status.Phase = phase
		if completed != nil {
			ct := metav1.NewTime(*completed)
			r.Status.CompletionTime = &ct
		}
		return r
	}
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-1 * time.Minute)

	if !TTLExpired(mk(&ttl, repackv1alpha1.RepackSucceeded, &old), now) {
		t.Error("terminal run 2h past a 1h TTL must be expired")
	}
	if TTLExpired(mk(&ttl, repackv1alpha1.RepackSucceeded, &recent), now) {
		t.Error("terminal run within TTL must NOT be expired")
	}
	if TTLExpired(mk(nil, repackv1alpha1.RepackSucceeded, &old), now) {
		t.Error("unset TTL must never expire")
	}
	if TTLExpired(mk(&ttl, repackv1alpha1.RepackRunning, &old), now) {
		t.Error("non-terminal run must not be TTL-collected")
	}
}

func TestSetCondition(t *testing.T) {
	var conds []metav1.Condition
	if !SetCondition(&conds, CondAdmitted, metav1.ConditionTrue, ReasonAdmitted, "ok", 1) {
		t.Error("first set should report changed")
	}
	if DerivePhase(conds) != repackv1alpha1.RepackPending {
		t.Error("admitted-only should still be Pending")
	}
	SetCondition(&conds, CondProgressing, metav1.ConditionTrue, ReasonEvicting, "evicting", 1)
	if DerivePhase(conds) != repackv1alpha1.RepackRunning {
		t.Error("progressing should derive Running")
	}
}
