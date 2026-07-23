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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
	"volcano.sh/repack-controller/pkg/placement"
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

func TestNominatorSerializesRepackRunStatusWrites(t *testing.T) {
	if nominationWorkerCount != 1 {
		t.Fatalf("nominationWorkerCount = %d, want 1 to serialize the active Execute RepackRun status", nominationWorkerCount)
	}
}

func matchNomination(n *Nominator, pod *corev1.Pod) (*repackv1alpha1.PodNomination, string) {
	runs, _ := n.repackRunLister.List(labels.Everything())
	return n.matchNominationInRuns(pod, runs)
}

func runWithNoms(name string, noms ...repackv1alpha1.PodNomination) *repackv1alpha1.RepackRun {
	r := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status:     repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackRunning},
	}
	r.Status.Nominations = noms
	return r
}

func pendingPod(ns, name, pg string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, Labels: labels,
			Annotations: map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: pg},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func expectRecorderEvent(t *testing.T, recorder *record.FakeRecorder, reason string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, reason) {
				return
			}
		case <-deadline:
			t.Fatalf("did not observe event reason %q", reason)
		}
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
		rec, owner := matchNomination(n, pendingPod("ns", "w-0", "g", nil))
		if rec == nil || owner != "r1" || rec.NodeName != "n2" {
			t.Fatalf("exact victim match failed: rec=%+v owner=%q", rec, owner)
		}
	})

	t.Run("victimPodName exact remains scoped to PodGroup", func(t *testing.T) {
		n := nominatorWith(runWithNoms("r1",
			repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "source", VictimPodName: "w-0", NodeName: "n2", ExpirationTime: &future},
		))
		rec, _ := matchNomination(n, pendingPod("ns", "w-0", "concurrent-scale-out", nil))
		if rec != nil {
			t.Fatalf("exact Pod name must not bypass PodGroup identity: %+v", rec)
		}
	})

	t.Run("identityLabels superset match", func(t *testing.T) {
		n := nominatorWith(runWithNoms("r1",
			repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "g", IdentityLabels: map[string]string{"apps.kubernetes.io/pod-index": "3"}, NodeName: "n5", ExpirationTime: &future},
		))
		pod := pendingPod("ns", "renamed-xyz", "g", map[string]string{"apps.kubernetes.io/pod-index": "3", "other": "x"})
		rec, owner := matchNomination(n, pod)
		if rec == nil || owner != "r1" || rec.NodeName != "n5" {
			t.Fatalf("identity match failed: rec=%+v", rec)
		}
	})

	t.Run("fungible when identityLabels empty", func(t *testing.T) {
		n := nominatorWith(runWithNoms("r1",
			repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "g", NodeName: "n1", ExpirationTime: &future},
		))
		rec, owner := matchNomination(n, pendingPod("ns", "any-pod", "g", nil))
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
		if rec, _ := matchNomination(n, pendingPod("ns", "p", "g", nil)); rec != nil {
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

	if rec, _ := matchNomination(n, replacement); rec != nil {
		t.Fatalf("prepared nomination must not be consumed while victim exists: %+v", rec)
	}
	if err := pods.Delete(victim); err != nil {
		t.Fatal(err)
	}
	if rec, _ := matchNomination(n, replacement); rec == nil || rec.NodeName != "n2" {
		t.Fatalf("nomination should activate after victim deletion: %+v", rec)
	}
}

func TestMatchNominationUsesRecordedReplacementPodGroupForEveryIdentityStrategy(t *testing.T) {
	future := metav1.NewTime(time.Unix(5000, 0))
	tests := []struct {
		name       string
		nomination repackv1alpha1.PodNomination
		pod        *corev1.Pod
	}{
		{
			name: "exact Pod name",
			nomination: repackv1alpha1.PodNomination{
				Namespace: "ns", PodGroupName: "old", ReplacementPodGroupName: "new",
				VictimPodName: "worker-0", NodeName: "node-a", ExpirationTime: &future,
			},
			pod: pendingPod("ns", "worker-0", "new", nil),
		},
		{
			name: "identity label",
			nomination: repackv1alpha1.PodNomination{
				Namespace: "ns", PodGroupName: "old", ReplacementPodGroupName: "new",
				VictimPodName: "old-worker", NodeName: "node-a", ExpirationTime: &future,
				IdentityLabels: map[string]string{"apps.kubernetes.io/pod-index": "1"},
			},
			pod: pendingPod("ns", "new-worker", "new", map[string]string{"apps.kubernetes.io/pod-index": "1"}),
		},
		{
			name: "fungible Pod",
			nomination: repackv1alpha1.PodNomination{
				Namespace: "ns", PodGroupName: "old", ReplacementPodGroupName: "new",
				VictimPodName: "old-random", NodeName: "node-a", ExpirationTime: &future,
			},
			pod: pendingPod("ns", "new-random", "new", nil),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nominator := nominatorWith(runWithNoms("run", test.nomination))
			nomination, owner := matchNomination(nominator, test.pod)
			if nomination == nil || owner != "run" {
				t.Fatalf("recorded replacement PodGroup did not match %s: nomination=%+v owner=%q",
					test.name, nomination, owner)
			}
		})
	}
}

