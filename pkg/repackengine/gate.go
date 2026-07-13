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
func (e *Engine) executeGateState(currentRunName string) (executeActive bool, latestExecuteFinishTime time.Time) {
	e.engineStateMutex.Lock()
	inMemoryExecuteActive := e.activeExecuteRunName != "" && e.activeExecuteRunName != currentRunName
	latestExecuteFinishTime = e.lastExecuteFinishTime
	e.engineStateMutex.Unlock()

	persistedExecuteActive, persistedExecuteFinishTime := e.persistedExecuteState(currentRunName)
	executeActive = inMemoryExecuteActive || persistedExecuteActive
	if persistedExecuteFinishTime.After(latestExecuteFinishTime) {
		latestExecuteFinishTime = persistedExecuteFinishTime
	}
	return executeActive, latestExecuteFinishTime
}

// markExecuteActive claims the in-memory K=1 slot for an Execute run.
func (e *Engine) markExecuteActive(name string) {
	e.engineStateMutex.Lock()
	e.activeExecuteRunName = name
	e.engineStateMutex.Unlock()
}

// markExecuteDone releases the slot and stamps the cooldown anchor.
func (e *Engine) markExecuteDone(name string) {
	e.engineStateMutex.Lock()
	if e.activeExecuteRunName == name {
		e.activeExecuteRunName = ""
	}
	e.lastExecuteFinishTime = e.now()
	e.engineStateMutex.Unlock()
}

// persistedExecuteState scans the lister for the Execute gate: whether another Execute is
// currently Running, and the most recent terminal Execute completion.
func (e *Engine) persistedExecuteState(currentRunName string) (executeActive bool, latestExecuteFinishTime time.Time) {
	runs, err := e.repackRunLister.List(labels.Everything())
	if err != nil {
		return true, time.Time{} // conservative: assume busy
	}
	for _, run := range runs {
		if run.Spec.Mode != repackv1alpha1.RepackModeExecute {
			continue
		}
		if run.Name != currentRunName && run.Status.Phase == repackv1alpha1.RepackRunning {
			executeActive = true
		}
		if state.IsTerminal(run.Status.Phase) && run.Status.CompletionTime != nil {
			if completionTime := run.Status.CompletionTime.Time; completionTime.After(latestExecuteFinishTime) {
				latestExecuteFinishTime = completionTime
			}
		}
	}
	return executeActive, latestExecuteFinishTime
}

// requeueGatedRuns re-enqueues every non-terminal Execute run so any run that was
// gated on the K=1 slot (reason AnotherRunActive) is re-evaluated now that the
// slot is free. Called when an Execute releases the slot; the just-finished run is
// terminal by then and is skipped. DryRun runs are never gated, so they are not
// re-enqueued here.
func (e *Engine) requeueGatedRuns() {
	runs, err := e.repackRunLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "repack: list for gated-run requeue")
		return
	}
	woken := 0
	for _, run := range runs {
		if run.Spec.Mode == repackv1alpha1.RepackModeExecute && !state.IsTerminal(run.Status.Phase) {
			e.workQueue.Add(run.Name)
			woken++
		}
	}
	if woken > 0 {
		klog.V(4).InfoS("requeued gated Execute runs after slot release", "count", woken)
	}
}
