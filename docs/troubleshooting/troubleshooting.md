# Troubleshooting

This section provides operational troubleshooting guides for Volcano.

## Guides

| Guide | Description |
|-------|-------------|
| [Scheduling Pending Guide](./scheduling-pending-guide.md) | **Most common**: cluster appears to have resources, but Pods stay Pending (Gang, affinity, fragmentation, topology) |

## Quick Health Check

```bash
kubectl get pods -n volcano-system
kubectl get pg,queue,vcjob -A
kubectl get cm -n volcano-system volcano-scheduler-configmap -o yaml
```

## Escalation

If the issue cannot be resolved using the guides above, contact the Volcano team via the [support page](../getting-started/support.md) with:

1. `kubectl describe pod <pod>` — PodScheduled Message
2. `kubectl describe pg <pg>` — Conditions
3. `kubectl get vcjob <job> -o yaml` — minAvailable, networkTopology
4. Scheduler logs: `kubectl logs -n volcano-system deploy/volcano-scheduler --tail=100 | grep -iE 'reject|gang|overused'`