func TestRemovePlacementGateRemovesOwnerMarker(t *testing.T) {
	pod := pendingPod("ns", "scale-out", "group", nil)
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/uid"
	client := k8sfake.NewSimpleClientset(pod)
	recorder := record.NewFakeRecorder(10)
	n := &Nominator{kubernetesClient: client, recorder: recorder}

	if err := n.clearPlacementGate(context.Background(), pod); err != nil {
		t.Fatalf("remove placement gate: %v", err)
	}
	for _, action := range client.Actions() {
		patch, ok := action.(k8stesting.PatchAction)
		if !ok || action.GetResource().Resource != "pods" {
			continue
		}
		body := string(patch.GetPatch())
		if !strings.Contains(body, "/spec/schedulingGates/0") || !strings.Contains(body, "placement-gate-owner") {
			t.Fatalf("patch must remove gate and owner marker, got %s", body)
		}
		expectRecorderEvent(t, recorder, eventReasonPlacementReleased)
		return
	}
	t.Fatal("expected Pod patch")
}

func TestPendingPlacementForPodGroup(t *testing.T) {
	pod := pendingPod("ns", "scale-out", "group", nil)
	future := metav1.NewTime(time.Unix(5000, 0))
	run := runWithNoms("run", repackv1alpha1.PodNomination{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", Phase: repackv1alpha1.PodPlacementPrepared, ExpirationTime: &future,
	})
	run.Spec.Mode = repackv1alpha1.RepackModeExecute
	run.Status.Phase = repackv1alpha1.RepackRunning
	if !pendingPlacementForPodGroup(run, pod, time.Unix(1000, 0)) {
		t.Fatal("unconsumed placement in the same PodGroup must hold an ambiguous Pod")
	}
	run.Status.Nominations[0].Phase = repackv1alpha1.PodPlacementPlaced
	if pendingPlacementForPodGroup(run, pod, time.Unix(1000, 0)) {
		t.Fatal("placed nomination must not hold an unrelated Pod")
	}
}

func TestPendingPlacementForLeasedWorkloadProtectsUnmappedPodGroup(t *testing.T) {
	controller := true
	run := runWithNoms("run", repackv1alpha1.PodNomination{
		Namespace: "ns", PodGroupName: "old", VictimPodName: "old-0",
		Phase: repackv1alpha1.PodPlacementPrepared,
	})
	run.Status.Plan = &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
		Namespace: "ns", PodGroupName: "old",
		Owner: &repackv1alpha1.WorkloadRef{APIVersion: "serving.example/v1", Kind: "Serving", Name: "model"},
	}}}
	podGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "candidate",
		Annotations: map[string]string{
			repackv1alpha1.PlacementLeaseAnnotation: placement.OwnerValue(run.Name, run.UID),
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "serving.example/v1", Kind: "Serving", Name: "model", Controller: &controller,
		}},
	}}
	nominator := &Nominator{volcanoClient: vcfake.NewSimpleClientset(podGroup)}
	pending, err := nominator.pendingPlacementForLeasedWorkload(
		context.Background(), run, pendingPod("ns", "candidate-0", "candidate", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("unmapped leased PodGroup must remain gated while its workload has pending placements")
	}

	run.Status.Nominations[0].Phase = repackv1alpha1.PodPlacementPlaced
	pending, err = nominator.pendingPlacementForLeasedWorkload(
		context.Background(), run, pendingPod("ns", "candidate-0", "candidate", nil))
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("workload candidate must be released after all placements finish")
	}
}

