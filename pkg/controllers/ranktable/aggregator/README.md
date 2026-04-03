# RankTable aggregator

This package and the `vc-ranktable-aggregator` binary (`cmd/ranktable-aggregator`) implement the **consumer side** of large per-job RankTable distribution: read a mounted **index** ConfigMap, `GET` **shard** ConfigMaps from the API, verify hashes and sizes, decompress, and write a **single local file** for the workload. Shards are not mounted; each Pod pulls shards independently (no shared storage).

Extended narrative (diagrams, test plan): [ranktable-sharded-distribution.md](../../../../docs/design/ranktable-sharded-distribution.md).

---

## Design overview

### Problem

A full RankTable can exceed the Kubernetes ConfigMap size limit (~1 MiB). We store the payload as **compress-then-shard** ConfigMaps and use one **index** ConfigMap as the authoritative manifest.

### Components

| Role | Responsibility |
|------|----------------|
| **Producer** (your controller) | Build RankTable → compress → shard → create/update shard ConfigMaps → set index to `status=completed` last. |
| **Aggregator (single process)** | If the output file is **missing**, run a **bootstrap** reconcile; then watch the index path + periodic poll and reconcile on index/version changes. Deploy as a **Native Sidecar** (`initContainers` + `restartPolicy: Always`) so the kubelet ties lifecycle to the workload (see Deployment). |
| **Workload** | Read only the output file (e.g. JSON). **Startup race:** the sidecar starts together with app containers—retry or delay reads until the file exists (or until your readiness logic allows). |

### Data flow

1. Producer writes all shards for version `V`, then updates the index with `ranktable_cur_version=V` and `status=completed`.
2. Aggregator loads the mounted index file, parses the `shards` JSON list, and issues targeted `GET` requests for each shard.
3. Bytes are concatenated in shard `id` order, verified against `compressed_size` / `compressed_sha256`, decompressed per `encoding`, verified against `original_size` / `content_sha256`.
4. Output is written with **temp + fsync + rename**; on failure the previous file is left in place.

### Apiserver pressure (large clusters)

- No list/watch of shards; only `GET` by namespace/name from the manifest.
- Bounded concurrency (`--workers`) and client QPS (`--kube-api-qps`, burst ≈ `2 * workers`).
- Startup jitter spreads Pod bursts.
- Sidecar coalesces overlapping reconciles; optional `changed_shards` + `ranktable_prev_version` allows reusing cached shard bytes when the index marks an incremental update.

### Protocol summary (index `data` keys)

| Key | Meaning |
|-----|---------|
| `ranktable_cur_version` / `ranktable_prev_version` | Current and previous complete version strings. |
| `status` | `initializing` — ignore for switch; **`completed`** — safe to reconcile. |
| `protocol_version` | Must be `v1.0`. |
| `encoding` | `zstd`, `gzip`, or `identity`. |
| `chunk_size`, `total_shards`, `compressed_size`, `original_size` | Sizes for validation. |
| `compressed_sha256`, `content_sha256` | Integrity of merged compressed stream and final content. |
| `max_original_size` | Optional cap in index; runtime also enforces `--max-original-size`. |
| `shards` | JSON array: `id`, `namespace`, `name`, `size`, `sha256` per shard. |
| `changed_shards` | Optional JSON array of shard ids changed vs `prev_version` (incremental hint). |

Shard ConfigMap: key `ranktable_shard_info` — base64-encoded chunk bytes (plain string falls back for debugging).

**Publish order:** all shards for `V` first, then index with `status=completed`.

Example manifests (authoritative field shapes): `../ranktable-index-configmap.yaml`, `../ranktable-shard-configmap.yaml`, `../ranktable.yaml`.

---

## Building And Packaging

From the repository root:

```bash
# Format + unit tests (recommended before build)
GOFLAGS=-mod=mod go test ./pkg/controllers/ranktable/aggregator/...

# Build binary
make vc-ranktable-aggregator

# Or build directly
GOFLAGS=-mod=mod go build -o _output/bin/vc-ranktable-aggregator ./cmd/ranktable-aggregator
```

Binary output: `_output/bin/vc-ranktable-aggregator`.

Image packaging (example, since repository currently has no dedicated Dockerfile for this binary):

```dockerfile
FROM golang:1.25 AS builder
WORKDIR /src
COPY . .
RUN make vc-ranktable-aggregator

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /src/_output/bin/vc-ranktable-aggregator /vc-ranktable-aggregator
ENTRYPOINT ["/vc-ranktable-aggregator"]
```

