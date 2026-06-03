/*
Copyright 2017 The Kubernetes Authors.
Copyright 2018-2025 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"volcano.sh/apis/pkg/apis/scheduling"
)

func TestResolvePodGroupTermTier(t *testing.T) {
	tierNameMap := HyperNodeTierNameMap{"supernode": 2}
	tier2 := int32(2)

	tests := []struct {
		name    string
		term    scheduling.PodGroupAffinityTerm
		want    int
		wantErr string
	}{
		{
			name: "topologyTierName",
			term: scheduling.PodGroupAffinityTerm{TopologyTierName: "supernode"},
			want: 2,
		},
		{
			name: "topologyTier",
			term: scheduling.PodGroupAffinityTerm{TopologyTier: &tier2},
			want: 2,
		},
		{
			name:    "mutually exclusive fields",
			term:    scheduling.PodGroupAffinityTerm{TopologyTierName: "supernode", TopologyTier: &tier2},
			wantErr: "mutually exclusive",
		},
		{
			name:    "missing both tier fields",
			term:    scheduling.PodGroupAffinityTerm{},
			wantErr: "must be set",
		},
		{
			name:    "unknown topologyTierName",
			term:    scheduling.PodGroupAffinityTerm{TopologyTierName: "missing"},
			wantErr: "unknown topologyTierName",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolvePodGroupTermTier(tt.term, tierNameMap)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("want tier %d, got %d", tt.want, got)
			}
		})
	}
}

func TestPodGroupMatchesTerm(t *testing.T) {
	selector := &metav1.LabelSelector{
		MatchLabels: map[string]string{"topology.volcano.sh/group": "prod"},
	}
	selfJob := &JobInfo{UID: "self", Namespace: "default", PodGroup: &PodGroup{}}
	otherJob := &JobInfo{
		UID:       "other",
		Namespace: "default",
		PodGroup: &PodGroup{
			PodGroup: scheduling.PodGroup{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"topology.volcano.sh/group": "prod"}},
			},
		},
	}

	tests := []struct {
		name string
		term scheduling.PodGroupAffinityTerm
		self *JobInfo
		other *JobInfo
		want bool
	}{
		{
			name: "matching label in same namespace",
			term: scheduling.PodGroupAffinityTerm{PodGroupSelector: selector},
			self: selfJob, other: otherJob, want: true,
		},
		{
			name: "self job is excluded",
			term: scheduling.PodGroupAffinityTerm{PodGroupSelector: selector},
			self: otherJob, other: otherJob, want: false,
		},
		{
			name: "label mismatch",
			term: scheduling.PodGroupAffinityTerm{PodGroupSelector: selector},
			self: selfJob,
			other: &JobInfo{
				UID: "other", Namespace: "default",
				PodGroup: &PodGroup{PodGroup: scheduling.PodGroup{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"topology.volcano.sh/group": "staging"}},
				}},
			},
			want: false,
		},
		{
			name: "nil podGroupSelector",
			term: scheduling.PodGroupAffinityTerm{},
			self: selfJob, other: otherJob, want: false,
		},
		{
			name: "different namespace without selector",
			term: scheduling.PodGroupAffinityTerm{PodGroupSelector: selector},
			self: selfJob,
			other: &JobInfo{
				UID: "other", Namespace: "other-ns",
				PodGroup: otherJob.PodGroup,
			},
			want: false,
		},
		{
			name: "nil other job",
			term: scheduling.PodGroupAffinityTerm{PodGroupSelector: selector},
			self: selfJob, other: nil, want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PodGroupMatchesTerm(tt.term, tt.self, tt.other); got != tt.want {
				t.Fatalf("PodGroupMatchesTerm() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchingPodGroupsAllocatedHyperNodesForTerm(t *testing.T) {
	tierNameMap := HyperNodeTierNameMap{"supernode": 2, "rack": 1}
	hn := HyperNodeInfoMap{
		"root":  newTestHyperNode("root", 3, "cluster", ""),
		"sn-a":  newTestHyperNode("sn-a", 2, "supernode", "root"),
		"sn-b":  newTestHyperNode("sn-b", 2, "supernode", "root"),
		"cab-a": newTestHyperNode("cab-a", 1, "rack", "sn-a"),
	}
	selector := &metav1.LabelSelector{
		MatchLabels: map[string]string{"topology.volcano.sh/group": "prod"},
	}
	selfJob := &JobInfo{UID: "self", Namespace: "default", PodGroup: &PodGroup{}}

	tests := []struct {
		name      string
		jobs      map[JobID]*JobInfo
		term      scheduling.PodGroupAffinityTerm
		want      sets.Set[string]
		wantErr   bool
	}{
		{
			name: "single matching peer at supernode tier",
			jobs: map[JobID]*JobInfo{
				"other": {
					UID: "other", Namespace: "default", AllocatedHyperNode: "sn-a",
					PodGroup: &PodGroup{PodGroup: scheduling.PodGroup{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"topology.volcano.sh/group": "prod"}},
					}},
				},
			},
			term: scheduling.PodGroupAffinityTerm{PodGroupSelector: selector, TopologyTierName: "supernode"},
			want: sets.New("sn-a"),
		},
		{
			name: "rack tier resolves ancestor hyperNode",
			jobs: map[JobID]*JobInfo{
				"other": {
					UID: "other", Namespace: "default", AllocatedHyperNode: "cab-a",
					PodGroup: &PodGroup{PodGroup: scheduling.PodGroup{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"topology.volcano.sh/group": "prod"}},
					}},
				},
			},
			term: scheduling.PodGroupAffinityTerm{PodGroupSelector: selector, TopologyTierName: "rack"},
			want: sets.New("cab-a"),
		},
		{
			name: "multiple matching peers collapse to set",
			jobs: map[JobID]*JobInfo{
				"j1": {
					UID: "j1", Namespace: "default", AllocatedHyperNode: "sn-a",
					PodGroup: &PodGroup{PodGroup: scheduling.PodGroup{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"topology.volcano.sh/group": "prod"}},
					}},
				},
				"j2": {
					UID: "j2", Namespace: "default", AllocatedHyperNode: "sn-b",
					PodGroup: &PodGroup{PodGroup: scheduling.PodGroup{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"topology.volcano.sh/group": "prod"}},
					}},
				},
			},
			term: scheduling.PodGroupAffinityTerm{PodGroupSelector: selector, TopologyTierName: "supernode"},
			want: sets.New("sn-a", "sn-b"),
		},
		{
			name: "self job in map is ignored",
			jobs: map[JobID]*JobInfo{
				"self": {
					UID: "self", Namespace: "default", AllocatedHyperNode: "sn-a",
					PodGroup: &PodGroup{PodGroup: scheduling.PodGroup{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"topology.volcano.sh/group": "prod"}},
					}},
				},
			},
			term: scheduling.PodGroupAffinityTerm{PodGroupSelector: selector, TopologyTierName: "supernode"},
			want: sets.New[string](),
		},
		{
			name: "peer without AllocatedHyperNode is skipped",
			jobs: map[JobID]*JobInfo{
				"other": {
					UID: "other", Namespace: "default",
					PodGroup: &PodGroup{PodGroup: scheduling.PodGroup{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"topology.volcano.sh/group": "prod"}},
					}},
				},
			},
			term: scheduling.PodGroupAffinityTerm{PodGroupSelector: selector, TopologyTierName: "supernode"},
			want: sets.New[string](),
		},
		{
			name: "invalid term returns error",
			jobs: map[JobID]*JobInfo{},
			term: scheduling.PodGroupAffinityTerm{PodGroupSelector: selector, TopologyTierName: "missing"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MatchingPodGroupsAllocatedHyperNodesForTerm(tt.jobs, hn, tierNameMap, selfJob, tt.term)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("want %v, got %v", tt.want.UnsortedList(), got.UnsortedList())
			}
		})
	}
}

func TestJobTopologyAffinityHelpers(t *testing.T) {
	required := []scheduling.PodGroupAffinityTerm{{TopologyTierName: "supernode"}}
	preferred := []scheduling.PodGroupAffinityTerm{{TopologyTierName: "rack", Weight: 1}}

	withRequired := &JobInfo{
		PodGroup: &PodGroup{PodGroup: scheduling.PodGroup{Spec: scheduling.PodGroupSpec{
			TopologyAffinity: &scheduling.TopologyAffinitySpec{
				PodGroupAntiAffinity: &scheduling.PodGroupAntiAffinity{Required: required},
			},
		}}},
	}
	withPreferred := &JobInfo{
		PodGroup: &PodGroup{PodGroup: scheduling.PodGroup{Spec: scheduling.PodGroupSpec{
			TopologyAffinity: &scheduling.TopologyAffinitySpec{
				PodGroupAntiAffinity: &scheduling.PodGroupAntiAffinity{Preferred: preferred},
			},
		}}},
	}
	withBoth := &JobInfo{
		PodGroup: &PodGroup{PodGroup: scheduling.PodGroup{Spec: scheduling.PodGroupSpec{
			TopologyAffinity: &scheduling.TopologyAffinitySpec{
				PodGroupAntiAffinity: &scheduling.PodGroupAntiAffinity{
					Required: required, Preferred: preferred,
				},
			},
		}}},
	}

	tests := []struct {
		name       string
		job        *JobInfo
		hard       bool
		soft       bool
		withTopo   bool
		reqLen     int
		prefLen    int
	}{
		{name: "required only", job: withRequired, hard: true, withTopo: true, reqLen: 1},
		{name: "preferred only", job: withPreferred, soft: true, withTopo: true, prefLen: 1},
		{name: "both", job: withBoth, hard: true, soft: true, withTopo: true, reqLen: 1, prefLen: 1},
		{name: "nil podgroup", job: &JobInfo{}, hard: false, soft: false, withTopo: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.job.ContainsHardPodGroupAntiAffinity(); got != tt.hard {
				t.Fatalf("ContainsHardPodGroupAntiAffinity = %v, want %v", got, tt.hard)
			}
			if got := tt.job.HasPreferredPodGroupAntiAffinity(); got != tt.soft {
				t.Fatalf("HasPreferredPodGroupAntiAffinity = %v, want %v", got, tt.soft)
			}
			if got := tt.job.WithTopologyAffinity(); got != tt.withTopo {
				t.Fatalf("WithTopologyAffinity = %v, want %v", got, tt.withTopo)
			}
			if len(tt.job.RequiredPodGroupAntiAffinityTerms()) != tt.reqLen {
				t.Fatalf("RequiredPodGroupAntiAffinityTerms len = %d, want %d", len(tt.job.RequiredPodGroupAntiAffinityTerms()), tt.reqLen)
			}
			if len(tt.job.PreferredPodGroupAntiAffinityTerms()) != tt.prefLen {
				t.Fatalf("PreferredPodGroupAntiAffinityTerms len = %d, want %d", len(tt.job.PreferredPodGroupAntiAffinityTerms()), tt.prefLen)
			}
		})
	}
}

func newTestHyperNode(name string, tier int, tierName, parent string) *HyperNodeInfo {
	return &HyperNodeInfo{
		Name:     name,
		tier:     tier,
		tierName: tierName,
		Parent:   parent,
		Children: sets.New[string](),
	}
}