func TestReconcilePatchesNominatedNodeAndRecordsPlacementNominated(t *testing.T) {
	pod := pendingPod("ns", "replacement", "group", nil)
	pod.UID = "replacement-uid"
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
	run := runWithNoms("run", repackv1alpha1.PodNomination{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "replacement", NodeName: "n2",
		SelectedNodeName: "n2", ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID, Phase: repackv1alpha1.PodPlacementGated,
	})
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(pod); err != nil {
		t.Fatal(err)
	}
	runs := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := runs.Add(run); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
	var nominatedNodePatch string
	kubernetesClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patchAction, ok := action.(k8stesting.PatchAction)
		if !ok || action.GetSubresource() != "status" {
			return false, nil, nil
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
		recorder:         record.NewFakeRecorder(10),
		now:              time.Now,
	}

	if err := nominator.reconcile(context.Background(), "ns/replacement"); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	if !strings.Contains(nominatedNodePatch, `"nominatedNodeName":"n2"`) {
		t.Errorf("pod status patch = %s, want nominatedNodeName n2", nominatedNodePatch)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), "run", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated RepackRun: %v", err)
	}
	if updated.Status.Nominations[0].Phase != repackv1alpha1.PodPlacementNominated {
		t.Errorf("nomination phase = %q, want %q", updated.Status.Nominations[0].Phase, repackv1alpha1.PodPlacementNominated)
	}
	if updated.Status.Nominations[0].ReplacementPodName != "replacement" {
		t.Errorf("replacement pod = %q, want replacement", updated.Status.Nominations[0].ReplacementPodName)
	}
	opened, err := kubernetesClient.CoreV1().Pods("ns").Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if hasPlacementGate(opened) {
		t.Fatal("selected replacement gate was not opened")
	}
	if opened.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != "run/run-uid" {
		t.Fatal("gate owner must remain until actual binding is observed")
	}
	expectRecorderEvent(t, nominator.recorder.(*record.FakeRecorder), eventReasonPlacementNominated)
}

func TestReconcileResumesInterruptedNominationAndOpensGate(t *testing.T) {
	for _, initialPhase := range []repackv1alpha1.PodNominationPhase{
		repackv1alpha1.PodPlacementGated,
		repackv1alpha1.PodPlacementNominated,
	} {
		t.Run(string(initialPhase), func(t *testing.T) {
			pod := pendingPod("ns", "replacement", "group", nil)
			pod.UID = "replacement-uid"
			pod.Status.NominatedNodeName = "n2"
			pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
			pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
			run := runWithNoms("run", repackv1alpha1.PodNomination{
				Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", NodeName: "n2",
				SelectedNodeName: "n2", ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID, Phase: initialPhase,
			})
			pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if err := pods.Add(pod); err != nil {
				t.Fatal(err)
			}
			kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
			volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
			nominator := &Nominator{
				kubernetesClient: kubernetesClient,
				volcanoClient:    volcanoClient,
				podLister:        corelisters.NewPodLister(pods),
				now:              time.Now,
			}

			if err := nominator.reconcile(context.Background(), "ns/replacement"); err != nil {
				t.Fatalf("reconcile() error = %v", err)
			}
			updatedRun, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if phase := updatedRun.Status.Nominations[0].Phase; phase != repackv1alpha1.PodPlacementNominated {
				t.Fatalf("nomination phase = %q, want Nominated", phase)
			}
			updatedPod, err := kubernetesClient.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if hasPlacementGate(updatedPod) {
				t.Fatal("an interrupted Nominated transition must eventually open the placement gate")
			}
			if updatedPod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] == "" {
				t.Fatal("gate owner must remain until binding is observed")
			}
		})
	}
}

