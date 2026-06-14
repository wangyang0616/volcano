/*
Copyright 2025 The Volcano Authors.

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

// Package perflog provides shared performance instrumentation helpers for scheduler hot paths.
// Enable with scheduler verbosity -v=4.
package perflog

import (
	"time"

	"k8s.io/klog/v2"
)

// Level is the klog verbosity level used for performance logs.
const Level = 4

// Enabled reports whether performance logs are emitted at the configured level.
func Enabled() bool {
	return klog.V(Level).Enabled()
}

// Timer measures elapsed time for a code block.
type Timer struct {
	start time.Time
}

// Start begins a new timer.
func Start() Timer {
	return Timer{start: time.Now()}
}

// Since returns the elapsed time since the timer was started.
func (t Timer) Since() time.Duration {
	return time.Since(t.start)
}

func countHyperNodesByTier(counts map[int]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}
