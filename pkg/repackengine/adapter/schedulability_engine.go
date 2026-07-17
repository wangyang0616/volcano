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

// This file intentionally holds no code: the reserved Statement-based feasibility
// check (EngineFit / ValidatePlan) was removed as dead code — repack feasibility
// now runs through Snapshot.FeasibleRelocation, which clones the node + cycle-state
// and evaluates the scheduler's full filter stack via ssn.SimulatePredicateFn (see
// snapshot_session.go). Delete this file with `git rm`; it is kept empty only
// because the build sandbox cannot remove files.
package adapter
