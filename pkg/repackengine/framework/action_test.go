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

package framework

import (
	"context"
	"errors"
	"testing"
)

type contextAwareTestAction struct {
	executions *int
}

func TestActionErrorPreservesReasonAndCause(t *testing.T) {
	cause := errors.New("invalid target")
	err := NewActionError("InvalidConfiguration", cause)
	if ActionErrorReason(err) != "InvalidConfiguration" || !errors.Is(err, cause) {
		t.Fatalf("error=%v reason=%q, want preserved reason and cause", err, ActionErrorReason(err))
	}
}

func (*contextAwareTestAction) Name() string { return "context-aware-test" }

func (action *contextAwareTestAction) Execute(*ActionContext) ActionResult {
	(*action.executions)++
	return ActionResult{}
}

func TestRunActionsStopsWhenContextIsCancelled(t *testing.T) {
	executions := 0
	RegisterAction("context-aware-test", ActionRegistration{
		Factory: func() Action { return &contextAwareTestAction{executions: &executions} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	RunActions([]string{"context-aware-test"}, &ActionContext{Context: ctx})
	if executions != 0 {
		t.Fatalf("action executions = %d, want 0 after cancellation", executions)
	}
}

func TestActionContextHoldsExecuteSlotSynchronously(t *testing.T) {
	ctx := &ActionContext{}
	if ctx.ExecuteSlotHeld() {
		t.Fatal("new ActionContext must not hold the Execute slot")
	}
	ctx.HoldExecuteSlot()
	if !ctx.ExecuteSlotHeld() {
		t.Fatal("HoldExecuteSlot must be visible immediately to reconcile's defer path")
	}
}
