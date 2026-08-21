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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	schedoptions "volcano.sh/volcano/cmd/scheduler/app/options"

	_ "volcano.sh/volcano/pkg/repackengine/actions/repack"
	"volcano.sh/volcano/pkg/repackengine/framework"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/binpack"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/gangdisruption"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/nodeconsolidation"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/repackbudget"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloaddisruption"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloadscope"
	_ "volcano.sh/volcano/pkg/scheduler/actions"
)

type capabilityTestPlugin struct{ name string }

func (p *capabilityTestPlugin) Name() string                    { return p.name }
func (*capabilityTestPlugin) OnSessionOpen(*framework.Session)  {}
func (*capabilityTestPlugin) OnSessionClose(*framework.Session) {}

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
	wantPlugins := []string{"workloadscope", "repackbudget", "nodeconsolidation", "workloaddisruption", "gangdisruption", "binpack"}
	if got := configuredPluginNames(e.config.Plugins); !reflect.DeepEqual(got, wantPlugins) {
		t.Errorf("default Plugins=%v, want %v", got, wantPlugins)
	}
	if e.config.NominationTTL != 10*time.Minute {
		t.Errorf("default NominationTTL = %v, want 10m", e.config.NominationTTL)
	}
}

func TestRepackConfigurationAndExplicitPrecedence(t *testing.T) {
	configured, err := decodeRepackConfiguration([]byte(`
actions: "prepare, repack"
plugins:
  - name: workloaddisruption
    arguments:
      movedPodsWeight: 25
  - name: binpack
`))
	if err != nil {
		t.Fatalf("decode configuration: %v", err)
	}
	if configured.Actions != "prepare, repack" {
		t.Fatalf("actions=%q, want comma-separated source value", configured.Actions)
	}
	if len(configured.Plugins) != 2 || configured.Plugins[0].Name != "workloaddisruption" ||
		configured.Plugins[0].Arguments["movedPodsWeight"] != 25 {
		t.Fatalf("plugins=%+v, want ordered plugins with arguments", configured.Plugins)
	}

	fromFile := &Engine{config: Config{Plugins: defaultPluginOptions()}}
	fromFile.applyRepackConfiguration(configured)
	if got := fromFile.config.Actions; !reflect.DeepEqual(got, []string{"prepare", "repack"}) {
		t.Fatalf("parsed actions=%v, want [prepare repack]", got)
	}
	if got := configuredPluginNames(fromFile.config.Plugins); !reflect.DeepEqual(got, []string{"workloaddisruption", "binpack"}) {
		t.Fatalf("file plugins=%v, want [workloaddisruption binpack]", got)
	}

	explicit := &Engine{
		config:          Config{Actions: []string{"repack"}, Plugins: framework.PluginOptions("workloadscope", "nodeconsolidation")},
		actionsExplicit: true,
		pluginsExplicit: true,
	}
	explicit.applyRepackConfiguration(configured)
	if got := explicit.config.Actions; !reflect.DeepEqual(got, []string{"repack"}) {
		t.Fatalf("explicit actions=%v, want command/programmatic override", got)
	}
	if got := configuredPluginNames(explicit.config.Plugins); !reflect.DeepEqual(got, []string{"workloadscope", "nodeconsolidation"}) {
		t.Fatalf("explicit plugins=%v, want command/programmatic override", got)
	}
}

func TestDecodeRepackConfigurationRejectsActionList(t *testing.T) {
	_, err := decodeRepackConfiguration([]byte(`
actions:
  - repack
`))
	if err == nil {
		t.Fatal("actions list must be rejected; want Scheduler-compatible comma-separated string")
	}
}

func TestDecodeRepackConfigurationRejectsUnknownFields(t *testing.T) {
	tests := map[string]string{
		"top-level": `action: "repack"`,
		"duplicate": `
actions: "repack"
actions: "repack"
`,
		"plugin-option": `
actions: "repack"
plugins:
  - name: gangdisruption
    argments:
      gangBreachesWeight: 0
`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRepackConfiguration([]byte(raw)); err == nil {
				t.Fatal("unknown configuration field must be rejected")
			}
		})
	}
}

