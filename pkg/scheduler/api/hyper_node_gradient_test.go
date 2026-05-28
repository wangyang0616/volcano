/*
Copyright 2025 The Volcano Authors.

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

package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/sets"
)

func TestIntersectHyperNodeGradients(t *testing.T) {
	hnA := NewHyperNodeInfo(BuildHyperNode("a", 1, nil))
	hnB := NewHyperNodeInfo(BuildHyperNode("b", 1, nil))
	hnC := NewHyperNodeInfo(BuildHyperNode("c", 2, nil))

	plugin1 := [][]*HyperNodeInfo{{hnA, hnB}, {hnC}}
	plugin2 := [][]*HyperNodeInfo{{hnB}, {hnC}}

	got := IntersectHyperNodeGradients([][][]*HyperNodeInfo{plugin1, plugin2})
	assert.Equal(t, sets.New("b", "c"), got)
}

func TestRebuildGradientsByTier(t *testing.T) {
	hnA := NewHyperNodeInfo(BuildHyperNode("a", 1, nil))
	hnB := NewHyperNodeInfo(BuildHyperNode("b", 2, nil))
	hyperNodes := HyperNodeInfoMap{"a": hnA, "b": hnB}

	result := RebuildGradientsByTier(hyperNodes, sets.New("a", "b"))
	assert.Len(t, result, 2)
	assert.Equal(t, "a", result[0][0].Name)
	assert.Equal(t, "b", result[1][0].Name)
}
