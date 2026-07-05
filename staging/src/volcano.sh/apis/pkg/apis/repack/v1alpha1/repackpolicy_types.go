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

// RepackPolicy is P1 (see the design proposal). Its schema is finalized here so
// the API is stable, but it is intentionally NOT added to register.go's
// SchemeBuilder and carries no +genclient marker yet: no controller/clientset is
// wired in P0. controller-gen still emits DeepCopy for these types.
//
// Model (finalized): RepackPolicy does ONE thing — generate RepackRuns on a
// trigger, embedding a RepackRun template (CronJob -> Job). It does NOT clamp
// user-authored RepackRuns (cluster defaults / hard guardrails are governance;
// handled separately via CEL ValidatingAdmissionPolicy or a future CRD).

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------- spec ----------

// RepackPolicySpec declares when to trigger and what RepackRun to generate.
type RepackPolicySpec struct {
	// Trigger is when to emit a RepackRun (three sources; any match fires).
	Trigger RepackTrigger `json:"trigger"`

	// RunTemplate is the RepackRun to create (reuses RepackRunSpec).
	// Whether the derived Run is DryRun or Execute is decided solely by
	// runTemplate.spec.mode (set mode: DryRun to only auto-produce reports);
	// Execute serialization/cooldown is enforced by the engine (K=1 + cooldown).
	RunTemplate RepackRunTemplateSpec `json:"runTemplate"`

	// Suspend pauses triggering (does not affect already-generated Runs).
	// +optional
	Suspend *bool `json:"suspend,omitempty"`

	// SuccessfulRunsHistoryLimit / FailedRunsHistoryLimit cap how many finished
	// derived Runs are retained (flat, mirroring CronJob; default 3 each).
	// +optional
	// +kubebuilder:validation:Minimum=0
	SuccessfulRunsHistoryLimit *int32 `json:"successfulRunsHistoryLimit,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	FailedRunsHistoryLimit *int32 `json:"failedRunsHistoryLimit,omitempty"`
}

// RepackRunTemplateSpec is the template for the derived RepackRun (mirrors
// CronJob's JobTemplateSpec).
type RepackRunTemplateSpec struct {
	// +optional
	ObjectMeta metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec       RepackRunSpec     `json:"spec"`
}

// RepackTrigger is three trigger sources; whichever is set is enabled and any
// match fires. The evaluation period for the reactive sources
// (onPendingBlocked/onFragmentation) is a controller-level setting (startup
// flag / controller config, one global value, akin to the Execute cooldown),
// not a per-Policy field.
//
// +kubebuilder:validation:XValidation:rule="has(self.cronSchedule) || has(self.onPendingBlocked) || has(self.onFragmentation)",message="trigger must set at least one of cronSchedule/onPendingBlocked/onFragmentation"
type RepackTrigger struct {
	// CronSchedule is a cron expression: fires on schedule.
	// +optional
	CronSchedule string `json:"cronSchedule,omitempty"`

	// OnPendingBlocked fires when gangs cannot schedule due to fragmentation
	// (reactive). "Due to fragmentation" means a repack plan exists that would
	// make them schedulable (repack can actually help) — not that the cluster is
	// simply full — to avoid useless triggers.
	// +optional
	OnPendingBlocked *PendingBlockedTrigger `json:"onPendingBlocked,omitempty"`

	// OnFragmentation fires when the fragmentation rate exceeds a threshold
	// (reactive).
	// +optional
	OnFragmentation *FragmentationTrigger `json:"onFragmentation,omitempty"`
}

// PendingBlockedTrigger gates the "pending blocked by fragmentation" source.
type PendingBlockedTrigger struct {
	// MinPendingPodGroups: at least this many PodGroups (gangs) blocked by
	// fragmentation before firing. Default 1.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinPendingPodGroups *int32 `json:"minPendingPodGroups,omitempty"`
	// MinBlockedDuration: and blocked continuously for at least this long
	// (debounce).
	// +optional
	MinBlockedDuration *metav1.Duration `json:"minBlockedDuration,omitempty"`
}

// FragmentationTrigger gates the "fragmentation rate above threshold" source.
type FragmentationTrigger struct {
	// FragAbovePercent fires when the fragmentation rate is above this percentage
	// (0-100). FragRate is this design's fragmentation metric; see the design
	// proposal's fragmentation-metric section.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	FragAbovePercent int32 `json:"fragAbovePercent"`
	// MinPendingPodGroups is an optional additional gate: also require at least
	// this many PodGroups pending before firing.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinPendingPodGroups *int32 `json:"minPendingPodGroups,omitempty"`
}

// ---------- status ----------

// RepackPolicyStatus reports triggering bookkeeping (mirrors CronJob's active[]
// / lastScheduleTime shape; LastTriggerTime is used instead of lastScheduleTime
// because triggers are multi-source, not schedule-only).
type RepackPolicyStatus struct {
	// LastEvaluationTime is when the controller last evaluated the trigger.
	// +optional
	LastEvaluationTime *metav1.Time `json:"lastEvaluationTime,omitempty"`
	// LastTriggerTime is when a RepackRun was last generated.
	// +optional
	LastTriggerTime *metav1.Time `json:"lastTriggerTime,omitempty"`
	// Active are the derived RepackRuns currently in progress.
	// +optional
	// +listType=atomic
	Active []v1.ObjectReference `json:"active,omitempty"`
	// Conditions is the authoritative status detail.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ---------- root objects ----------

// RepackPolicy generates RepackRuns on a trigger, embedding a RepackRun
// template (CronJob -> Job). Cluster-scoped.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=repackpolicies,scope=Cluster,shortName=rpp;repackpolicy
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="SUSPEND",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="LAST-TRIGGER",type=date,JSONPath=`.status.lastTriggerTime`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type RepackPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RepackPolicySpec `json:"spec"`
	// +optional
	Status RepackPolicyStatus `json:"status,omitempty"`
}

// RepackPolicyList is a list of RepackPolicy.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type RepackPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RepackPolicy `json:"items"`
}