func TestReconcileDoesNotMutatePodBeforeNominationStatusIsDurable(t *testing.T) {
	pod := pendingPod("ns", "replacement", "group", nil)
	pod.UID = "replacement-uid"
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
	run := runWithNoms("run", repackv1alpha1.PodNomination{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", NodeName: "n2",
		SelectedNodeName: "n2", ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID,
		Phase: repackv1alpha1.PodPlacementGated,
	})
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(pod); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	volcanoClient.PrependReactor("update", "repackruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: repackv1alpha1.GroupName, Resource: "repackruns"},
			run.Name, errors.New("forced status conflict"))
	})
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    volcanoClient,
		podLister:        corelisters.NewPodLister(pods),
		now:              time.Now,
	}

	if err := nominator.reconcile(context.Background(), "ns/replacement"); !apierrors.IsConflict(err) {
		t.Fatalf("reconcile() error = %v, want conflict", err)
	}
	for _, action := range kubernetesClient.Actions() {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "pods" {
			t.Fatalf("Pod was patched before RepackRun status became durable: %#v", action)
		}
	}
}

func TestReconcileRetriesEveryPodMutationAfterNominationIsDurable(t *testing.T) {
	for _, failurePoint := range []string{"pod-status", "placement-gate"} {
		t.Run(failurePoint, func(t *testing.T) {
			pod := pendingPod("ns", "replacement", "group", nil)
			pod.UID = "replacement-uid"
			pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
			pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
			run := runWithNoms("run", repackv1alpha1.PodNomination{
				Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", NodeName: "n2",
				SelectedNodeName: "n2", ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID,
				Phase: repackv1alpha1.PodPlacementNominated,
			})
			pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if err := pods.Add(pod); err != nil {
				t.Fatal(err)
			}
			kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
			failureInjected := false
			kubernetesClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if failureInjected {
					return false, nil, nil
				}
				isStatusPatch := action.GetSubresource() == "status"
				if (failurePoint == "pod-status" && isStatusPatch) || (failurePoint == "placement-gate" && !isStatusPatch) {
					failureInjected = true
					return true, nil, errors.New("forced Pod patch failure")
				}
				return false, nil, nil
			})
			nominator := &Nominator{
				kubernetesClient: kubernetesClient,
				volcanoClient:    vcfake.NewSimpleClientset(run.DeepCopy()),
				podLister:        corelisters.NewPodLister(pods),
				now:              time.Now,
			}

			if err := nominator.reconcile(context.Background(), "ns/replacement"); err == nil {
				t.Fatalf("first reconcile() unexpectedly succeeded at failure point %s", failurePoint)
			}
			partiallyUpdatedPod, err := kubernetesClient.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !hasPlacementGate(partiallyUpdatedPod) {
				t.Fatal("placement gate must remain until every preceding mutation succeeds")
			}

			if err := nominator.reconcile(context.Background(), "ns/replacement"); err != nil {
				t.Fatalf("retry reconcile() error = %v", err)
			}
			updatedPod, err := kubernetesClient.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if updatedPod.Status.NominatedNodeName != "n2" {
				t.Fatalf("nominatedNodeName = %q, want n2", updatedPod.Status.NominatedNodeName)
			}
			if hasPlacementGate(updatedPod) {
				t.Fatal("retry did not open the placement gate")
			}
			if updatedPod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] == "" {
				t.Fatal("gate owner must remain until binding is observed")
			}
		})
	}
}

func TestReconcileExpiredNominationClearsGateFromAlreadyNominatedPod(t *testing.T) {
	pod := pendingPod("ns", "replacement", "group", nil)
	pod.UID = "replacement-uid"
	pod.Status.NominatedNodeName = "n2"
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
	run := runWithNoms("run", repackv1alpha1.PodNomination{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", NodeName: "n2",
		SelectedNodeName: "n2", ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID,
		Phase: repackv1alpha1.PodPlacementExpired,
	})
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(pod); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    vcfake.NewSimpleClientset(run.DeepCopy()),
		podLister:        corelisters.NewPodLister(pods),
		now:              time.Now,
	}

	if err := nominator.reconcile(context.Background(), "ns/replacement"); err != nil {
		t.Fatal(err)
	}
	updatedPod, err := kubernetesClient.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if hasPlacementGate(updatedPod) || updatedPod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != "" {
		t.Fatalf("expired placement metadata was not cleared: gates=%v annotations=%v", updatedPod.Spec.SchedulingGates, updatedPod.Annotations)
	}
}

