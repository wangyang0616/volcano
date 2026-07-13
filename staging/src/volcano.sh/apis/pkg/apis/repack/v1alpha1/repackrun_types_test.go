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

package v1alpha1

import "testing"

func TestRepackRunSpecDeepCopyEviction(t *testing.T) {
	gracePeriodSeconds := int64(30)
	original := &RepackRunSpec{
		Eviction: &EvictionPolicy{GracePeriodSeconds: &gracePeriodSeconds},
	}

	copy := original.DeepCopy()
	if copy == nil || copy.Eviction == nil || copy.Eviction.GracePeriodSeconds == nil {
		t.Fatal("DeepCopy() did not preserve eviction grace period")
	}
	if copy.Eviction == original.Eviction || copy.Eviction.GracePeriodSeconds == original.Eviction.GracePeriodSeconds {
		t.Fatal("DeepCopy() retained an eviction pointer from the original")
	}

	*copy.Eviction.GracePeriodSeconds = 0
	if got := *original.Eviction.GracePeriodSeconds; got != 30 {
		t.Fatalf("original gracePeriodSeconds = %d, want 30", got)
	}
}
