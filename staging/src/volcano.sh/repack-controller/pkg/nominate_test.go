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

package repackcontroller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
)

func nominatorWith(runs ...*repackv1alpha1.RepackRun) *Nominator {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, r := range runs {
		_ = idx.Add(r)
	}
	return &Nominator{
		repackRunLister: repacklisters.NewRepackRunLister(idx),
		now:             func() time.Time { return time.Unix(1000, 0) },
	}
}

func runWithNoms(name string, noms ...repackv1alpha1.PodNomination) *repackv1alpha1.RepackRun {
	r := &repackv1alpha1.RepackRun{ObjectMeta: metav1.ObjectMeta{Name: name}}
	r.Status.Nominations = noms
	return r
}

func pendingPod(ns, name, pg string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, Labels: labels,
			Annotations: map[string]string{podGroupAnnotationKey: pg},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func TestNeedsNomination(t *testing.T) {
	ok := pendingPod("ns", "p", "g", nil)
	if !needsNomination(ok) {
		t.Error("a pending, unscheduled, unnominated pod needs nomination")
	}
	cases := map[string]func(*corev1.Pod){
		"scheduled":  func(p *corev1.Pod) { p.Spec.NodeName = "n0" },
		"nominated":  func(p *corev1.Pod) { p.Status.NominatedNodeName = "n0" },
		"notPending": func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning },
		"deleting":   func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now },
	}
	for name, mutate := range cases {
		p := pendingPod("ns", "p", "g", nil)
		mutate(p)
		if needsNomination(p) {
			t.Errorf("%s pod must NOT need nomination", name)
		}
	}
}

func TestLabelsMatch(t *testing.T) {
	pod := map[string]string{"a": "1", "b": "2", "c": "3"}
	if !labelsMatch(pod, map[string]string{"a": "1", "b": "2"}) {
		t.Error("superset should match")
	}
	if !labelsMatch(pod, nil) {
		t.Error("empty want matches anything")
	}
	if labelsMatch(pod, map[string]string{"a": "9"}) {
		t.Error("value mismatch should not match")
	}
	if labelsMatch(pod, map[string]string{"z": "1"}) {
		t.Error("missing key should not match")
	}
}

func TestMatchNomination(t *testing.T) {
	future := metav1.NewTime(time.Unix(5000, 0)) // > now (1000)
	past := metav1.NewTime(time.Unix(500, 0))    // < now

	t.Run("victimPodName exact wins", func(t *testing.T) {
		n := nominatorWith(runWithNoms("r1",
			repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "g", VictimPodName: "w-0", NodeName: "n2", ExpirationTime: &future},
		))
		rec, owner := n.matchNomination(pendingPod("ns", "w-0", "g", nil))
		if rec == nil || owner != "r1" || rec.NodeName != "n2" {
			t.Fatalf("exact victim match failed: rec=%+v owner=%q", rec, owner)
		}
	})

	t.Run("identityLabels superset match", func(t *testing.T) {
		n := nominatorWith(runWithNoms("r1",
			repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "g", IdentityLabels: map[string]string{"apps.kubernetes.io/pod-index": "3"}, NodeName: "n5", ExpirationTime: &future},
		))
		pod := pendingPod("ns", "renamed-xyz", "g", map[string]string{"apps.kubernetes.io/pod-index": "3", "other": "x"})
		rec, owner := n.matchNomination(pod)
		if rec == nil || owner != "r1" || rec.NodeName != "n5" {
			t.Fatalf("identity match failed: rec=%+v", rec)
		}
	})

	t.Run("fungible when identityLabels empty", func(t *testing.T) {
		n := nominatorWith(runWithNoms("r1",
			repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "g", NodeName: "n1", ExpirationTime: &future},
		))
		rec, owner := n.matchNomination(pendingPod("ns", "any-pod", "g", nil))
		if rec == nil || owner != "r1" || rec.NodeName != "n1" {
			t.Fatalf("fungible match failed: rec=%+v", rec)
		}
	})

	t.Run("no match: wrong PodGroup / namespace / expired / bound", func(t *testing.T) {
		bound := repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "g", NodeName: "n1", Phase: repackv1alpha1.PodNominationBound, ExpirationTime: &future}
		expired := repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "g", NodeName: "n1", ExpirationTime: &past}
		wrongPG := repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "other", NodeName: "n1", ExpirationTime: &future}
		wrongNS := repackv1alpha1.PodNomination{Namespace: "elsewhere", PodGroupName: "g", NodeName: "n1", ExpirationTime: &future}
		n := nominatorWith(runWithNoms("r1", bound, expired, wrongPG, wrongNS))
		if rec, _ := n.matchNomination(pendingPod("ns", "p", "g", nil)); rec != nil {
			t.Fatalf("should not match any (bound/expired/wrong pg/ns): got %+v", rec)
		}
	})
}

