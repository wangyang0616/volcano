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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

// GangInfo returns a gang's "namespace/name" and labels for scope matching.
// "Everything is a PodGroup": the labels are the PodGroup's own labels — for
// non-vcjob workloads the pg-controller inherits pod template labels onto the
// auto-created PodGroup (§5.2.1), so a PG label selector addresses every
// workload type uniformly (no need to read pods).
// ok=false means the JobID is unknown to the snapshot (treated as out of scope).
// The live-Session implementation is session.SessionGangInfo; tests use a func.
type GangInfo func(schedapi.JobID) (namespacedName string, lbls labels.Labels, ok bool)

type compiledTerm struct {
	includeAll bool
	incSel     labels.Selector
	incNames   map[string]bool
	excSel     labels.Selector
	excNames   map[string]bool
}

// matches applies "included AND NOT excluded", include/exclude each (selector ∪
// names); empty include = all, empty exclude = none.
func (c *compiledTerm) matches(name string, lbls labels.Labels) bool {
	included := c.includeAll ||
		(c.incNames != nil && c.incNames[name]) ||
		(c.incSel != nil && c.incSel.Matches(lbls))
	if !included {
		return false
	}
	excluded := (c.excNames != nil && c.excNames[name]) ||
		(c.excSel != nil && c.excSel.Matches(lbls))
	return !excluded
}

func compileSelector(sel *repackv1alpha1.RepackSelector) (ls labels.Selector, names map[string]bool, empty bool, err error) {
	if sel == nil || (sel.Selector == nil && len(sel.Names) == 0) {
		return nil, nil, true, nil
	}
	if sel.Selector != nil {
		ls, err = metav1.LabelSelectorAsSelector(sel.Selector)
		if err != nil {
			return nil, nil, false, err
		}
	}
	if len(sel.Names) > 0 {
		names = make(map[string]bool, len(sel.Names))
		for _, n := range sel.Names {
			names[n] = true
		}
	}
	return ls, names, false, nil
}

func compileTerm(t *repackv1alpha1.RepackSelectorTerm) (*compiledTerm, error) {
	c := &compiledTerm{includeAll: true}
	if t == nil {
		return c, nil
	}
	incSel, incNames, incEmpty, err := compileSelector(t.Include)
	if err != nil {
		return nil, fmt.Errorf("include: %w", err)
	}
	excSel, excNames, _, err := compileSelector(t.Exclude)
	if err != nil {
		return nil, fmt.Errorf("exclude: %w", err)
	}
	c.includeAll, c.incSel, c.incNames = incEmpty, incSel, incNames
	c.excSel, c.excNames = excSel, excNames
	return c, nil
}

// ResolveScope compiles a RepackScope into the in-scope (gang) and node-in-scope
// predicates the driver consumes (to build the scope Movable and to filter the
// snapshot's nodes). Selectors are parsed once; a malformed selector is a
// resolve-time error. A nil scope/axis means "whole domain" for that axis;
// exclude wins.
func ResolveScope(scope *repackv1alpha1.RepackScope, gangInfo GangInfo) (
	inScope func(schedapi.JobID) bool, nodeInScope func(*schedapi.NodeInfo) bool, err error) {

	var pgTerm, nodeTerm *repackv1alpha1.RepackSelectorTerm
	if scope != nil {
		pgTerm, nodeTerm = scope.PodGroups, scope.Nodes
	}
	pg, err := compileTerm(pgTerm)
	if err != nil {
		return nil, nil, fmt.Errorf("scope.podGroups: %w", err)
	}
	nd, err := compileTerm(nodeTerm)
	if err != nil {
		return nil, nil, fmt.Errorf("scope.nodes: %w", err)
	}

	inScope = func(id schedapi.JobID) bool {
		nn, lbls, ok := gangInfo(id)
		if !ok {
			return false
		}
		if lbls == nil {
			lbls = labels.Set{}
		}
		return pg.matches(nn, lbls)
	}
	nodeInScope = func(n *schedapi.NodeInfo) bool {
		if n == nil {
			return false
		}
		return nd.matches(n.Name, nodeLabels(n))
	}
	return inScope, nodeInScope, nil
}

func nodeLabels(n *schedapi.NodeInfo) labels.Labels {
	if n.Node != nil && n.Node.Labels != nil {
		return labels.Set(n.Node.Labels)
	}
	return labels.Set{}
}
