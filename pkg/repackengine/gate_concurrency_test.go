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
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/tools/cache"

	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
)

// The K=1 in-memory gate state (execActive/lastExecFinish, guarded by mu) is
// designed to be safe even if Workers > 1. Hammer markExecuteActive/Done and
// executeGateState from many goroutines; run with -race to catch data races.
func TestExecuteGateState_ConcurrentAccess(t *testing.T) {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	e := &Engine{
		lister: repacklisters.NewRepackRunLister(idx),
		now:    time.Now,
	}

	const goroutines, iters = 16, 500
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			name := fmt.Sprintf("run-%d", id)
			for j := 0; j < iters; j++ {
				e.markExecuteActive(name)
				active, last := e.executeGateState(name)
				_ = active
				_ = last
				e.markExecuteDone(name)
			}
		}(i)
	}
	wg.Wait()

	// Every goroutine finished with markExecuteDone, which stamps the cooldown
	// anchor, so lastExecFinish must be set (and reads must not have raced).
	if _, last := e.executeGateState("x"); last.IsZero() {
		t.Error("lastExecFinish should be stamped after Execute completions")
	}
}