func TestFungibleNominationWaitsForVictimDeletion(t *testing.T) {
	future := metav1.NewTime(time.Unix(5000, 0))
	n := nominatorWith(runWithNoms("r1", repackv1alpha1.PodNomination{
		Namespace: "ns", PodGroupName: "g", VictimPodName: "old", NodeName: "n2", ExpirationTime: &future,
	}))
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	victim := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "old"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if err := pods.Add(victim); err != nil {
		t.Fatal(err)
	}
	n.podLister = corelisters.NewPodLister(pods)
	replacement := pendingPod("ns", "new-random-name", "g", nil)

	if rec, _ := n.matchNomination(replacement); rec != nil {
		t.Fatalf("prepared nomination must not be consumed while victim exists: %+v", rec)
	}
	if err := pods.Delete(victim); err != nil {
		t.Fatal(err)
	}
	if rec, _ := n.matchNomination(replacement); rec == nil || rec.NodeName != "n2" {
		t.Fatalf("nomination should activate after victim deletion: %+v", rec)
	}
}

func TestReconcilePatchesNominatedNodeAndMarksNominationBound(t *testing.T) {
	pod := pendingPod("ns", "replacement", "group", nil)
	run := runWithNoms("run", repackv1alpha1.PodNomination{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "replacement", NodeName: "n2", Phase: repackv1alpha1.PodNominationPending,
	})
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(pod); err != nil {
		t.Fatal(err)
	}
	runs := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := runs.Add(run); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := k8sfake.NewSimpleClientset()
	var nominatedNodePatch string
	kubernetesClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patchAction, ok := action.(k8stesting.PatchAction)
		if !ok || action.GetSubresource() != "status" {
			t.Fatalf("unexpected pod patch action: %#v", action)
		}
		nominatedNodePatch = string(patchAction.GetPatch())
		return true, pod, nil
	})
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    volcanoClient,
		podLister:        corelisters.NewPodLister(pods),
		repackRunLister:  repacklisters.NewRepackRunLister(runs),
		now:              time.Now,
	}

	if err := nominator.reconcile(context.Background(), "ns/replacement"); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	if err := nominator.flushBoundNominations(context.Background(), "run"); err != nil {
		t.Fatalf("flushBoundNominations() error = %v", err)
	}
	if !strings.Contains(nominatedNodePatch, `"nominatedNodeName":"n2"`) {
		t.Errorf("pod status patch = %s, want nominatedNodeName n2", nominatedNodePatch)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), "run", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated RepackRun: %v", err)
	}
	if updated.Status.Nominations[0].Phase != repackv1alpha1.PodNominationBound {
		t.Errorf("nomination phase = %q, want %q", updated.Status.Nominations[0].Phase, repackv1alpha1.PodNominationBound)
	}
}

func TestBoundNominationsAreFlushedOncePerRun(t *testing.T) {
	run := runWithNoms("run",
		repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "group", VictimPodName: "p0", NodeName: "n1", Phase: repackv1alpha1.PodNominationPending},
		repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "group", VictimPodName: "p1", NodeName: "n2", Phase: repackv1alpha1.PodNominationPending},
	)
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	nominator := &Nominator{volcanoClient: volcanoClient}
	nominator.queueBoundNomination(run.Name, &run.Status.Nominations[0])
	nominator.queueBoundNomination(run.Name, &run.Status.Nominations[1])

	if err := nominator.flushBoundNominations(context.Background(), run.Name); err != nil {
		t.Fatalf("flushBoundNominations() error = %v", err)
	}
	statusUpdates := 0
	for _, action := range volcanoClient.Actions() {
		if action.GetVerb() == "update" && action.GetResource().Resource == "repackruns" && action.GetSubresource() == "status" {
			statusUpdates++
		}
	}
	if statusUpdates != 1 {
		t.Fatalf("status updates = %d, want one coalesced update", statusUpdates)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated RepackRun: %v", err)
	}
	for _, nomination := range updated.Status.Nominations {
		if nomination.Phase != repackv1alpha1.PodNominationBound {
			t.Errorf("nomination %q phase = %q, want %q", nomination.VictimPodName, nomination.Phase, repackv1alpha1.PodNominationBound)
		}
	}
}

func TestEnqueuePendingForRunUsesPodGroupIndex(t *testing.T) {
	matchingPod := pendingPod("ns", "matching", "target", nil)
	unrelatedPod := pendingPod("ns", "unrelated", "other", nil)
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{podGroupIndexName: podGroupIndex})
	if err := podIndexer.Add(matchingPod); err != nil {
		t.Fatal(err)
	}
	if err := podIndexer.Add(unrelatedPod); err != nil {
		t.Fatal(err)
	}
	nominator := &Nominator{
		podLister:              corelisters.NewPodLister(podIndexer),
		podIndexer:             podIndexer,
		podGroupIndexAvailable: true,
		workQueue:              workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}
	defer nominator.workQueue.ShutDown()

	run := runWithNoms("run", repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "target", VictimPodName: "gone", NodeName: "n1"})
	nominator.enqueuePendingForRun(run)
	if nominator.workQueue.Len() != 1 {
		t.Fatalf("queued pods = %d, want only the indexed matching PodGroup", nominator.workQueue.Len())
	}
	key, _ := nominator.workQueue.Get()
	defer nominator.workQueue.Done(key)
	if key != "ns/matching" {
		t.Errorf("queued key = %q, want ns/matching", key)
	}
}
