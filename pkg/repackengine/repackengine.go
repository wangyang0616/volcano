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

// Package repackengine exposes the stable construction and runtime entry points
// for volcano-repack-engine. Runtime orchestration is intentionally hidden in
// internal/engine; planning capabilities live in actions, plugins and planner.
package repackengine

import (
	"context"

	"k8s.io/client-go/rest"

	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	"volcano.sh/volcano/pkg/repackengine/framework"
	internalengine "volcano.sh/volcano/pkg/repackengine/internal/engine"
)

// Config is the public runtime configuration accepted by NewEngine.
type Config = engineconf.Config

// Engine is the stable public facade for the internal Repack runtime.
type Engine struct {
	impl *internalengine.Engine
}

// NewEngine constructs a Repack Engine using the supplied Kubernetes client
// configuration and runtime settings.
func NewEngine(config *rest.Config, engineConfig Config) (*Engine, error) {
	impl, err := internalengine.NewEngine(config, engineConfig)
	if err != nil {
		return nil, err
	}
	return &Engine{impl: impl}, nil
}

// Run starts the Repack runtime and blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	if e == nil || e.impl == nil {
		return
	}
	e.impl.Run(ctx)
}

// PluginOptions converts command-line plugin names into framework options.
func PluginOptions(names []string) []framework.PluginOption {
	return engineconf.PluginOptions(names)
}