```bash
# Example image build/push
docker build -f Dockerfile.ranktable-aggregator -t <registry>/vc-ranktable-aggregator:<tag> .
docker push <registry>/vc-ranktable-aggregator:<tag>
```

---

## Command-line usage

Log verbosity uses klog (e.g. `-v=4`).

| Flag | Default | Description |
|------|---------|-------------|
| `-index-file-path` | `/etc/ranktable/index/index.yaml` | Path to the mounted index (file name should match your volume `items` key, often `index.yaml` or full ConfigMap YAML). |
| `-output-path` | `/etc/ranktable/jobstart_hccl.json` | Decompressed RankTable output path (shared `emptyDir` or volume with workload). |
| `-kubeconfig` | (empty) | Kubeconfig path; empty uses in-cluster config when `-master` is also empty. |
| `-master` | | Apiserver override (optional). |
| `-workers` | `4` | Max concurrent shard `GET` workers. |
| `-kube-api-qps` | `3` | REST client QPS; burst is `2 * workers`. |
| `-max-original-size` | `209715200` (200 MiB) | Reject decompressed payload larger than this (bytes). |
| `-poll-interval` | `30s` | Periodic reconcile trigger (watch loop fallback). |
| `-startup-jitter` | `30s` | Random delay in `[0, jitter]` before bootstrap/watch loop. |
| `-allow-plain-shard` | `false` | If true, shard payload may be raw bytes in the ConfigMap (not base64). **Debug/tests only.** |
| `-metrics-addr` | (empty) | If set (e.g. `:9090`), serves Prometheus metrics at `/metrics`. |
| `-exit-on-main-container-exit` | `false` | If true, sidecar exits when a local main-container exit signal file appears. |
| `-main-container-exit-file` | `/etc/ranktable/main-container.exit` | Exit signal file path on shared volume, created by main container on exit. |
| `-pod-poll-interval` | `2s` | Poll interval for checking the local exit signal file when exit-on-main-container-exit is enabled. |

Decompression is **size-capped** during decode using `min(original_size, max_original_size, -max-original-size)` so gzip/zstd cannot expand past the configured bound before validation.

Prometheus (when `-metrics-addr` is set; counters/histogram also register on the default registry):

- `volcano_ranktable_aggregator_reconcile_total{result="success|failure|skipped"}`
- `volcano_ranktable_aggregator_reconcile_duration_seconds`
- `volcano_ranktable_aggregator_shard_fetch_total{result="success|failure"}`

**Tests:** `go test ./pkg/controllers/ranktable/aggregator/...`

Example:

```bash
_output/bin/vc-ranktable-aggregator \
  -index-file-path=/etc/ranktable/index/index.yaml \
  -output-path=/shared/ranktable.json \
  -kube-api-qps=3 -workers=4
```

---

## Deployment (Kubernetes)

1. **Volume:** mount only the index ConfigMap (e.g. `items` -> `index.yaml`).
2. **Shared volume:** `emptyDir` for assembled output; aggregator sidecar + workload mount the same path.
3. **ServiceAccount/RBAC:** grant `get` on `configmaps` in job namespace.
4. **Native Sidecar:** one `initContainers` entry with `restartPolicy: Always` (Kubernetes 1.28+; default-on from 1.29). It runs **with** the workload, bootstraps when the output file is absent, then watches the index; on Pod shutdown the kubelet stops sidecars in order (SIGTERM).
5. **Optional:** `--exit-on-main-container-exit` + shared exit file if you need explicit in-container exit coupling.

Regular **`containers`** list: workload only (do **not** duplicate the aggregator as a normal container when using Native Sidecar).

Minimal RBAC example:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ranktable-aggregator
  namespace: <job-namespace>
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ranktable-aggregator
  namespace: <job-namespace>
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ranktable-aggregator
  namespace: <job-namespace>
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ranktable-aggregator
subjects:
  - kind: ServiceAccount
    name: ranktable-aggregator
    namespace: <job-namespace>
```

Pod snippet (**Native Sidecar** only + workload):

```yaml
volumes:
  - name: ranktable-index
    configMap:
      name: rt-index-job-001
      items:
        - key: index.yaml
          path: index.yaml
  - name: ranktable-shared
    emptyDir: {}

initContainers:
  - name: ranktable-aggregator
    restartPolicy: Always
    image: <registry>/vc-ranktable-aggregator:<tag>
    args:
      - -index-file-path=/etc/ranktable/index/index.yaml
      - -output-path=/etc/ranktable/jobstart_hccl.json
      - -kube-api-qps=3
      - -workers=4
      - -poll-interval=30s
      - -startup-jitter=30s
      - -metrics-addr=:9090
      - -exit-on-main-container-exit=true
      - -main-container-exit-file=/etc/ranktable/main-container.exit
    volumeMounts:
      - name: ranktable-index
        mountPath: /etc/ranktable/index
      - name: ranktable-shared
        mountPath: /etc/ranktable

