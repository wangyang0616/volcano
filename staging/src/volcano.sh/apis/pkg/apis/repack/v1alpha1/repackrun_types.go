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

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RepackMode selects whether a RepackRun only simulates or actually evicts.
// +kubebuilder:validation:Enum=DryRun;Execute
type RepackMode string

const (
	// RepackModeDryRun simulates and reports a plan without evicting anything.
	RepackModeDryRun RepackMode = "DryRun"
	// RepackModeExecute evicts/relocates per the recomputed plan.
	RepackModeExecute RepackMode = "Execute"
)

// BundlePolicy controls the unit of relocation for a gang.
// +kubebuilder:validation:Enum=SurplusPodsOnly;EntireJobPermitted
type BundlePolicy string

const (
	// BundleSurplusPodsOnly moves only pods above MinAvailable (never breaches gang).
	BundleSurplusPodsOnly BundlePolicy = "SurplusPodsOnly"
	// BundleEntireJobPermitted allows relocating a whole gang.
	BundleEntireJobPermitted BundlePolicy = "EntireJobPermitted"
)

// RepackPhase is the coarse lifecycle phase. conditions are authoritative;
// phase is a derived projection for kubectl wait / list views.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Cancelled
type RepackPhase string

const (
	RepackPending   RepackPhase = "Pending"
	RepackRunning   RepackPhase = "Running"
	RepackSucceeded RepackPhase = "Succeeded"
	RepackFailed    RepackPhase = "Failed"
	RepackCancelled RepackPhase = "Cancelled"
)

// ---------- spec ----------

// RepackRunSpec is the user-facing, self-contained spec of one repack job.
// P0 is self-contained (hand-written in full); relief and disruptionPolicy are
// P1 — fields are reserved here so the schema is stable, but the engine
// honors engine defaults in P0.
//
// Admission is enforced entirely at the apiserver via CEL/markers (no controller
// admission step): mode is an enum, goals is capped at one entry, the spec is
// immutable, and the rule below requires Execute to name a non-empty scope on at
// least one axis (a blanket whole-cluster Execute is rejected).
// +kubebuilder:validation:XValidation:rule="self.mode != 'Execute' || (has(self.scope) && ((has(self.scope.podGroups) && has(self.scope.podGroups.include) && (has(self.scope.podGroups.include.selector) || (has(self.scope.podGroups.include.names) && size(self.scope.podGroups.include.names) > 0))) || (has(self.scope.nodes) && has(self.scope.nodes.include) && (has(self.scope.nodes.include.selector) || (has(self.scope.nodes.include.names) && size(self.scope.nodes.include.names) > 0)))))",message="Execute requires spec.scope with a non-empty include on podGroups or nodes"
type RepackRunSpec struct {
	// Mode selects DryRun (simulate + report) or Execute (act).
	// +kubebuilder:validation:Required
	Mode RepackMode `json:"mode"`

	// Scope bounds which PodGroups may move and which nodes participate.
	// +optional
	Scope *RepackScope `json:"scope,omitempty"`

	// Relief (P1): pending PodGroups this run aims to make schedulable.
	// +optional
	Relief *RepackRelief `json:"relief,omitempty"`

	// Goals is the per-resource fragmentation target. P0/P1: exactly one entry
	// (a run defragments a single accelerator resource); multi-resource is P2+.
	// +optional
	// +kubebuilder:validation:MaxItems=1
	Goals []RepackGoal `json:"goals,omitempty"`

	// DisruptionPolicy (P1): how/whether running jobs may be disturbed.
	// +optional
	DisruptionPolicy *DisruptionPolicy `json:"disruptionPolicy,omitempty"`

	// MaxPerRun caps the blast radius of a single run.
	// +optional
	MaxPerRun *MaxPerRun `json:"maxPerRun,omitempty"`

	// TTLSecondsAfterFinished: auto-DELETE this Run that long after it finishes
	// (like Job). Unset = not auto-deleted (P0; Policy default is P1).
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished,omitempty"`
}

