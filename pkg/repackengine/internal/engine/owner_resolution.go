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
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
)

// resolveMoveOwners returns the direct controller owner for every PodGroup
// affected by plan. Owner display is best-effort status enrichment: a deleted
// PodGroup or a transient read failure must not prevent an otherwise valid
// RepackRun from completing.
func (e *Engine) resolveMoveOwners(ctx context.Context, plan *engineapi.RepackPlan) map[string]*repackv1alpha1.WorkloadRef {
	if plan == nil || e.volcanoClient == nil {
		return nil
	}
	owners := make(map[string]*repackv1alpha1.WorkloadRef)
	for _, podGroupID := range plan.AffectedPodGroups() {
		namespace, name := enginestatus.SplitPodGroupID(string(podGroupID))
		if namespace == "" || name == "" {
			continue
		}
		podGroup, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				klog.V(4).InfoS("repack: cannot resolve PodGroup owner for status move", "podGroup", podGroupID, "err", err)
			}
			continue
		}
		owner := metav1.GetControllerOf(podGroup)
		if owner == nil {
			continue
		}
		owners[string(podGroupID)] = &repackv1alpha1.WorkloadRef{
			APIVersion: owner.APIVersion,
			Kind:       owner.Kind,
			Name:       owner.Name,
		}
	}
	return owners
}
