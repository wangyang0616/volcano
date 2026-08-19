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
	"testing"
	"time"

	"k8s.io/client-go/rest"

	schedoptions "volcano.sh/volcano/cmd/scheduler/app/options"
)

// fakeConfig is a non-nil rest.Config that never connects — client construction
// is lazy, so NewEngine can be built offline (no cluster required).
func fakeConfig() *rest.Config { return &rest.Config{Host: "https://127.0.0.1:6443"} }

// NewEngine applies its defaults so an operator can start the engine with an
// empty Config and still get a working driver.
func TestNewEngineAppliesDefaults(t *testing.T) {
	e, err := NewEngine(fakeConfig(), Config{})
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	if e == nil {
		t.Fatal("NewEngine() returned nil engine")
	}
	if len(e.config.Plugins) == 0 {
		t.Error("default Plugins should be non-empty")
	}
	if e.config.NominationTTL != 10*time.Minute {
		t.Errorf("default NominationTTL = %v, want 10m", e.config.NominationTTL)
	}
}

// The repack-engine binary never runs the scheduler's flag setup, so the global
// options.ServerOpts is nil. NewEngine must initialize it before building the
// scheduler cache, or reused scheduler code nil-derefs at startup.
func TestNewEngineInitializesServerOptsWhenNil(t *testing.T) {
	orig := schedoptions.ServerOpts
	t.Cleanup(func() { schedoptions.ServerOpts = orig })
	schedoptions.ServerOpts = nil

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewEngine panicked with a nil ServerOpts: %v", r)
		}
	}()

	if _, err := NewEngine(fakeConfig(), Config{}); err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	if schedoptions.ServerOpts == nil {
		t.Fatal("NewEngine did not initialize the global scheduler ServerOpts")
	}
}
