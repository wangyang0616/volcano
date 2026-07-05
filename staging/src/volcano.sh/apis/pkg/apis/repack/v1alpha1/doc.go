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

// Package v1alpha1 contains API Schema definitions for the repack v1alpha1
// API group — the Repack (runtime defragmentation) CRDs. P0 ships only
// RepackRun (a one-shot, user-immutable job); RepackPolicy is P1 (see design
// docs/design/repack-policy-design.md §3.3).
//
// +kubebuilder:object:generate=true
// +groupName=repack.volcano.sh
// +k8s:deepcopy-gen=package
// +k8s:openapi-gen=true

package v1alpha1