containers:
  - name: workload
    image: <workload-image>
    command: ["sh", "-c", "trap 'touch /etc/ranktable/main-container.exit' EXIT; exec /your-workload-command"]
    volumeMounts:
      - name: ranktable-shared
        mountPath: /etc/ranktable
```

---


## E2E Coverage (Recommended)

Yes, this should be covered by e2e. Suggested suite location: `test/e2e/ranktable/`.

The e2e Pod specs use **Native Sidecar** (`initContainers` entry with `restartPolicy: Always`). The test cluster must run Kubernetes **1.28+** with the Sidecar Containers feature available (default-on from 1.29).

Recommended scenarios:

1. **bootstrap success**: publish `V1` index+shards, Pod starts, native sidecar writes output file, workload reads expected content (workload may need to retry until file exists).
2. **sidecar refresh**: update to `V2`, sidecar detects index update and refreshes local file atomically.
3. **corrupted shard**: one shard hash mismatch, sidecar must keep old file and report failure metric/log.
4. **invalid changed_shards**: set malformed JSON in index, reconcile fails loudly and does not switch file.
5. **partial publish**: index `status=initializing`, consumer must not switch to new version.
6. **incremental reuse**: `prev_version` matches and `changed_shards` lists subset, verify only changed shards are fetched.

Execution pattern (example):

```bash
# unit/integration for package
GOFLAGS=-mod=mod go test ./pkg/controllers/ranktable/aggregator/...

# e2e suite (requires aggregator image in cluster)
export ENABLE_RANKTABLE_E2E=true
export RANKTABLE_AGGREGATOR_IMAGE=<registry>/vc-ranktable-aggregator:<tag>
# optional if image entrypoint differs:
# export RANKTABLE_AGGREGATOR_CMD=/vc-ranktable-aggregator
go test ./test/e2e/ranktable -run TestE2E -v
```

## Package layout

| File | Role |
|------|------|
| `doc.go` | Package docstring. |
| `types.go` / `index.go` | Index metadata and parsing. |
| `shard_fetch.go` | Shard ConfigMap GET (retries on transient API errors). |
| `hash.go` | SHA-256 and hash comparison helpers. |
| `decompress.go` | Compute decompression limits and decode/decompress with bounds. |
| `atomic_write.go` | Atomic file write (`tmp + fsync + rename`). |
| `metrics.go` | Prometheus counters/histogram. |
| `reconciler.go` | Fetch plan, cache/reuse, validate, write. |
| `sidecar.go` | Bootstrap when output missing; fsnotify + poll + background reconciler. |

---

## 中文说明（概要）

**目标：** 单机 RankTable 超过 ConfigMap 上限时，先**压缩再分片**存多个 ConfigMap；Pod 内只挂 **index**，分片通过 apiserver **按名 GET**，聚合校验后写入**本地单文件**，业务只读该文件；**不依赖共享存储**。

**发布顺序：** 先写齐某版本 `V` 的全部分片 ConfigMap，最后再把 index 设为 `status=completed` 且 `ranktable_cur_version=V`。消费者仅信任 `completed`。

**单一运行路径：** 输出文件不存在时先做 **bootstrap 聚合**（可选抖动），再监听 index 变更并定时兜底轮询。**部署上使用 Native Sidecar**（唯一一个带 `restartPolicy: Always` 的 init 容器），与业务容器并行启动，由 kubelet 在 Pod 退出时结束 sidecar；业务宜对缺失文件做重试。详见上文 Deployment。

**构建：** 仓库根目录执行 `make vc-ranktable-aggregator`，产物在 `_output/bin/vc-ranktable-aggregator`。

**关键参数：** `-index-file-path`、`-output-path`、`-kube-api-qps` / `-workers`、`-startup-jitter`、`-poll-interval`；`-metrics-addr` 开启 Prometheus；`-allow-plain-shard` 仅调试用（生产应用 base64 分片）。**解压过程**受 `original_size` / `max_original_size` / `-max-original-size` 的最小值约束，减轻恶意压缩。**`changed_shards`** 非空且 JSON 非法会直接报错，避免误增量。**分片 GET** 对可重试错误带退避重试。

更完整的中文设计叙述见：[ranktable-sharded-distribution.zh.md](../../../../docs/design/ranktable-sharded-distribution.zh.md)。