// RepackScope bounds the run on two independent axes: which PodGroups may move
// and which nodes participate. Each axis has symmetric include/exclude
// (label selector and/or explicit name list); exclude wins.
//
// Everything is a PodGroup: selection is expressed uniformly against PodGroups
// (the engine's action/cost unit is the gang), so both `selector` (PG labels)
// and `names` (PG "namespace/name") address the SAME object. This works for
// every workload type — Volcano-native (vcjob), K8s-native (Deployment /
// StatefulSet / ...), and user-custom CRDs — because Volcano's pg-controller
// inherits pod template labels onto the auto-created PodGroup (system/controller
// labels such as pod-template-hash are filtered out), so a PG label selector
// addresses all of them. Note `names` is only practical for deterministically
// named PodGroups (e.g. vcjob); auto-created PGs have UID-derived names, so those
// are selected via `selector`.
type RepackScope struct {
	// +optional
	PodGroups *RepackSelectorTerm `json:"podGroups,omitempty"`
	// +optional
	Nodes *RepackSelectorTerm `json:"nodes,omitempty"`
}

// RepackSelectorTerm is an include/exclude pair (exclude wins).
type RepackSelectorTerm struct {
	// +optional
	Include *RepackSelector `json:"include,omitempty"`
	// +optional
	Exclude *RepackSelector `json:"exclude,omitempty"`
}

// RepackSelector matches by label selector and/or explicit names (union).
type RepackSelector struct {
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// Names are "namespace/name" for PodGroups, or node names for nodes.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	Names []string `json:"names,omitempty"`
}

// RepackRelief (P1) names pending PodGroups to unblock (beneficiaries; they
// themselves are not moved).
type RepackRelief struct {
	// PodGroupRefs are "namespace/name" of pending PodGroups to relieve.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	PodGroupRefs []string `json:"podGroupRefs,omitempty"`
	// MinRelieved: at least this many must become schedulable to be worthwhile.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinRelieved *int32 `json:"minRelieved,omitempty"`
}

// RepackGoal is one resource's fragmentation target.
type RepackGoal struct {
	// Resource is the accelerator to defragment (e.g. nvidia.com/gpu). Only
	// fully-qualified extended resources (a name containing "/") are supported;
	// core compute resources such as cpu, memory, ephemeral-storage and pods are
	// rejected, because repack only consolidates scalar accelerator capacity.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self.contains('/')",message="goals.resource must be a fully-qualified accelerator/extended resource (e.g. nvidia.com/gpu); core resources like cpu, memory and ephemeral-storage are not supported"
	Resource v1.ResourceName `json:"resource"`
	// MinFragImprovementPercent is the required minimum drop in this resource's
	// fragmentation, in percentage points (0-100). 0 = any benefit counts.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MinFragImprovementPercent int32 `json:"minFragImprovementPercent,omitempty"`
}

// DisruptionPolicy (P1) tunes how running jobs may be disturbed. Lives on the
// Run (not in plugin config): plugin config only selects which scoring plugins
// are enabled. P0 ignores these and uses engine defaults.
type DisruptionPolicy struct {
	// +optional
	BundlePolicy BundlePolicy `json:"bundlePolicy,omitempty"`
	// MinRunDuration: jobs running less than this are not moved.
	// +optional
	MinRunDuration *metav1.Duration `json:"minRunDuration,omitempty"`
	// MaxDisruptionScore is the disruption-cost red line.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxDisruptionScore *int32 `json:"maxDisruptionScore,omitempty"`
	// RespectPDB enables PodDisruptionBudget compatibility.
	// +optional
	RespectPDB *bool `json:"respectPDB,omitempty"`
	// Lambda is the benefit-vs-friction trade-off as an integer weight (default 1).
	// +optional
	// +kubebuilder:validation:Minimum=0
	Lambda int32 `json:"lambda,omitempty"`
	// Weights are per-disruption-term integer weights (relative; keys must match
	// scoring plugins enabled in config).
	// +optional
	Weights map[string]int32 `json:"weights,omitempty"`
	// HardFloors are optional hard guardrails (frozen anchors / per-job caps).
	// +optional
	HardFloors *DisruptionHardFloors `json:"hardFloors,omitempty"`
}

// DisruptionHardFloors are optional hard guardrails.
type DisruptionHardFloors struct {
	// FreezePriorityAbove: gangs with priority >= this never move.
	// +optional
	FreezePriorityAbove *int32 `json:"freezePriorityAbove,omitempty"`
	// MaxMovesPerJob caps relocations of any single PodGroup.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxMovesPerJob *int32 `json:"maxMovesPerJob,omitempty"`
}

// MaxPerRun caps a single run's blast radius (distinct from K8s resource limits).
type MaxPerRun struct {
	// PodGroups caps distinct PodGroups moved (cross-resource count).
	// +optional
	// +kubebuilder:validation:Minimum=0
	PodGroups *int32 `json:"podGroups,omitempty"`
	// Resources caps accelerator cards moved per resource.
	// +optional
	Resources v1.ResourceList `json:"resources,omitempty"`
}