func TestReconcileGatedReplacementReportsIdentityWithoutOpeningGate(t *testing.T) {
	pod := pendingPod("ns", "replacement", "group", nil)
	pod.UID = "replacement-uid"
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
	run := runWithNoms("run", repackv1alpha1.PodNomination{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "replacement", NodeName: "n2", Phase: repackv1alpha1.PodPlacementPrepared,
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
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    volcanoClient,
		podLister:        corelisters.NewPodLister(pods),
		repackRunLister:  repacklisters.NewRepackRunLister(runs),
		recorder:         record.NewFakeRecorder(10),
		now:              time.Now,
	}

	if err := nominator.reconcile(context.Background(), "ns/replacement"); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), "run", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated RepackRun: %v", err)
	}
	nomination := updated.Status.Nominations[0]
	if nomination.Phase != repackv1alpha1.PodPlacementGated {
		t.Errorf("phase = %q, want %q", nomination.Phase, repackv1alpha1.PodPlacementGated)
	}
	if nomination.ReplacementPodName != pod.Name || nomination.ReplacementPodUID != pod.UID {
		t.Errorf("replacement identity = %q/%q, want %q/%q", nomination.ReplacementPodName, nomination.ReplacementPodUID, pod.Name, pod.UID)
	}
	for _, action := range kubernetesClient.Actions() {
		if action.GetVerb() == "patch" {
			t.Errorf("gated replacement must not be nominated or opened before engine selection: %#v", action)
		}
	}
	expectRecorderEvent(t, nominator.recorder.(*record.FakeRecorder), eventReasonReplacementGated)
}

func TestObservePlacementRecordsSuccessAndDrift(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		actualNode     string
		expectedPhase  repackv1alpha1.PodNominationPhase
		expectedReason string
	}{
		{name: "selected node", actualNode: "n2", expectedPhase: repackv1alpha1.PodPlacementPlaced, expectedReason: eventReasonPlacementSucceeded},
		{name: "different node", actualNode: "n3", expectedPhase: repackv1alpha1.PodPlacementDegraded, expectedReason: eventReasonPlacementDrifted},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pod := pendingPod("ns", "replacement", "group", nil)
			pod.UID = "replacement-uid"
			pod.Spec.NodeName = testCase.actualNode
			pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
			run := runWithNoms("run", repackv1alpha1.PodNomination{
				Namespace: "ns", PodGroupName: "group", VictimPodName: "victim",
				NodeName: "n2", SelectedNodeName: "n2",
				ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID,
				Phase: repackv1alpha1.PodPlacementNominated,
			})
			recorder := record.NewFakeRecorder(10)
			volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
			nominator := &Nominator{
				volcanoClient: volcanoClient,
				recorder:      recorder,
			}

			if err := nominator.observePlacement(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := updated.Status.Nominations[0]; got.Phase != testCase.expectedPhase || got.ActualNodeName != testCase.actualNode {
				t.Fatalf("observed nomination = %+v, want phase %s on %s", got, testCase.expectedPhase, testCase.actualNode)
			}
			expectRecorderEvent(t, recorder, testCase.expectedReason)
		})
	}
}

func TestReconcileDerivesAutomaticPodGroupBeforeAnnotation(t *testing.T) {
	controller := true
	pod := pendingPod("ns", "deployment-new", "", nil)
	pod.UID = "replacement-uid"
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "deployment-rs",
		UID: types.UID("replicaset-uid"), Controller: &controller,
	}}
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
	run := runWithNoms("run", repackv1alpha1.PodNomination{
		Namespace: "ns", PodGroupName: "podgroup-replicaset-uid", VictimPodName: "deployment-old",
		NodeName: "n2", Phase: repackv1alpha1.PodPlacementPrepared,
	})
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(pod); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    volcanoClient,
		podLister:        corelisters.NewPodLister(pods),
		now:              time.Now,
	}

	if err := nominator.reconcile(context.Background(), "ns/deployment-new"); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), "run", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if nomination := updated.Status.Nominations[0]; nomination.Phase != repackv1alpha1.PodPlacementGated ||
		nomination.ReplacementPodName != pod.Name || nomination.ReplacementPodUID != pod.UID {
		t.Fatalf("derived automatic PodGroup did not claim replacement: %+v", nomination)
	}
}

