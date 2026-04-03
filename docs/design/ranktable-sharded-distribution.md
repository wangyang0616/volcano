# RankTable Sharded Distribution Design

[中文版 (Chinese)](./ranktable-sharded-distribution.zh.md)

## Summary

This document defines how Volcano distributes large per-job RankTable payloads that exceed the Kubernetes ConfigMap size limit. The solution uses:

- compressed-then-sharded RankTable storage in ConfigMaps,
- one index ConfigMap as the control plane signal,
- a single **Native Sidecar** process (one `initContainers` entry with `restartPolicy: Always`) in each Pod: **bootstrap** when the output file is absent, then watch the index and refresh; kubelet manages shutdown ordering with app containers.

The design assumes **each Pod independently pulls shard data from kube-apiserver** (no shared storage dependency).

A reference consumer implementation lives in **`pkg/controllers/ranktable/aggregator`** (library + `vc-ranktable-aggregator` binary). Operational notes, flags, and metric names are summarized in [`pkg/controllers/ranktable/aggregator/README.md`](../../pkg/controllers/ranktable/aggregator/README.md).

## Motivation

Large AI/HPC jobs can generate RankTable files significantly larger than 1 MiB. A single ConfigMap cannot hold the full content safely. We need a solution that:

1. scales to very large clusters (up to 10k nodes),
2. supports frequent RankTable updates,
3. avoids mounting hundreds of shard files into Pods,
4. provides strict integrity checks and atomic file refresh for consumers.

## Goals

- Store large RankTable content with sharding (`~800 KiB` per shard target).
- Reduce transfer volume via compression (`zstd` by default).
- Assemble RankTable inside Pod as a local file for business container consumption.
- Trigger refresh by index file change only.
- Bound kube-apiserver pressure with strict throttling and deduplicated reconcile behavior.

## Non-goals

- No shared PVC / distributed cache dependency.
- No backward compatibility with a pre-release protocol.
- No custom CRD required in the first stage (ConfigMap-based protocol only).

## Architecture

### Components

1. **Producer (controller side)**  
   Generates full RankTable -> compresses -> shards -> writes shard ConfigMaps -> writes index ConfigMap as final signal.

2. **Aggregator / Native Sidecar**  
   After optional startup jitter: if the local RankTable file is **missing**, runs a synchronous bootstrap reconcile; then starts the long-running loop (background reconcile + `fsnotify` on the index + periodic poll). Skips work only when index version matches in-memory state **and** the output file still exists (recreate if the file was deleted).

3. **business container**  
   Reads only the local file; should **retry** until the file exists (aggregator starts concurrently with app containers).

### Data flow

1. Controller publishes shard ConfigMaps first.
2. Controller publishes index ConfigMap with `status=completed`.
3. Aggregator sees index update (or bootstrap on empty output), then fetches shards via apiserver `GET` calls.
4. Aggregated file is atomically written to shared in-Pod volume.

## ConfigMap Protocol

Examples are under:

- `pkg/controllers/ranktable/ranktable-index-configmap.yaml`
- `pkg/controllers/ranktable/ranktable-shard-configmap.yaml`
- `pkg/controllers/ranktable/ranktable.yaml`

### Index ConfigMap (authoritative metadata)

Required fields in `data`:

- `ranktable_cur_version`: current version (monotonic)
- `ranktable_prev_version`: previous complete version (optional for first publish)
- `status`: `initializing | completed`
- `protocol_version`: `v1.0`
- `encoding`: `zstd | gzip | identity`
- `chunk_size`: shard target size (bytes)
- `total_shards`
- `compressed_size`
- `original_size`
- `compressed_sha256`
- `content_sha256`
- `max_original_size` (optional consumer guard; must be ≥ `original_size` when set)
- `selector` (optional discovery helper)
- `changed_shards` (optional optimization hint; see below)
- `shards`: JSON array with per-shard metadata

**Parsing rules (reference consumer):**

