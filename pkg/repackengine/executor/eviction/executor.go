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

// Package eviction owns Kubernetes Eviction API request construction and error
// normalization. The Engine owns durable journal sequencing around each call.
package eviction

import (
	"context"
	"errors"
	"fmt"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
)

// ErrVictimNotFound preserves the semantic reason of a Kubernetes NotFound
// response so the Engine can distinguish workload-level recreation from a
// rejected eviction.
var ErrVictimNotFound = errors.New("repack victim Pod not found")

type Executor struct {
	client             kubernetes.Interface
	gracePeriodSeconds *int64
}

// New creates an Execute-mode Eviction API executor. DryRun and nil runs do not
// expose an executor because they must never issue eviction requests.
func New(run *repackv1alpha1.RepackRun, client kubernetes.Interface) *Executor {
	if run == nil || run.Spec.Mode != repackv1alpha1.RepackModeExecute {
		return nil
	}
	executor := &Executor{client: client}
	if run.Spec.Eviction != nil {
		executor.gracePeriodSeconds = run.Spec.Eviction.GracePeriodSeconds
	}
	return executor
}

// Evict issues a PDB-respecting Eviction API request. The caller context makes
// in-flight requests responsive to Engine shutdown.
func (e *Executor) Evict(ctx context.Context, move *engineapi.Move) error {
	if e == nil || e.client == nil || move == nil || move.Task == nil || move.Task.Pod == nil {
		return nil
	}
	pod := move.Task.Pod
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		DeleteOptions: &metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &pod.UID},
		},
	}
	if e.gracePeriodSeconds != nil {
		eviction.DeleteOptions.GracePeriodSeconds = e.gracePeriodSeconds
	}
	err := e.client.PolicyV1().Evictions(pod.Namespace).Evict(ctx, eviction)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %s/%s", ErrVictimNotFound, pod.Namespace, pod.Name)
	}
	return err
}
