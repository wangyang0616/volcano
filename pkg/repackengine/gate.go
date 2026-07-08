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
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
)

// executeGateState is the authoritative K=1 gate view: it combines this engine's
// in-memory state (which does not lag the informer cache) with the cache scan
// (which covers history across restarts / other leaders). active OR-s both; the
// cooldown anchor takes the latest of the two, so a just-finished Execute's
// cooldown is enforced even before its status write reaches the lister.
func (e *Engine) executeGateState(self string) (active bool, lastFinish time.Time) {
	e.mu.Lock()
	imActive := e.execActive != "" && e.execActive != self
	lastFinish = e.lastExecFinish
	e.mu.Unlock()

	cacheActive, cacheFinish := e.executeState(self)
	active = imActive || cacheActive
	if cacheFinish.After(lastFinish) {
		lastFinish = cacheFinish
	}
	return active, lastFinish
}

// markExecuteActive claims the in-memory K=1 slot for an Execute run.
func (e *Engine) markExecuteActive(name string) {
	e.mu.Lock()
	e.execActive = name
	e.mu.Unlock()
}

// markExecuteDone releases the slot and stamps the cooldown anchor.
func (e *Engine) markExecuteDone(name string) {
	e.mu.Lock()
	if e.execActive == name {
		e.execActive = ""
	}
	e.lastExecFinish = e.now()
	e.mu.Unlock()
}

// executeState scans the lister for the Execute gate: whether another Execute is
// currently Running, and the most recent terminal Execute completion.
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

// requeueGatedRuns re-enqueues every non-terminal Execute run so any run that was
// gated on the K=1 slot (reason AnotherRunActive) is re-evaluated now that the
// slot is free. Called when an Execute releases the slot; the just-finished run is
// terminal by then and is skipped. DryRun runs are never gated, so they are not
// re-enqueued here.
func (e *Engine) requeueGatedRuns() {
	runs, err := e.lister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "repack-engine: list for gated-run requeue")
		return
	}
	for _, r := range runs {
		if r.Spec.Mode == repackv1alpha1.RepackModeExecute && !state.IsTerminal(r.Status.Phase) {
			e.queue.Add(r.Name)
		}
	}
}
