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

package state

import (
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	batch "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	"volcano.sh/volcano/pkg/controllers/apis"
	jobhelpers "volcano.sh/volcano/pkg/controllers/job/helpers"
)

func TestTerminalObservationFenceSkipsDiscardedPods(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*v1.Pod)
		handled bool
		want    bool
	}{
		{name: "unhandled current failure", want: false},
		{name: "acknowledged failure", handled: true, want: true},
		{name: "old version", mutate: func(p *v1.Pod) { p.Annotations[batch.JobVersion] = "0" }, want: true},
		{name: "out of sync", mutate: func(p *v1.Pod) { p.Annotations[jobhelpers.OutOfSyncKey] = "true" }, want: true},
		{name: "terminating", mutate: func(p *v1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now }, want: true},
		{name: "running", mutate: func(p *v1.Pod) { p.Status.Phase = v1.PodRunning }, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", UID: "pod-uid", Annotations: map[string]string{batch.JobVersion: "1"}},
				Status: v1.PodStatus{Phase: v1.PodFailed}}
			if test.mutate != nil {
				test.mutate(pod)
			}
			info := &apis.JobInfo{Job: &batch.Job{Status: batch.JobStatus{Version: 1}}, Pods: map[string]map[string]*v1.Pod{"worker": {"pod": pod}}}
			if test.handled {
				info.MarkTerminalPodHandled("worker", pod.Name, pod.UID)
			}
			if got := terminalPodsHandled(info); got != test.want {
				t.Fatalf("handled=%v, want %v", got, test.want)
			}
		})
	}
}

func BenchmarkTerminalObservationFence5000(b *testing.B) {
	for _, terminal := range []bool{false, true} {
		b.Run(fmt.Sprintf("terminal=%v", terminal), func(b *testing.B) {
			info := &apis.JobInfo{Job: &batch.Job{}, Pods: map[string]map[string]*v1.Pod{"worker": {}}}
			for i := range 5000 {
				pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), UID: types.UID(fmt.Sprintf("uid-%d", i)), Annotations: map[string]string{batch.JobVersion: "0"}},
					Status: v1.PodStatus{Phase: v1.PodRunning}}
				if terminal {
					pod.Status.Phase = v1.PodSucceeded
				}
				info.Pods["worker"][pod.Name] = pod
				if terminal {
					info.MarkTerminalPodHandled("worker", pod.Name, pod.UID)
				}
			}
			b.Run("check", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if !terminalPodsHandled(info) {
						b.Fatal("unexpected fence")
					}
				}
			})
			b.Run("clone", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = info.Clone()
				}
			})
		})
	}
}