// ---------- status ----------

// RepackRunStatus reports lifecycle and business output. conditions are
// authoritative; phase is derived. "Worth repacking?" is folded into the
// terminal Complete condition's reason (RepackRecommended / Executed /
// NoFragmentation / BelowGoalThreshold), not a summary field.
type RepackRunStatus struct {
	// Phase is a derived projection of conditions.
	// +optional
	Phase RepackPhase `json:"phase,omitempty"`

	// Conditions are the authoritative facts (Job-style: Queued/
	// Progressing/Complete/Failed/Cancelled). Admission is CEL-only, so there is
	// no Admitted condition. The Complete condition's reason also encodes whether
	// repacking was worthwhile.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Message is a one-line human summary written at terminal state.
	// +optional
	Message string `json:"message,omitempty"`

	// StartTime is when the run first entered Running.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the run first reached a terminal phase (TTL anchor).
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Plan is the migration plan, populated in BOTH modes with the SAME shape:
	// DryRun = predicted plan; Execute = executed plan. moves is a pure plan;
	// Execute's realized landing is reported via nominations[].phase + summary.
	// +optional
	Plan *RepackPlan `json:"plan,omitempty"`

	// Nominations are the durable landing-steering intents produced by Execute:
	// one entry per relocated pod, consumed by the nomination reconciler per the
	// landing-identity contract (victimPodName exact match -> identityLabels ->
	// fungible). See the design proposal §5.2.2.
	// +optional
	// +kubebuilder:validation:MaxItems=4096
	Nominations []PodNomination `json:"nominations,omitempty"`
}

// PodNomination steers one relocated pod's replacement onto a target node
// (Execute-only). The reconciler patches pod.status.nominatedNodeName, claiming
// the replacement per the landing-identity contract (proposal §5.2.2):
// victimPodName exact match -> identityLabels (label match) -> fungible (any
// pending pod in the PodGroup).
type PodNomination struct {
	// Namespace of the target pod (PodGroup shares this namespace).
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
	// PodGroupName the pod belongs to.
	// +optional
	PodGroupName string `json:"podGroupName,omitempty"`
	// VictimPodName is the evicted pod's name: audit + exact fast-path when the
	// controller recreates the replacement with the same name.
	// +optional
	VictimPodName string `json:"victimPodName,omitempty"`
	// IdentityLabels are the labels that identify the replacement pod across
	// reconstruction (key = the identity label the contract used, e.g.
	// repack.volcano.sh/pod-identity, or a native-kind label such as
	// apps.kubernetes.io/pod-index; value = its value). The reconciler claims a
	// pending pod whose labels are a superset. Empty = fungible (any pod in the
	// PodGroup). Self-describing: which label + value is visible in status.
	// +optional
	IdentityLabels map[string]string `json:"identityLabels,omitempty"`
	// NodeName is the target node to nominate the replacement onto.
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`
	// ExpirationTime bounds re-assertion; after it the nomination is Expired.
	// +optional
	ExpirationTime *metav1.Time `json:"expirationTime,omitempty"`
	// Phase: Pending (not yet matched) / Bound (patched onto a replacement) /
	// Expired (elapsed without a match).
	// +optional
	// +kubebuilder:validation:Enum=Pending;Bound;Expired
	Phase string `json:"phase,omitempty"`
}

// RepackPlan is the terminal output, populated in BOTH modes with the same
// shape: DryRun = predicted plan; Execute = executed plan. Three progressive
// layers (summary + moves + freedNodes), capped by maxItems. Schema evolves with
// the CRD apiVersion (no internal formatVersion).
type RepackPlan struct {
	// Summary is the flat, at-a-glance layer (UI lists / alerts / printers read this).
	// +optional
	Summary *RepackSummary `json:"summary,omitempty"`
	// Moves is the per-PodGroup relocation detail; fromNode/toNode live per-pod
	// inside pods[]. moves is a pure plan (identical in DryRun and Execute).
	// +optional
	// +kubebuilder:validation:MaxItems=4096
	Moves []RepackMove `json:"moves,omitempty"`
	// FreedNodes are the names of nodes the plan empties.
	// +optional
	// +kubebuilder:validation:MaxItems=2048
	FreedNodes []string `json:"freedNodes,omitempty"`
	// Relief reports which pending PodGroups would be unblocked (P1).
	// +optional
	// +kubebuilder:validation:MaxItems=256
	Relief []RelievedPodGroup `json:"relief,omitempty"`
}

// RepackSummary is the flat second layer of the plan. Single-resource per run
// (goals maxItems=1): the fragmentation figures are for that one accelerator
// resource (goals[0].resource) — no per-resource breakdown. Multi-resource is
// P2+ (would add a per-resource layer then).
type RepackSummary struct {
	// Fragmentation before/after for the run's resource, in percentage points
	// (0-100). Improvement = before - after (derive client-side).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	FragBeforePercent int32 `json:"fragBeforePercent,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	FragAfterPercent int32 `json:"fragAfterPercent,omitempty"`
	// FreedNodeCount is the number of nodes freed (headline; printer column).
	// +optional
	FreedNodeCount int32 `json:"freedNodeCount,omitempty"`
	// MovedCardCount is the total accelerator cards relocated (the run's resource).
	// +optional
	MovedCardCount int64 `json:"movedCardCount,omitempty"`
	// +optional
	ResolvedScope *ResolvedScope `json:"resolvedScope,omitempty"`
}

