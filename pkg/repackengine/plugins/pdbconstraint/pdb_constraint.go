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

// Package pdbconstraint applies deterministic PodDisruptionBudget constraints
// during Repack planning. It excludes Pods protected by a fresh, zero-
// disruption PDB while leaving transient budget exhaustion to the Eviction API
// and the execution retry protocol.
package pdbconstraint

import (
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the Repack configuration name of this plugin.
const Name = "pdbconstraint"

type pdbConstraintPlugin struct{}

type compiledConstraint struct {
	namespace                  string
	name                       string
	selector                   labels.Selector
	unhealthyPodEvictionPolicy policyv1.UnhealthyPodEvictionPolicyType
	currentHealthy             int32
	desiredHealthy             int32
	disruptedPods              map[string]metav1.Time
}

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory: func(framework.Arguments) framework.Plugin { return &pdbConstraintPlugin{} },
		Validator: func(arguments framework.Arguments) error {
			return arguments.ValidateKeys()
		},
	})
}

func (*pdbConstraintPlugin) Name() string { return Name }

func (*pdbConstraintPlugin) OnSessionOpen(ssn *framework.Session) {
	if ssn == nil || ssn.Snapshot() == nil {
		return
	}
	reader, ok := ssn.Snapshot().(framework.PodDisruptionBudgetReader)
	if !ok {
		klog.Warning("repack: pdbconstraint is enabled but the planning snapshot does not expose PodDisruptionBudgets; failing open")
		return
	}
	pdbs, err := reader.ListPodDisruptionBudgets()
	if err != nil {
		klog.Warningf("repack: cannot read PodDisruptionBudgets for pdbconstraint; failing open: %v", err)
		return
	}

	constraintsByNamespace := compileConstraints(pdbs)
	blockedTasks, blockedPodGroups, targetTaskCount := collectBlockedTasks(ssn, constraintsByNamespace)
	klog.V(4).InfoS("repack: PDB constraints prepared",
		"run", runName(ssn),
		"pdbCount", len(pdbs),
		"zeroDisruptionPDBCount", constraintCount(constraintsByNamespace),
		"targetTaskCount", targetTaskCount,
		"blockedTaskCount", len(blockedTasks),
		"blockedPodGroupCount", len(blockedPodGroups))
	if len(blockedTasks) == 0 {
		return
	}

	// blockedTasks is immutable after session open, so planning lookups are O(1)
	// and require no synchronization or repeated informer reads.
	ssn.AddMovableFn(func(task *schedapi.TaskInfo) bool {
		key := taskIdentity(task)
		if key == "" {
			return true
		}
		_, blocked := blockedTasks[key]
		return !blocked
	})
}

func (*pdbConstraintPlugin) OnSessionClose(*framework.Session) {}

func compileConstraints(pdbs []*policyv1.PodDisruptionBudget) map[string][]compiledConstraint {
	constraintsByNamespace := make(map[string][]compiledConstraint)
	for _, pdb := range pdbs {
		if !isZeroDisruptionPDB(pdb) || pdb.Spec.Selector == nil {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			klog.ErrorS(err, "repack: ignoring zero-disruption PDB with an invalid selector",
				"pdb", pdb.Namespace+"/"+pdb.Name)
			continue
		}
		constraint := compiledConstraint{
			namespace:      pdb.Namespace,
			name:           pdb.Name,
			selector:       selector,
			currentHealthy: pdb.Status.CurrentHealthy,
			desiredHealthy: pdb.Status.DesiredHealthy,
			disruptedPods:  pdb.Status.DisruptedPods,
		}
		if pdb.Spec.UnhealthyPodEvictionPolicy != nil {
			constraint.unhealthyPodEvictionPolicy = *pdb.Spec.UnhealthyPodEvictionPolicy
		}
		constraintsByNamespace[pdb.Namespace] = append(constraintsByNamespace[pdb.Namespace], constraint)
	}
	return constraintsByNamespace
}

