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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
)

func nominatorWith(runs ...*repackv1alpha1.RepackRun) *Nominator {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, r := range runs {
		_ = idx.Add(r)
	}
	return &Nominator{
		repackLister: repacklisters.NewRepackRunLister(idx),
		now:          func() time.Time { return time.Unix(1000, 0) },
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
			Annotations: map[string]string{annPodGroup: pg},
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
		bound := repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "g", NodeName: "n1", Phase: nomBound, ExpirationTime: &future}
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