func TestValidatePluginOptionsAllowsOptionalPolicies(t *testing.T) {
	if err := validatePluginOptions(nil); err != nil {
		t.Fatalf("empty optional plugin pipeline should be valid: %v", err)
	}
	if err := validatePluginOptions(framework.PluginOptions("workloadscope")); err != nil {
		t.Fatalf("workloadscope should be independently configurable: %v", err)
	}
	if err := validatePluginOptions(framework.PluginOptions("repackbudget")); err != nil {
		t.Fatalf("repackbudget should be independently configurable: %v", err)
	}
	if err := validatePluginOptions(framework.PluginOptions("workloadscope", "workloadscope", "repackbudget")); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate error=%v, want duplicate-plugin rejection", err)
	}
	invalidWeight := []framework.PluginOption{
		{Name: "workloadscope"},
		{Name: "repackbudget"},
		{Name: "workloaddisruption", Arguments: framework.Arguments{"movedPodsWeight": -1}},
	}
	if err := validatePluginOptions(invalidWeight); err == nil || !strings.Contains(err.Error(), "movedPodsWeight") {
		t.Fatalf("invalid weight error=%v, want plugin argument rejection", err)
	}
}

func TestValidatePipelineConfigurationRequiresDomainCapability(t *testing.T) {
	withoutDomain := framework.PluginOptions("workloadscope", "repackbudget", "binpack")
	if err := validatePipelineConfiguration([]string{"repack"}, withoutDomain); err == nil ||
		!strings.Contains(err.Error(), `requires capability "domain"`) {
		t.Fatalf("pipeline validation error=%v, want missing domain capability", err)
	}

	minimal := framework.PluginOptions("nodeconsolidation")
	if err := validatePipelineConfiguration([]string{"repack"}, minimal); err != nil {
		t.Fatalf("nodeconsolidation should satisfy the repack domain capability: %v", err)
	}

	const replacement = "test-replacement-domain"
	framework.RegisterPlugin(replacement, framework.PluginRegistration{
		Factory:  func(framework.Arguments) framework.Plugin { return &capabilityTestPlugin{name: replacement} },
		Provides: []framework.PluginCapability{framework.CapabilityDomain},
	})
	if err := validatePipelineConfiguration([]string{"repack"}, framework.PluginOptions(replacement)); err != nil {
		t.Fatalf("a replacement domain provider must satisfy repack without nodeconsolidation: %v", err)
	}
	emptyProvider := framework.OpenSession(framework.SessionConfig{}, framework.PluginOptions(replacement))
	defer framework.CloseSession(emptyProvider)
	if err := validateSessionCapabilities([]string{"repack"}, framework.PluginOptions(replacement), emptyProvider); err == nil ||
		!strings.Contains(err.Error(), `runtime capability "domain"`) {
		t.Fatalf("runtime capability error=%v, want missing AddDomainFn rejection", err)
	}

	workingProvider := framework.OpenSession(framework.SessionConfig{}, minimal)
	defer framework.CloseSession(workingProvider)
	if err := validateSessionCapabilities([]string{"repack"}, minimal, workingProvider); err != nil {
		t.Fatalf("nodeconsolidation must register the runtime domain capability: %v", err)
	}
}

func TestLoadConfRejectsInvalidPluginWeight(t *testing.T) {
	directory := t.TempDir()
	schedulerConf := filepath.Join(directory, "scheduler.conf")
	repackConf := filepath.Join(directory, "repack.conf")
	if err := os.WriteFile(schedulerConf, []byte(`actions: "enqueue"`), 0o600); err != nil {
		t.Fatalf("write scheduler config: %v", err)
	}
	if err := os.WriteFile(repackConf, []byte(`
actions: "repack"
plugins:
  - name: workloadscope
  - name: repackbudget
  - name: workloaddisruption
    arguments:
      movedPodsWeight: 0.1
`), 0o600); err != nil {
		t.Fatalf("write repack config: %v", err)
	}

	engine := &Engine{config: Config{
		SchedulerConf: schedulerConf,
		RepackConf:    repackConf,
		Plugins:       defaultPluginOptions(),
	}}
	err := engine.loadConf()
	if err == nil || !strings.Contains(err.Error(), "movedPodsWeight") {
		t.Fatalf("loadConf error=%v, want fail-fast invalid weight rejection", err)
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