func TestMarkPlacementGatedClaimsFreshUnassignedNomination(t *testing.T) {
	run := runWithNoms("run",
		repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "group", VictimPodName: "old-0", NodeName: "n1", Phase: repackv1alpha1.PodPlacementPrepared},
		repackv1alpha1.PodNomination{Namespace: "ns", PodGroupName: "group", VictimPodName: "old-1", NodeName: "n2", Phase: repackv1alpha1.PodPlacementPrepared},
	)
	first := pendingPod("ns", "new-a", "group", nil)
	first.UID = "new-a-uid"
	second := pendingPod("ns", "new-b", "group", nil)
	second.UID = "new-b-uid"
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	nominator := &Nominator{volcanoClient: volcanoClient, now: time.Now}

	if err := nominator.markPlacementGated(context.Background(), run.Name, first); err != nil {
		t.Fatal(err)
	}
	if err := nominator.markPlacementGated(context.Background(), run.Name, second); err != nil {
		t.Fatal(err)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for i := range updated.Status.Nominations {
		nomination := &updated.Status.Nominations[i]
		if nomination.Phase != repackv1alpha1.PodPlacementGated {
			t.Fatalf("nomination was not claimed: %+v", nomination)
		}
		got[nomination.ReplacementPodName] = true
	}
	if !got[first.Name] || !got[second.Name] {
		t.Fatalf("replacement claims = %v, want both Pods", got)
	}
}

func TestEnsureReplacementPodGroupRecordsWorkloadRecreation(t *testing.T) {
	controller := true
	run := runWithNoms("run",
		repackv1alpha1.PodNomination{
			Namespace: "ns", PodGroupName: "old", VictimPodName: "old-0",
			NodeName: "n1", Phase: repackv1alpha1.PodPlacementPrepared,
		},
		repackv1alpha1.PodNomination{
			Namespace: "ns", PodGroupName: "old", VictimPodName: "old-1",
			NodeName: "n2", Phase: repackv1alpha1.PodPlacementPrepared,
		},
	)
	run.Status.Plan = &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
		Namespace: "ns", PodGroupName: "old",
		Owner: &repackv1alpha1.WorkloadRef{APIVersion: "serving.example/v1", Kind: "Serving", Name: "model"},
	}}}
	newPodGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "new",
		Annotations: map[string]string{
			repackv1alpha1.PlacementLeaseAnnotation: placement.OwnerValue(run.Name, run.UID),
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "serving.example/v1", Kind: "Serving", Name: "model", Controller: &controller,
		}},
	}}
	pod := pendingPod("ns", "new-0", "new", nil)
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy(), newPodGroup)
	nominator := &Nominator{
		volcanoClient: volcanoClient,
		recorder:      record.NewFakeRecorder(10),
	}

	updated, err := nominator.ensureReplacementPodGroup(context.Background(), run.DeepCopy(), pod)
	if err != nil {
		t.Fatal(err)
	}
	for index := range updated.Status.Nominations {
		if got := updated.Status.Nominations[index].ReplacementPodGroupName; got != "new" {
			t.Fatalf("nomination[%d].replacementPodGroupName = %q, want new", index, got)
		}
	}
	expectRecorderEvent(t, nominator.recorder.(*record.FakeRecorder), eventReasonPodGroupRecreated)

	// Reconciliation is idempotent: the durable mapping is reused and no second
	// status mutation is required.
	again, err := nominator.ensureReplacementPodGroup(context.Background(), updated.DeepCopy(), pod)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status.Nominations[0].ReplacementPodGroupName != "new" {
		t.Fatalf("durable mapping was not retained: %+v", again.Status.Nominations[0])
	}
}

func TestSourcePodGroupForReplacementIncludesNamespace(t *testing.T) {
	run := runWithNoms("run", repackv1alpha1.PodNomination{
		Namespace: "namespace-a", PodGroupName: "old", ReplacementPodGroupName: "new",
	})
	if source, found := sourcePodGroupForReplacement(run, "namespace-a", "new"); !found || source != "old" {
		t.Fatalf("namespace-a/new mapping = %q,%t; want old,true", source, found)
	}
	if source, found := sourcePodGroupForReplacement(run, "namespace-b", "new"); found || source != "" {
		t.Fatalf("namespace-b/new must not reuse namespace-a mapping: %q,%t", source, found)
	}
}

