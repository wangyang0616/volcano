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
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// Built-in action names (config repack.actions).
const ActionRepack = "repack"

// Action is one ordered Repack workflow entry (mirrors framework.Action in the
// scheduler). An Action owns the business flow; Runtime supplies the controller
// primitives required to persist and execute that flow.
type Action interface {
	Name() string
	Execute(ctx *ActionContext) ActionResult
}

// ActionContext is the per-RepackRun input shared by configured actions. A
// planning Session is opened lazily by Runtime so eviction/placement recovery
// does not rebuild a cluster-wide scheduler snapshot unnecessarily.
type ActionContext struct {
	Context context.Context
	Run     *repackv1alpha1.RepackRun
	Runtime Runtime

	holdExecuteSlot bool
}

// HoldExecuteSlot marks that this Action has entered a durable Execute stage.
// It must be called before invoking a Runtime operation that may panic or return
// asynchronously; the Engine reads the flag from its defer path as well as the
// normal return path, so a recovered panic cannot accidentally start cooldown.
func (c *ActionContext) HoldExecuteSlot() {
	if c != nil {
		c.holdExecuteSlot = true
	}
}

func (c *ActionContext) ExecuteSlotHeld() bool {
	return c != nil && c.holdExecuteSlot
}

// ActionResult tells the Engine how to drive the controller after an Action
// returns. Execute-slot ownership is marked synchronously on ActionContext so it
// also survives a panic before an ActionResult can be returned.
type ActionResult struct {
	Stop         bool
	Requeue      bool
	RequeueAfter time.Duration
	Err          error
}

// RuntimeResult is returned by long-lived execution primitives. Runtime
// observes the detailed state; Action maps the result into its workflow result;
// Engine remains the only component that mutates the workqueue.
type RuntimeResult struct {
	Requeue      bool
	RequeueAfter time.Duration
	Err          error
}

// PlanningCycle owns the plugin-populated planning Session and its backing
// scheduler snapshot. Close must be called exactly once when planning ends.
type PlanningCycle struct {
	Session       *Session
	Resource      v1.ResourceName
	ResolvedScope *repackv1alpha1.ResolvedScope
	Close         func()
}

// Runtime groups the controller ports used by Actions. Keeping the groups
// separate makes dependency ownership explicit without exposing Kubernetes
// clients, queues or the Engine implementation to an Action.
type Runtime interface {
	PlanningRuntime
	StatusRuntime
	ExecutionRuntime
}

type PlanningRuntime interface {
	OpenPlanningCycle(context.Context, *repackv1alpha1.RepackRun) (*PlanningCycle, error)
	ResolveMoveOwners(context.Context, *api.RepackPlan) map[string]*repackv1alpha1.WorkloadRef
	RecordPlanComputed(*repackv1alpha1.RepackRun)
}

// ActionError carries the terminal Condition reason for a failure discovered by
// a Runtime primitive without making the Action infer it from an error string.
type ActionError struct {
	Reason string
	Err    error
}

func (e *ActionError) Error() string {
	if e == nil || e.Err == nil {
		return "repack action failed"
	}
	return e.Err.Error()
}
func (e *ActionError) Unwrap() error { return e.Err }

func NewActionError(reason string, err error) error {
	if err == nil {
		return nil
	}
	return &ActionError{Reason: reason, Err: err}
}

func ActionErrorReason(err error) string {
	var actionErr *ActionError
	if errors.As(err, &actionErr) {
		return actionErr.Reason
	}
	return ""
}

type StatusRuntime interface {
	UpdateStatus(context.Context, *repackv1alpha1.RepackRun) error
	UpdateTerminalStatus(context.Context, *repackv1alpha1.RepackRun) error
	Fail(context.Context, *repackv1alpha1.RepackRun, string, error) error
}

type ExecutionRuntime interface {
	PrepareExecution(context.Context, *repackv1alpha1.RepackRun, *api.RepackPlan, Snapshot) error
	ExecutePreparedEvictions(context.Context, *repackv1alpha1.RepackRun, v1.ResourceName) RuntimeResult
	ResumePreparedEvictions(context.Context, *repackv1alpha1.RepackRun) RuntimeResult
	ReconcilePlacement(context.Context, *repackv1alpha1.RepackRun) RuntimeResult
	CleanupPlacement(context.Context, *repackv1alpha1.RepackRun) error
}

// ActionRegistration declares the implementation and semantic plugin
// capabilities required to execute an Action meaningfully.
type ActionRegistration struct {
	Factory  func() Action
	Requires []PluginCapability
}

var actionRegistry = map[string]ActionRegistration{}

// RegisterAction registers an action factory and its capability requirements
// under a config name.
func RegisterAction(name string, registration ActionRegistration) {
	registration.Requires = append([]PluginCapability(nil), registration.Requires...)
	actionRegistry[name] = registration
}

// GetAction returns a fresh action for the config name, ok=false if unknown.
func GetAction(name string) (Action, bool) {
	registration, ok := actionRegistry[name]
	if !ok || registration.Factory == nil {
		return nil, false
	}
	return registration.Factory(), true
}

// ActionRequires returns a copy of the plugin capabilities required by an
// action. Unknown actions return nil and are rejected separately by validation.
func ActionRequires(name string) []PluginCapability {
	return append([]PluginCapability(nil), actionRegistry[name].Requires...)
}

// ActionNames lists registered action names, sorted.
func ActionNames() []string {
	out := make([]string, 0, len(actionRegistry))
	for n := range actionRegistry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DefaultActions is the default pipeline when config names none.
func DefaultActions() []string { return []string{ActionRepack} }

// RunActions executes the named workflow actions in order. The first Action to
// request Stop terminates the pipeline. Unknown names are skipped with a warning;
// startup validation rejects them in production.
func RunActions(names []string, ctx *ActionContext) ActionResult {
	if len(names) == 0 {
		names = DefaultActions()
	}
	for _, name := range names {
		if ctx == nil || ctx.Context == nil || ctx.Context.Err() != nil {
			if ctx != nil && ctx.Context != nil {
				return ActionResult{Stop: true, Err: ctx.Context.Err()}
			}
			return ActionResult{Stop: true}
		}
		a, ok := GetAction(name)
		if !ok {
			klog.ErrorS(nil, "repack: unknown action in config, skipping", "action", name, "registered", ActionNames())
			continue
		}
		result := a.Execute(ctx)
		if result.Err != nil || result.Stop {
			return result
		}
	}
	return ActionResult{}
}
