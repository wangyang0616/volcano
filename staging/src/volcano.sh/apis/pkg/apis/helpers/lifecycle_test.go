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

package helpers

import (
	"context"
	"errors"
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

func TestJobResourcesRejectPreviousLifecycle(t *testing.T) {
	old := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same", UID: "old"}}
	current := old.DeepCopy()
	current.UID = "new"
	cm := &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same-svc", UID: "cm", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(current, JobKind)}}, Data: map[string]string{"hosts": "current"}}
	secret := &v1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same-ssh", UID: "secret", OwnerReferences: cm.OwnerReferences}, Data: map[string][]byte{"config": []byte("current")}}
	client := fake.NewSimpleClientset(cm, secret)
	var conflict *JobResourceConflictError
	if err := CreateOrUpdateConfigMap(old, client, map[string]string{"hosts": "stale"}, cm.Name); !errors.As(err, &conflict) {
		t.Fatalf("expected owner conflict: %v", err)
	}
	if err := CreateOrUpdateSecret(old, client, map[string][]byte{"config": []byte("stale")}, secret.Name); !errors.As(err, &conflict) {
		t.Fatalf("expected owner conflict: %v", err)
	}
	if err := DeleteConfigmap(old, client, cm.Name); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSecret(old, client, secret.Name); err != nil {
		t.Fatal(err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" {
			t.Fatalf("old lifecycle mutated current resource: %#v", a)
		}
	}
	if err := CreateOrUpdateConfigMap(current, client, map[string]string{"hosts": "updated"}, cm.Name); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSecret(current, client, secret.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets(secret.Namespace).Get(context.TODO(), secret.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("current lifecycle could not delete its Secret")
	}
}

func TestDeleteJobResourceUsesObservedResourceUID(t *testing.T) {
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same", UID: "job"}}
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "same", UID: "old-service", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, JobKind)}}}
	client := fake.NewSimpleClientset(svc)
	client.PrependReactor("delete", "services", func(a clienttesting.Action) (bool, runtime.Object, error) {
		options := a.(clienttesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != svc.UID {
			t.Fatal("missing resource UID precondition")
		}
		replacement := svc.DeepCopy()
		replacement.UID = "new-service"
		if err := client.Tracker().Update(v1.SchemeGroupVersion.WithResource("services"), replacement, replacement.Namespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "services"}, svc.Name, fmt.Errorf("UID changed"))
	})
	if err := DeleteJobResource(job, client.CoreV1().Services(job.Namespace), svc.Name); err != nil {
		t.Fatal(err)
	}
	current, err := client.CoreV1().Services(job.Namespace).Get(context.TODO(), svc.Name, metav1.GetOptions{})
	if err != nil || current.UID != "new-service" {
		t.Fatal("replacement resource deleted")
	}
}