func TestEnsureReplacementPodGroupAdvancesAfterRepeatedRecreation(t *testing.T) {
	controller := true
	run := runWithNoms("run",
		repackv1alpha1.PodNomination{
			Namespace: "ns", PodGroupName: "old", ReplacementPodGroupName: "replacement-v1",
			VictimPodName: "worker-0", NodeName: "node-a", SelectedNodeName: "node-b",
			ReplacementPodName: "replacement-v1-0", ReplacementPodUID: types.UID("replacement-v1-uid"),
			Phase: repackv1alpha1.PodPlacementNominated,
		},
		repackv1alpha1.PodNomination{
			Namespace: "ns", PodGroupName: "old", ReplacementPodGroupName: "replacement-v1",
			VictimPodName: "worker-1", NodeName: "node-a", SelectedNodeName: "node-b",
			ReplacementPodName: "replacement-v1-1", ReplacementPodUID: types.UID("replacement-v1-uid-1"),
			ActualNodeName: "node-b", Phase: repackv1alpha1.PodPlacementPlaced,
		},
	)
	run.Status.Plan = &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
		Namespace: "ns", PodGroupName: "old",
		Owner: &repackv1alpha1.WorkloadRef{APIVersion: "serving.example/v1", Kind: "Serving", Name: "model"},
	}}}
	replacementV2 := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "replacement-v2",
		Annotations: map[string]string{
			repackv1alpha1.PlacementLeaseAnnotation: placement.OwnerValue(run.Name, run.UID),
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "serving.example/v1", Kind: "Serving", Name: "model", Controller: &controller,
		}},
	}}
	pod := pendingPod("ns", "worker-0", "replacement-v2", nil)
	nominator := &Nominator{
		volcanoClient: vcfake.NewSimpleClientset(run.DeepCopy(), replacementV2),
		recorder:      record.NewFakeRecorder(10),
		now:           time.Now,
	}

	updated, err := nominator.ensureReplacementPodGroup(context.Background(), run.DeepCopy(), pod)
	if err != nil {
		t.Fatal(err)
	}
	for index := range updated.Status.Nominations {
		nomination := &updated.Status.Nominations[index]
		if nomination.ReplacementPodGroupName != "replacement-v2" {
			t.Fatalf("nomination[%d] replacement PodGroup = %q, want replacement-v2", index, nomination.ReplacementPodGroupName)
		}
		if nomination.Phase != repackv1alpha1.PodPlacementPrepared ||
			nomination.ReplacementPodName != "" || nomination.ReplacementPodUID != "" ||
			nomination.SelectedNodeName != "" || nomination.ActualNodeName != "" {
			t.Fatalf("nomination[%d] was not reset for the next PodGroup generation: %+v", index, nomination)
		}
	}
	if nomination, _ := nominator.matchNominationInRuns(pod, []*repackv1alpha1.RepackRun{updated}); nomination == nil {
		t.Fatal("same-name Pod in replacement-v2 did not match the advanced PodGroup mapping")
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

func TestEnqueueTerminalRunUsesGateOwnerIndex(t *testing.T) {
	run := runWithNoms("run")
	run.Status.Phase = repackv1alpha1.RepackFailed
	pod := pendingPod("ns", "held-scale-out", "group", nil)
	pod.Status.NominatedNodeName = "n2"
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = placement.OwnerValue(run.Name, run.UID)
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		podGroupIndexName:           podGroupIndex,
		placementGateOwnerIndexName: placementGateOwnerIndex,
	})
	if err := podIndexer.Add(pod); err != nil {
		t.Fatal(err)
	}
	nominator := &Nominator{
		podLister:               corelisters.NewPodLister(podIndexer),
		podIndexer:              podIndexer,
		podGroupIndexAvailable:  true,
		gateOwnerIndexAvailable: true,
		workQueue:               workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}
	defer nominator.workQueue.ShutDown()

	nominator.enqueuePendingForRun(run)
	if nominator.workQueue.Len() != 1 {
		t.Fatalf("queued pods = %d, want owner-marked Pod", nominator.workQueue.Len())
	}
	key, _ := nominator.workQueue.Get()
	defer nominator.workQueue.Done(key)
	if key != "ns/held-scale-out" {
		t.Fatalf("queued key = %q", key)
	}
}
