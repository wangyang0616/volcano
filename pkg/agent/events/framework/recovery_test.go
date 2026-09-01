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
	"strings"
	"testing"

	"volcano.sh/volcano/pkg/agent/config/api"
)

type recoveryTestHandle struct {
	handle     func(interface{}) error
	refreshCfg func(*api.ColocationConfig) error
}

func (recoveryTestHandle) HandleName() string { return "panic-handler" }
func (h recoveryTestHandle) Handle(event interface{}) error {
	if h.handle != nil {
		return h.handle(event)
	}
	return nil
}
func (recoveryTestHandle) IsActive() bool { return true }
func (h recoveryTestHandle) RefreshCfg(cfg *api.ColocationConfig) error {
	if h.refreshCfg != nil {
		return h.refreshCfg(cfg)
	}
	return nil
}

func TestCallHandlerWithRecoveryCapturesStack(t *testing.T) {
	err := callHandlerWithRecovery(recoveryTestHandle{}, "testing recovery", func() error {
		panic("diagnostic panic")
	})
	if err == nil {
		t.Fatal("callHandlerWithRecovery() expected error, got nil")
	}
	for _, expected := range []string{"panic-handler", "testing recovery", "diagnostic panic", "goroutine", "recovery_test.go"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("recovered error %q does not contain %q", err, expected)
		}
	}
}

func TestCallHandlerWithRecoveryPreservesError(t *testing.T) {
	want := errors.New("handler error")
	got := callHandlerWithRecovery(recoveryTestHandle{}, "testing error", func() error {
		return want
	})
	if !errors.Is(got, want) {
		t.Fatalf("callHandlerWithRecovery() error = %v, want %v", got, want)
	}
}

func TestEventQueueFactorySyncConfigRecoversHandlerPanic(t *testing.T) {
	handler := recoveryTestHandle{refreshCfg: func(*api.ColocationConfig) error {
		panic("refresh panic")
	}}
	factory := &EventQueueFactory{Queues: map[string]*EventQueue{
		"test": {Handlers: []Handle{handler}},
	}}

	err := factory.SyncConfig(&api.ColocationConfig{})
	if err == nil || !strings.Contains(err.Error(), "refresh panic") || !strings.Contains(err.Error(), "goroutine") {
		t.Fatalf("SyncConfig() error = %v, want recovered panic with stack", err)
	}
}

func TestEventQueueRecoversHandlerPanic(t *testing.T) {
	handler := recoveryTestHandle{handle: func(interface{}) error {
		panic("event panic")
	}}
	queue := NewEventQueue("panic-test")
	defer queue.Queue.ShutDown()
	queue.AddHandler(handler)
	queue.Queue.Add("event")

	if !queue.processNextWorkItem(context.Background()) {
		t.Fatal("processNextWorkItem() unexpectedly shut down")
	}
	if retries := queue.Queue.NumRequeues("event"); retries != 1 {
		t.Fatalf("event retries = %d, want 1", retries)
	}
}
