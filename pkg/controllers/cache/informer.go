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

package cache

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	toolscache "k8s.io/client-go/tools/cache"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	batchlisters "volcano.sh/apis/pkg/client/listers/batch/v1alpha1"
)

const JobOwnerIndex = "volcanoJobOwner"

func ownerKey(namespace, name string, uid types.UID) string {
	return namespace + "/" + name + "/" + string(uid)
}

// JobOwnerIndexFunc must only read object fields: it runs with the informer
// store lock held, while the job cache reads that store under its own lock.
func JobOwnerIndexFunc(obj interface{}) ([]string, error) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return nil, fmt.Errorf("expected Pod, got %T", obj)
	}
	owner := volcanoJobOwner(pod)
	if owner == nil {
		return nil, nil
	}
	return []string{ownerKey(pod.Namespace, owner.Name, owner.UID)}, nil
}

func volcanoJobOwner(pod *v1.Pod) *metav1.OwnerReference {
	owner := metav1.GetControllerOfNoCopy(pod)
	if owner == nil {
		return nil
	}
	gv, err := schema.ParseGroupVersion(owner.APIVersion)
	if err != nil || gv.Group != batch.SchemeGroupVersion.Group || owner.Kind != "Job" || owner.UID == "" {
		return nil
	}
	return owner
}

type informerReader struct {
	jobs    batchlisters.JobLister
	pods    corelisters.PodLister
	indexer toolscache.Indexer
}

func NewInformerReader(jobs batchlisters.JobLister, pods toolscache.Indexer) SnapshotReader {
	return &informerReader{jobs: jobs, pods: corelisters.NewPodLister(pods), indexer: pods}
}

func (r *informerReader) GetJob(namespace, name string) (*batch.Job, error) {
	return r.jobs.Jobs(namespace).Get(name)
}

func (r *informerReader) GetPod(namespace, name string) (*v1.Pod, error) {
	return r.pods.Pods(namespace).Get(name)
}

func (r *informerReader) ListPods(namespace, name string, uid types.UID) ([]*v1.Pod, error) {
	objects, err := r.indexer.ByIndex(JobOwnerIndex, ownerKey(namespace, name, uid))
	if err != nil {
		return nil, err
	}
	pods := make([]*v1.Pod, 0, len(objects))
	for _, obj := range objects {
		pods = append(pods, obj.(*v1.Pod))
	}
	return pods, nil
}
