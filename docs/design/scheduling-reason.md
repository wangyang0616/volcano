# Scheduling Reason

[@eggiter](https://github.com/eggiter); Aug 13, 2021
[@tgaddair](https://github.com/tgaddair); Dec 12, 2022
[@ouyangshengjia](https://github.com/ouyangshengjia); Mar 20, 2025

## Table of Contents

* [Table of Contents](#table-of-contents)
* [Problem](#problem)
* [Solution](#solution)

## Problem

- Currently, the reason and message in `pod.status.conditions["PodScheduled"]` and `podgroup.status.conditions["Unschedulable"]` is **NOT** informative, for example `x/x tasks in gang unschedulable: pod group is not ready, x Pending, x minAvailable`.
- Users want to know which one breaks the scheduling cycle and why is that.
- (@tgaddair): However, cluster autoscaling systems like [Karpenter](https://github.com/aws/karpenter) explicitly check for the `Unschedulable` reason on the pod to trigger scale-up (see [here](https://github.com/aws/karpenter-core/blob/fedb036e598fb51cda28bd5ae150743df009312f/pkg/utils/pod/scheduling.go#L25)). As such, we need to keep the `Reason` field consistent with default scheduler behavior.

## Solution

- Introducing an extra scheduling reasons in PodScheduled PodCondition:
  + `Schedulable`: means that the scheduler can schedule the pod right now, but not bind yet.

- Case:
  + 3 nodes: node1, node2, node3;
  + 6 tasks in the PodGroup(`minAvailable = 6`);
    + 3 tasks(Task1 ~ 3) can be scheduled and other 3 tasks(Task4 ~ 6) is unschedulable, thus the whole job is unschedulable.
  
- Current information:
  +  | Tasks     | Reason             | Message                                                                                                                                                                                         |
     |-----------|--------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
     | PodGroup  | NotEnoughResources | 6/6 tasks in gang unschedulable: pod group is not ready, 6 Pending, 6 minAvailable                                                                                                              |
     | Task1 ~ 3 | Unschedulable      | 6/6 tasks in gang unschedulable: pod group is not ready, 6 Pending, 6 minAvailable                                                                                                              |
     | Task4 ~ 6 | Unschedulable      | all nodes are unavailable: 1 plugin InterPodAffinity predicates failed node(s) didn't match pod affinity/anti-affinity, node(s) didn't match pod affinity rules, 2 node(s) resource fit failed. |

- Improved information:
  + | Tasks     | Reason             | Message                                                                                                                                                                                         |
    |-----------|--------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
    | PodGroup  | NotEnoughResources | **3/6** tasks in gang unschedulable: pod group is not ready, 6 Pending, 6 minAvailable; **Pending: 3 Unschedulable, 3 Schedulable**                                                             |
    | Task1     | **Schedulable**    | **Pod ns1/task-1 last observed candidate node node1; scheduling requires minAvailable**                                                                                                        |
    | Task2     | **Schedulable**    | **Pod ns1/task-2 last observed candidate node node2; scheduling requires minAvailable**                                                                                                        |
    | Task3     | **Schedulable**    | **Pod ns1/task-3 last observed candidate node node3; scheduling requires minAvailable**                                                                                                        |
    | Task4     | Unschedulable      | all nodes are unavailable: 1 plugin InterPodAffinity predicates failed node(s) didn't match pod affinity/anti-affinity, node(s) didn't match pod affinity rules, 2 node(s) resource fit failed. |
    | Task5 ~ 6 | Unschedulable      | **3/6** tasks in gang unschedulable: pod group is not ready, 6 Pending, 6 minAvailable; **Pending: 3 Unschedulable, 3 Schedulable**                                                             |

    - Note: Task5 ~ 6 are `Unschedulable` maybe because that this two locate after task4 by `TaskOrderFn`;

- In improved information, we can easily find the one that breaks the whole scheduling cycle and why dose that happen. Additionally,  we can find the histogram of reason why there are some tasks whose status is pending.

### Pod candidate snapshots

`LastTransaction` preserves a task's trial placement before a statement is
discarded. Job diagnostics and gang counts continue to use that attempt.
For Pod conditions, the cache records the candidate nodes of all tasks in a
VCJob from one scheduling session. An uncommitted Allocated or Pipeline attempt
is presented as a **last observed candidate**, never as a binding or a
nomination. A Pipeline candidate has reason `Unschedulable`; an Allocated
candidate retains reason `Schedulable`.

Candidate-node changes alone are published at most once every five minutes.
When a session does not evaluate a previously published candidate, the cache
keeps the prior complete snapshot rather than combining nodes from two
sessions. A newly discovered candidate, a definite node-fit failure, a task
membership change, or a real nomination publishes the current Job-wide view
immediately. Thus another task in the VCJob may lose a stale candidate if it
was not evaluated in that session. Only Pods whose resulting condition differs
are written; unrelated VCJobs are not refreshed. There is no atomic multi-Pod
status update, so readers may briefly see old and new messages during writes.

Pending conditions include the Job's HyperNode diagnosis and the task's own
node-fit errors. Without either, they fall back to the current Job state, the
aggregate Pending reason counts, and one named task's node-fit error when
available. This preserves gang failure detail for tasks skipped after another
task fails, without attributing that error to the skipped task.

A real eviction may set `Pod.Status.NominatedNodeName`. Its existing message
and nomination behavior take precedence over candidate presentation, including
later sessions while a victim is terminating. Scheduler decisions and
nomination creation or invalidation are unchanged.

The `scheduling-condition` E2E cases run as part of the existing scheduling
actions suite. They use two workers, preemptible reservations, and a blocked
gang member to force Pipeline rollback. Changing only a soft node-affinity
label selects each candidate deterministically; Pod specs and resources stay
unchanged during observation. The cases separately watch candidate-node changes
and Pipeline presence/absence for both candidate Pods, and require at least three
completed trial and rollback sessions per selection using existing scheduler
V(3) logs. A Pod watch starts at the baseline List resourceVersion and rejects
any status change or watch error. Finally, releasing reservations and
unblocking the gang must let the same Pods run. A third case performs a real
eviction with a long terminating victim, then checks that the Pod status remains
unchanged for several complete sessions and that the nominated Pod ultimately
binds.

On an existing test cluster with one scheduler (verbosity >= 3) and two ready
workers with at least 800m CPU free each, run only these cases with:

```sh
KUBECONFIG=/path/to/kubeconfig go test ./test/e2e/schedulingaction \
  -run TestE2E -count=1 -ginkgo.label-filter=scheduling-condition
```

The fixture restores scheduler configuration and temporary node labels, and
deletes only its own namespace and PriorityClass. Existing HyperNodes are kept.
