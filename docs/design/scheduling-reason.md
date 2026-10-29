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
    | Task1     | **Schedulable**    | **Pod ns1/task-1 can possibly be assigned to node1, once minAvailable is satisfied**                                                                                                            |
    | Task2     | **Schedulable**    | **Pod ns1/task-2 can possibly be assigned to node2, once minAvailable is satisfied**                                                                                                            |
    | Task3     | **Schedulable**    | **Pod ns1/task-3 can possibly be assigned to node3, once minAvailable is satisfied**                                                                                                            |
    | Task4     | Unschedulable      | all nodes are unavailable: 1 plugin InterPodAffinity predicates failed node(s) didn't match pod affinity/anti-affinity, node(s) didn't match pod affinity rules, 2 node(s) resource fit failed. |
    | Task5 ~ 6 | Unschedulable      | **3/6** tasks in gang unschedulable: pod group is not ready, 6 Pending, 6 minAvailable; **Pending: 3 Unschedulable, 3 Schedulable**                                                             |

    - Note: Task5 ~ 6 are `Unschedulable` maybe because that this two locate after task4 by `TaskOrderFn`;

- In improved information, we can easily find the one that breaks the whole scheduling cycle and why dose that happen. Additionally,  we can find the histogram of reason why there are some tasks whose status is pending.

### Rolled-back Pipeline attempts

`LastTransaction` preserves a task's context before a statement is discarded.
`TaskSchedulingReason` continues to expose that attempt for diagnostics and gang
statistics, including its candidate node. `TaskSchedulingCondition` shares the
same formatting logic but reports a rolled-back Pipeline as Pending when the
live context is Pending with no node. This prevents candidate-node changes, or
alternating between a Pipeline attempt and no attempt, from changing the Pod
condition when the failure diagnostics are otherwise unchanged.

Pending conditions include the Job's HyperNode diagnosis and the task's own
node-fit errors. Without either, they fall back to the current Job state and
the aggregate Pending reason counts, without another task's node-fit errors.
For Pod conditions, a Pipeline without a nomination counts as Pending in this
fallback summary; Job diagnostics still report its live Pipeline status. The
`Schedulable` reason for Allocated attempts retains its behavior.

A live Pipeline is only a trial placement until it actually evicts a victim.
The Pod condition includes a candidate node only when that eviction has produced
a nomination. In later sessions, the victim may still be terminating and no new
eviction may occur. The condition continues to show the nomination stored in
`Pod.Status.NominatedNodeName`, even if the current session has no Pipeline or
tries a different node without evicting. A new eviction may replace the
nomination. When the existing scheduling fallback invalidates a nomination,
the condition follows the nomination value visible on the task's Pod. This
change leaves nomination creation, invalidation, and scheduling decisions as
they were.

Changes to HyperNode or task fit errors still update the Pod condition; this
separation does not cache old errors or suppress newly observed failures.

The `scheduling-condition` E2E cases run as part of the existing scheduling
actions suite. They use two workers, preemptible reservations, and a blocked
gang member to force Pipeline rollback. Changing only a soft node-affinity
label selects each candidate deterministically; Pod specs and resources stay
unchanged during observation. The cases separately watch candidate-node changes
and Pipeline presence/absence, and require at least three completed trial and
rollback sessions per selection using existing scheduler V(3) logs. A Pod watch
starts at the baseline List resourceVersion and rejects any status change or
watch error. Finally, releasing reservations and unblocking the gang must let
the same Pods run. A third case performs a real eviction with a long terminating
victim, then checks that the Pod status remains unchanged for several complete
sessions and that the nominated Pod ultimately binds.

On an existing test cluster with one scheduler (verbosity >= 3) and two ready
workers with at least 800m CPU free each, run only these cases with:

```sh
KUBECONFIG=/path/to/kubeconfig go test ./test/e2e/schedulingaction \
  -run TestE2E -count=1 -ginkgo.label-filter=scheduling-condition
```

The fixture restores scheduler configuration and temporary node labels, and
deletes only its own namespace and PriorityClass. Existing HyperNodes are kept.
