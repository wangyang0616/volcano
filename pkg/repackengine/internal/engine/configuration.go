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
	"os"

	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/scheduler"
)

func configuredPluginNames(options []engineframework.PluginOption) []string {
	names := make([]string, 0, len(options))
	for _, option := range options {
		names = append(names, option.Name)
	}
	return names
}

// loadConf loads the scheduler filter stack and the independent Repack action
// and plugin pipeline. Explicit flags keep precedence over file values.
func (e *Engine) loadConf() error {
	if e.config.SchedulerConf == "" {
		return fmt.Errorf("scheduler-conf is required")
	}
	raw, err := os.ReadFile(e.config.SchedulerConf)
	if err != nil {
		return err
	}
	_, tiers, configurations, _, err := scheduler.UnmarshalSchedulerConf(string(raw))
	if err != nil {
		return err
	}
	e.tiers, e.configurations = tiers, configurations
	if e.config.RepackConf != "" {
		repackRaw, err := os.ReadFile(e.config.RepackConf)
		if err != nil {
			return fmt.Errorf("read repack-conf: %w", err)
		}
		repackConfig, err := engineconf.Decode(repackRaw)
		if err != nil {
			return fmt.Errorf("decode repack-conf: %w", err)
		}
		engineconf.ApplyFile(&e.config, repackConfig, e.actionsExplicit, e.pluginsExplicit)
	}
	return engineconf.ValidatePipeline(e.config.Actions, e.config.Plugins)
}