- When the index is mounted as a full Kubernetes object, it MUST be a **`ConfigMap`** (`kind: ConfigMap`). Numeric fields in `data` MUST be valid decimal strings (`strconv`); parse failures abort reconcile.
- `shards` MUST contain exactly `total_shards` entries with **unique** `id` values; each entry MUST include non-empty `namespace` and `name`.

**`changed_shards` semantics:**

- Empty or absent: no explicit “changed shard id” list; incremental reuse follows `ranktable_prev_version` vs last locally applied version as implemented.
- Non-empty: MUST be a valid JSON array of shard ids. **Invalid JSON is a hard error** (reconcile fails) so a broken incremental hint is never silently treated as “no changes”.

Per-shard metadata entry:

- `id`
- `namespace`
- `name`
- `size`
- `sha256`

### Shard ConfigMap

Required:

- labels: job id, type=`shard`, version, shard index
- `data.ranktable_shard_info`: **standard base64** (UTF-8 string) of the shard byte payload (compressed chunk). Newlines in the string are stripped before decode. **Production consumers require valid base64**; a debug-only flag (e.g. `--allow-plain-shard`) may allow raw bytes for local testing—must not be used in production.

## Version and publish ordering

Publisher must follow:

1. Create/update all shard ConfigMaps for version `V`.
2. Verify shard count/hash readiness.
3. Update index ConfigMap to `ranktable_cur_version=V` and `status=completed`.

`status=completed` is the only signal consumers trust for switching.

## Pod-side Reconcile Algorithm

### Trigger model

- Mount only index ConfigMap into Pod.
- sidecar listens file changes (`fsnotify`) and also runs periodic check (fallback).
- shard files are never mounted.

### Reconcile steps (bootstrap and refresh share one pipeline)

1. Load and validate index schema.
2. Ensure `status=completed` (otherwise error; watch loop retries).
3. If in-memory version equals index version **and** output file exists, skip.
4. Build shard fetch plan.
5. Download shards (bounded worker pool).
6. Validate each shard (`size`, `sha256`).
7. Concatenate by `id`.
8. Validate merged compressed stream (`compressed_sha256`, `compressed_size`).
9. Decompress based on `encoding` using a **streaming decode with a hard output cap** of  
   `min(original_size, max_original_size if set, runtime max-bytes flag)` (mitigates “zip bombs”).
10. Validate final content (`content_sha256`, `original_size`, optional `max_original_size`).
11. Atomic write (`tmp + fsync + rename`) to target file.

**Runtime loop:** single in-flight reconcile (coalesced triggers), `fsnotify` on index directory + poll ticker; on failure keep previous valid local file; shard `GET` uses bounded backoff on transient API errors.

## Scaling and kube-apiserver pressure control

Given worst case (200 shards/job, many Pods), Pod-side consumers must enforce:

1. **No shard list/watch**: use precise `GET ns/name` from index manifest.
2. **Small bounded concurrency**: 2-4 shard workers per Pod.
3. **Client throttling**: per sidecar client-go QPS/Burst, e.g. `QPS=2~5`, `Burst=4~10`.
4. **Jitter**: random startup and update delay to avoid synchronized burst.
5. **Debounce**: coalesce frequent index file events.
6. **Backoff retry**: exponential + jitter on **shard `GET`** for retriable errors (throttling, timeouts, connection resets, 5xx)—see reference `aggregator` package.
7. **Version idempotency**: skip duplicate work when version matches in memory **and** the output file still exists; recreate if the file is missing.
8. **Optional incremental hint**: use `changed_shards` (valid JSON only) plus matching `ranktable_prev_version` to avoid re-downloading unchanged shards already cached in memory.

## Integrity and safety

Mandatory checks before file switch:

- shard-level hash/size verification,
- merged compressed stream hash/size verification,
- decompressed content hash/size verification.

Security and robustness:

- reject unknown `encoding`,
- reject unsupported `protocol_version`,
- enforce decompressed size upper bound **during decode** (not only after full materialization),
- reject manifest inconsistencies (`original_size` > `max_original_size`, duplicate shard ids, empty shard `namespace`/`name`),
- always write atomically,
- never delete last known-good file on reconcile failure.