func isZeroDisruptionPDB(pdb *policyv1.PodDisruptionBudget) bool {
	if pdb == nil || pdb.Status.ObservedGeneration != pdb.Generation || pdb.Status.ExpectedPods <= 0 {
		return false
	}
	condition := apiMeta.FindStatusCondition(pdb.Status.Conditions, policyv1.DisruptionAllowedCondition)
	if condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == policyv1.SyncFailedReason {
		return false
	}
	return pdb.Status.DesiredHealthy >= pdb.Status.ExpectedPods
}

func collectBlockedTasks(
	ssn *framework.Session,
	constraintsByNamespace map[string][]compiledConstraint,
) (map[string]struct{}, map[schedapi.JobID]struct{}, int) {
	blockedTasks := make(map[string]struct{})
	blockedPodGroups := make(map[schedapi.JobID]struct{})
	targetTaskCount := 0
	for _, node := range ssn.Nodes() {
		if node == nil {
			continue
		}
		for _, task := range node.Tasks {
			if task == nil || task.Pod == nil || engineapi.Scalar(task.InitResreq, ssn.Resource()) <= 0 {
				continue
			}
			targetTaskCount++
			constraint, blocked := blockingConstraint(task.Pod, constraintsByNamespace[task.Pod.Namespace])
			if !blocked {
				continue
			}
			key := taskIdentity(task)
			if key == "" {
				continue
			}
			blockedTasks[key] = struct{}{}
			if task.Job != "" {
				blockedPodGroups[task.Job] = struct{}{}
			}
			klog.V(5).InfoS("repack: task excluded by zero-disruption PDB constraint",
				"pod", task.Pod.Namespace+"/"+task.Pod.Name,
				"podGroup", task.Job,
				"pdb", constraint.namespace+"/"+constraint.name,
				"reason", "pdb_zero_disruption")
		}
	}
	return blockedTasks, blockedPodGroups, targetTaskCount
}

func blockingConstraint(pod *v1.Pod, constraints []compiledConstraint) (compiledConstraint, bool) {
	if pod == nil || canIgnorePDB(pod) {
		return compiledConstraint{}, false
	}
	for _, constraint := range constraints {
		if !constraint.selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		if _, alreadyDisrupted := constraint.disruptedPods[pod.Name]; alreadyDisrupted {
			continue
		}
		if !podutil.IsPodReady(pod) && constraint.allowsUnhealthyPodEviction() {
			continue
		}
		return constraint, true
	}
	return compiledConstraint{}, false
}

// canIgnorePDB mirrors the pod states for which the Kubernetes Eviction API
// bypasses PDB evaluation. Bound Pending and terminating Pods can still be
// present in the scheduler node snapshot, so this check cannot be left to the
// execution phase without conservatively rejecting a feasible drain.
func canIgnorePDB(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodSucceeded ||
		pod.Status.Phase == v1.PodFailed ||
		pod.Status.Phase == v1.PodPending ||
		pod.DeletionTimestamp != nil
}

// allowsUnhealthyPodEviction mirrors the Eviction API's per-PDB treatment of
// an unready Pod. AlwaysAllow bypasses the budget unconditionally; the default
// IfHealthyBudget policy permits it only while the guarded workload already
// has the desired number of healthy replicas.
func (c compiledConstraint) allowsUnhealthyPodEviction() bool {
	if c.unhealthyPodEvictionPolicy == policyv1.AlwaysAllow {
		return true
	}
	return c.currentHealthy >= c.desiredHealthy && c.desiredHealthy > 0
}

func taskIdentity(task *schedapi.TaskInfo) string {
	if task == nil {
		return ""
	}
	if task.UID != "" {
		return "uid/" + string(task.UID)
	}
	if task.Pod != nil && task.Pod.Namespace != "" && task.Pod.Name != "" {
		return "pod/" + task.Pod.Namespace + "/" + task.Pod.Name
	}
	return ""
}

func constraintCount(constraintsByNamespace map[string][]compiledConstraint) int {
	count := 0
	for _, constraints := range constraintsByNamespace {
		count += len(constraints)
	}
	return count
}

func runName(ssn *framework.Session) string {
	if ssn == nil || ssn.Run() == nil {
		return ""
	}
	return ssn.Run().Name
}
