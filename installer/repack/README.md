# Repack (runtime defragmentation) — deployment

Repack ships in two pieces:

- **RepackRun controller + nomination reconciler** — manages the `RepackRun`
  lifecycle (admission, the Execute K=1 gate + cooldown, phase/conditions, TTL GC)
  and steers replacement pods. Runs **by default inside volcano-controller-manager**;
  can also run standalone.
- **volcano-repack-engine** — a standalone Deployment that reuses the scheduler
  cache + the same `scheduler-conf`, plans the defrag, and (Execute) evicts.

## 1. Install the CRD

The `RepackRun` CRD is generated from the kubebuilder markers on the API types:

```bash
make manifests   # → config/crd/.../repack.volcano.sh_repackruns.yaml
kubectl apply -f config/crd/<...>/repack.volcano.sh_repackruns.yaml
```

## 2. Choose a controller deployment mode

### Default — built-in (recommended)

The controller is compiled into volcano-controller-manager and enabled by the
default `--controllers=*`. Just grant the extra RBAC:

```bash
kubectl apply -f installer/repack/repack-controller-rbac.yaml
```

Do **not** deploy `repack-controller-standalone.yaml` in this mode.

### Standalone (optional)

Run the controller as its own Deployment and **disable the built-in copy** so the
two don't both reconcile the same objects:

```bash
# tell volcano-controller-manager to skip the built-in repack controller:
#   --controllers=*,-repack-controller
kubectl apply -f installer/repack/repack-controller-standalone.yaml
```

Do **not** apply `repack-controller-rbac.yaml` in this mode (the standalone file
carries its own ServiceAccount + role).

## 3. Install the engine

```bash
kubectl apply -f installer/repack/repack-engine.yaml
```

The engine mounts `volcano-scheduler-configmap` for `--scheduler-conf`, so it sees
the cluster exactly as the scheduler does. Adjust `--repack-default-resource`
(e.g. `nvidia.com/gpu`) and `--repack-algorithm` (P0: `drain`) as needed.

## Notes

- Namespace is `volcano-system` throughout; adjust if your install differs.
- Images: `make vc-repack-engine-image` and `make vc-repack-controller-image`
  (Dockerfiles under `installer/dockerfile/repack-engine` /
  `installer/dockerfile/repack-controller`; binaries via `make vc-repack-engine`
  / `make vc-repack-controller`). The standalone controller builds from its own
  module under `staging/src/volcano.sh/repack-controller`.
- Helm chart integration (templating these under `installer/helm/...`): TODO.