// ResolvedScope reports the effective scope after selector resolution.
type ResolvedScope struct {
	// +optional
	PodGroupCount int32 `json:"podGroupCount,omitempty"`
	// +optional
	NodeCount int32 `json:"nodeCount,omitempty"`
}

// RepackMove is one PodGroup's relocation. fromNode/toNode are per-pod (a gang's
// pods may be spread across nodes / move to multiple targets), so they live in
// pods[]; the PodGroup level keeps identity + aggregate.
type RepackMove struct {
	// Namespace of this PodGroup (owner and pods share it).
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
	// PodGroupName is the precise scheduling-dimension identity (matches scope).
	// +kubebuilder:validation:Required
	PodGroupName string `json:"podGroupName"`
	// Owner is the user-facing workload owning this PodGroup (PodGroup is an
	// internal object). Taken from the PG controller ownerReference (not walked
	// up); empty for ownerless bare pods.
	// +optional
	Owner *WorkloadRef `json:"owner,omitempty"`
	// Cards is this PodGroup's total accelerator cards moved (= sum of pods[].cards).
	// +optional
	Cards int64 `json:"cards,omitempty"`
	// Pods is the per-pod relocation detail (only relocated pods appear).
	// +optional
	// +kubebuilder:validation:MaxItems=4096
	Pods []PodMove `json:"pods,omitempty"`
}

// PodMove is one pod's planned relocation (pure plan; DryRun/Execute identical).
type PodMove struct {
	// Name is the pod name (a plan-time snapshot for random-named controllers).
	// +optional
	Name string `json:"name,omitempty"`
	// FromNode is where the pod currently runs.
	// +optional
	FromNode string `json:"fromNode,omitempty"`
	// ToNode is the pod's PLANNED target node (soft nomination, no reservation).
	// +optional
	ToNode string `json:"toNode,omitempty"`
	// Cards is the accelerator cards (GPU/NPU) this pod occupies.
	// +optional
	Cards int64 `json:"cards,omitempty"`
}

// WorkloadRef is the user-facing workload owning a PodGroup (Deployment/
// StatefulSet/Job/vcjob…). Passed through from the PG controller ownerReference
// (not walked up: a Deployment's pod shows ReplicaSet). Namespace matches the move.
type WorkloadRef struct {
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
	// +optional
	Kind string `json:"kind,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
}

// RelievedPodGroup reports a pending gang that would become schedulable (P1).
type RelievedPodGroup struct {
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:Required
	PodGroupName string `json:"podGroupName"`
	// +optional
	Relieved bool `json:"relieved,omitempty"`
}

// ---------- root objects ----------

// RepackRun is a one-shot runtime-defragmentation job. It is cluster-scoped and
// user-immutable (CREATE/READ/DELETE only): spec is frozen after admission via a
// CEL transition rule — to change anything, delete and create a new Run.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=repackruns,scope=Cluster,shortName=rpr;repackrun
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="MODE",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="PHASE",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="FREED",type=integer,JSONPath=`.status.plan.summary.freedNodeCount`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type RepackRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is immutable once set (one-shot job; recompute by creating a new Run).
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="RepackRun.spec is immutable; create a new RepackRun to change it"
	Spec RepackRunSpec `json:"spec"`
	// +optional
	Status RepackRunStatus `json:"status,omitempty"`
}

// RepackRunList is a list of RepackRun.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type RepackRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RepackRun `json:"items"`
}
