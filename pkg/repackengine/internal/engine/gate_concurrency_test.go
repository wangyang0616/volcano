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

package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/tools/cache"

	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
)

// K=1 admission must be an atomic check-and-claim: concurrent workers may not
// both observe a free slot and start Execute.
func TestTryAcquireExecute_ConcurrentK1(t *testing.T) {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	e := &Engine{
		repackRunLister: repacklisters.NewRepackRunLister(idx),
		now:             func() time.Time { return now },
	}

	const goroutines = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	admitted := make(chan string, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			name := fmt.Sprintf("run-%d", id)
			if gate, _, _ := e.tryAcquireExecute(name, now); gate.Admit {
				admitted <- name
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(admitted)

	var winner string
	for name := range admitted {
		if winner != "" {
			t.Fatalf("multiple Execute runs admitted: %q and %q", winner, name)
		}
		winner = name
	}
	if winner == "" {
		t.Fatal("no Execute run admitted")
	}
	e.markExecuteDone(winner)
	if gate, _, last := e.tryAcquireExecute("next", now); !gate.Admit || last.IsZero() {
		t.Errorf("slot should be reusable after release, gate=%+v last=%v", gate, last)
	}
}

func TestMarkExecuteDoneIsOwnerCheckedAndIdempotent(t *testing.T) {
	first := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	now := first
	e := &Engine{activeExecuteRunName: "owner", now: func() time.Time { return now }}

	if e.markExecuteDone("other") {
		t.Fatal("a different Run must not release the active Execute slot")
	}
	if e.activeExecuteRunName != "owner" || !e.lastExecuteFinishTime.IsZero() {
		t.Fatalf("foreign release changed state: active=%q finish=%v", e.activeExecuteRunName, e.lastExecuteFinishTime)
	}
	if !e.markExecuteDone("owner") {
		t.Fatal("the current owner must release the Execute slot")
	}
	now = second
	if e.markExecuteDone("owner") {
		t.Fatal("an idempotent retry must not report another release")
	}
	if e.lastExecuteFinishTime != first {
		t.Fatalf("idempotent release moved cooldown anchor to %v, want %v", e.lastExecuteFinishTime, first)
	}
}
