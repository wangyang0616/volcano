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

package framework

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

type gangFixture struct {
	nn   string
	lbls map[string]string
}

func gangInfoFrom(m map[schedapi.JobID]gangFixture) GangInfo {
	return func(id schedapi.JobID) (string, labels.Labels, bool) {
		g, ok := m[id]
		if !ok {
			return "", nil, false
		}
		return g.nn, labels.Set(g.lbls), true
	}
}

func sel(ml map[string]string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: ml}
}

// nil scope = whole domain: every known gang and every node is in scope.
func TestResolveScope_NilIsAll(t *testing.T) {
	gi := gangInfoFrom(map[schedapi.JobID]gangFixture{"ns/a": {"ns/a", nil}})
	in, nodeIn, err := ResolveScope(nil, gi)
	if err != nil {
		t.Fatal(err)
	}
	if !in("ns/a") {
		t.Error("nil scope should admit any known gang")
	}
	if in("ns/unknown") {
		t.Error("unknown gang must be out of scope")
	}
	if !nodeIn(node("n0", nil)) {
		t.Error("nil scope should admit any node")
	}
}

// Node include selector restricts to matching nodes; exclude-by-name wins.
func TestResolveScope_NodeSelectorAndExclude(t *testing.T) {
	scope := &repackv1alpha1.RepackScope{
		Nodes: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Selector: sel(map[string]string{"pool": "a100"})},
			Exclude: &repackv1alpha1.RepackSelector{Names: []string{"n-guard"}},
		},
	}
	_, nodeIn, err := ResolveScope(scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !nodeIn(node("n1", map[string]string{"pool": "a100"})) {
		t.Error("a100 node should be in scope")
	}
	if nodeIn(node("n2", map[string]string{"pool": "v100"})) {
		t.Error("non-a100 node should be out of scope")
	}
	if nodeIn(node("n-guard", map[string]string{"pool": "a100"})) {
		t.Error("excluded-by-name node must be out even if it matches include")
	}
}

// PodGroup include by names restricts the gang set.
func TestResolveScope_PodGroupNames(t *testing.T) {
	gi := gangInfoFrom(map[schedapi.JobID]gangFixture{
		"ns/keep": {"ns/keep", map[string]string{"team": "ml"}},
		"ns/drop": {"ns/drop", map[string]string{"team": "ml"}},
	})
	scope := &repackv1alpha1.RepackScope{
		PodGroups: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Names: []string{"ns/keep"}},
		},
	}
	in, _, err := ResolveScope(scope, gi)
	if err != nil {
		t.Fatal(err)
	}
	if !in("ns/keep") {
		t.Error("named gang should be in scope")
	}
	if in("ns/drop") {
		t.Error("unnamed gang should be out of scope")
	}
}

// PodGroup include-by-selector matches PG labels; exclude-by-label wins.
// This is the "everything is a PodGroup" path: for Deployment/StatefulSet/custom
// workloads the pg-controller inherits pod template labels onto the PodGroup
// (§5.2.1), so a PG label selector addresses them uniformly.
func TestResolveScope_PodGroupSelector(t *testing.T) {
	gi := gangInfoFrom(map[schedapi.JobID]gangFixture{
		"ns/dep-x": {"ns/dep-x", map[string]string{"app": "recommender"}},
		"ns/dep-y": {"ns/dep-y", map[string]string{"app": "ranking"}},
		"ns/prot":  {"ns/prot", map[string]string{"app": "recommender", "repack.volcano.sh/protected": "true"}},
	})
	scope := &repackv1alpha1.RepackScope{
		PodGroups: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Selector: sel(map[string]string{"app": "recommender"})},
			Exclude: &repackv1alpha1.RepackSelector{Selector: sel(map[string]string{"repack.volcano.sh/protected": "true"})},
		},
	}
	in, _, err := ResolveScope(scope, gi)
	if err != nil {
		t.Fatal(err)
	}
	if !in("ns/dep-x") {
		t.Error("gang matching include selector should be in scope")
	}
	if in("ns/dep-y") {
		t.Error("gang not matching include selector should be out of scope")
	}
	if in("ns/prot") {
		t.Error("gang matching exclude selector must be out even if it matches include")
	}
}

// A malformed selector is a resolve-time error.
func TestResolveScope_BadSelectorErrors(t *testing.T) {
	scope := &repackv1alpha1.RepackScope{
		Nodes: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "x", Operator: "BadOperator"}},
				},
			},
		},
	}
	if _, _, err := ResolveScope(scope, nil); err == nil {
		t.Error("malformed label selector should be a resolve-time error")
	}
}