## Failure handling

- **Missing shard**: per-`GET` retries with backoff; after failure, keep old file.
- **Hash mismatch**: treat as data corruption, do not switch.
- **Index points to incomplete publish**: wait until `status=completed`.
- **apiserver throttle/error**: bounded retries and jitter on shard fetch; no busy loop.
- **Invalid `changed_shards` JSON** (when non-empty): fail reconcile loudly; producer must fix the index.

## Observability

**Implemented in reference binary** (`--metrics-addr`, Prometheus default registry):

- `volcano_ranktable_aggregator_reconcile_total{result="success|failure|skipped"}`
- `volcano_ranktable_aggregator_reconcile_duration_seconds`
- `volcano_ranktable_aggregator_shard_fetch_total{result="success|failure"}`

Additional metrics (optional / future):

- `ranktable_shard_fetch_inflight`
- `ranktable_current_version` (gauge)
- `ranktable_bytes_downloaded_total`

Recommended structured log fields:

- `job_id`, `namespace`, `cur_version`, `prev_version`, `total_shards`,
- `changed_shards_count`, `attempt`, `latency_ms`, `error`.

## Deployment and RBAC

Per-Pod init/sidecar ServiceAccount requires ConfigMap read permissions in the job namespace:

- `get` on index/shard ConfigMaps,
- optionally `list/watch` only if discovery fallback is needed.

Prefer least privilege Role/RoleBinding scoped to namespace and label conventions.

Build/deploy notes for the reference consumer:

- Build: `make vc-ranktable-aggregator` (or `go build ./cmd/ranktable-aggregator`).
- Container image: build from source binary and deploy as a **single Native Sidecar** (`initContainers` + `restartPolicy: Always`, Kubernetes 1.28+; default-on from 1.29). The kubelet runs it with app containers and **terminates it during Pod shutdown** (SIGTERM ordering). On older clusters, fall back to the same binary under regular `containers` (no kube-managed sidecar ordering).
- `--exit-on-main-container-exit` plus a local signal file remains optional for explicit in-container exit coupling.
- Runtime flags usually tuned per cluster: `--workers`, `--kube-api-qps`, `--poll-interval`, `--startup-jitter`.
- Expose metrics with `--metrics-addr` and scrape `/metrics`.

## Test plan

1. **Unit tests** (see `pkg/controllers/ranktable/aggregator/*_test.go`)
   - index parsing (`kind: ConfigMap` path, numeric field errors, `changed_shards` errors),
   - schema validation (duplicate shard ids, namespace/name),
   - bounded compression/decompression paths,
   - atomic write behavior,
   - single-shard reconcile with fake kube client.

2. **Integration tests**
   - publish V1 then V2, verify sidecar refresh,
   - corrupted shard should not replace local file,
   - partial publish with `initializing` should not switch.

3. **Scale tests**
   - N Pods x M shards, verify apiserver request rate within budget,
   - synchronized index update with jitter/debounce enabled.

4. **E2E tests** (suite: `test/e2e/ranktable/`) — Pods use **Native Sidecar** (`restartPolicy: Always` init); cluster should be Kubernetes **1.28+** (sidecar feature on).
   - bootstrap success: V1 publish -> init writes local file -> workload reads expected payload,
   - sidecar refresh: V1 -> V2 switch updates local file atomically,
   - corrupted shard: hash mismatch keeps old file and reports failure metric/log,
   - invalid `changed_shards`: malformed JSON fails reconcile and blocks version switch,
   - partial publish: `status=initializing` must not switch output,
   - incremental reuse: matching `prev_version` + subset `changed_shards` only refetches changed shards.

Example run command once suite exists: `go test ./test/e2e/ranktable -run TestE2E -v`.

## Future enhancements

- Replace shard payload storage with object storage URLs (index still in ConfigMap).
- Add producer-side `changed_shards` accuracy guarantees.
- Consider CRD-based protocol if metadata complexity grows.
