# RankTable 分片分发设计

[English version](./ranktable-sharded-distribution.md)

## 摘要

本文档定义了 Volcano 在单个作业 RankTable 超过 Kubernetes ConfigMap 大小限制时的分发方案。核心思路：

- 先压缩后分片存储到 ConfigMap，
- 使用一个 index ConfigMap 作为控制面信号，
- 在每个 Pod 内通过 initContainer + sidecar 完成本地聚合与刷新。

本方案假设 **每个 Pod 独立从 kube-apiserver 拉取分片**，不依赖共享存储。

参考消费者实现位于 **`pkg/controllers/ranktable/aggregator`**（库 + `vc-ranktable-aggregator` 二进制）。命令行参数、指标名与运维说明见 [`pkg/controllers/ranktable/aggregator/README.md`](../../pkg/controllers/ranktable/aggregator/README.md)。

## 背景与动机

AI/HPC 大作业的 RankTable 可能明显超过 1 MiB，无法安全放入单个 ConfigMap。需要满足：

1. 大规模集群（可达 1 万节点）可运行；
2. 支持 RankTable 频繁更新；
3. 避免把大量分片文件直接挂载进 Pod；
4. 提供严格完整性校验与原子更新，供业务容器稳定读取。

## 目标

- 支持大 RankTable 分片存储（单片目标约 `800 KiB`）。
- 通过压缩（默认 `zstd`）降低传输体积。
- 在 Pod 内生成本地 RankTable 文件供业务容器消费。
- 仅通过 index 变化触发更新。
- 通过限流、去重、重试退避控制 apiserver 压力。

## 非目标

- 不依赖共享 PVC 或分布式缓存。
- 不考虑 pre-release 旧协议兼容逻辑。
- 第一阶段不引入 CRD（仅基于 ConfigMap 协议）。

## 架构

### 组件角色

1. **Producer（控制器侧）**  
   生成完整 RankTable -> 压缩 -> 分片 -> 写入 shard ConfigMap -> 最后写 index ConfigMap（完成信号）。

2. **initContainer（启动路径）**  
   读取挂载的 index，按需拉分片并校验，聚合解压后原子写本地文件。

3. **sidecar（运行时更新路径）**  
   监听挂载 index 文件变化，触发本地 RankTable 重建与切换。

4. **业务容器**  
   仅读取 Pod 本地目标文件，不感知分片细节。

### 数据流

1. 控制器先发布全部 shard ConfigMap；
2. 控制器最后更新 index 为 `status=completed`；
3. init/sidecar 检测到 index 后，从 apiserver 拉取 shard；
4. 在 Pod 内生成并原子替换目标文件。

## ConfigMap 协议

示例文件：

- `pkg/controllers/ranktable/ranktable-index-configmap.yaml`
- `pkg/controllers/ranktable/ranktable-shard-configmap.yaml`
- `pkg/controllers/ranktable/ranktable.yaml`

### Index ConfigMap（权威元数据）

`data` 中关键字段：

- `ranktable_cur_version`：当前版本（单调递增）
- `ranktable_prev_version`：上一完整版本（首版可空）
- `status`：`initializing | completed`
- `protocol_version`：`v1.0`
- `encoding`：`zstd | gzip | identity`
- `chunk_size`：目标分片大小
- `total_shards`
- `compressed_size`
- `original_size`
- `compressed_sha256`
- `content_sha256`
- `max_original_size`（可选；若设置须 ≥ `original_size`，与消费者解压上限一致）
- `selector`（可选，发现辅助）
- `changed_shards`（可选增量提示，见下文）
- `shards`：分片元数据清单（JSON）

**解析规则（参考实现）：**

- 挂载为完整 Kubernetes 对象时，必须是 **`ConfigMap`**（`kind: ConfigMap`）。`data` 中数值字段须为合法十进制整数字符串，解析失败则本次 reconcile 失败。
- `shards` 条目数须等于 `total_shards`，**`id` 不得重复**；每条须含非空 `namespace` 与 `name`。

**`changed_shards` 语义：**

- 缺省或空字符串：不携带「变更分片 id」列表；是否复用已缓存分片由 `ranktable_prev_version` 与本地已应用版本比对决定（见参考实现）。
- 非空：必须是 **合法 JSON 数组**（分片 id）。**JSON 非法视为硬错误**，禁止静默当成「无变更」，以免错误复用旧分片。

每个分片元数据项：

- `id`
- `namespace`
- `name`
- `size`
- `sha256`

### Shard ConfigMap

要求：

- label 包含：job-id、`ranktable-type=shard`、version、shard-index
- `data.ranktable_shard_info`：分片字节的 **标准 base64**（字符串）；解码前可剥离换行。**生产环境必须合法 base64**；仅调试可通过开关（如 `--allow-plain-shard`）接受明文负载，**生产勿用**。

## 版本发布顺序

Producer 必须遵循：

1. 先写全量 shard（版本 V）；
2. 校验 shard 完整性；
3. 最后更新 index：`ranktable_cur_version=V` 且 `status=completed`。

消费者只在 `status=completed` 时切换版本。

## Pod 侧聚合算法

### 触发模型

- 仅挂载 index ConfigMap；
- sidecar 监听 index 文件变化（`fsnotify`）+ 周期兜底检查；
- shard 不挂载，按 index 清单通过 apiserver 获取。

### initContainer（启动阶段）

1. 读取并校验 index 协议；
2. 必须 `status=completed`；
3. 生成分片拉取计划；
4. 并发拉取（有上限）；
5. 校验每片 `size` / `sha256`；
6. 按 `id` 顺序拼接；
7. 校验压缩流 `compressed_sha256` / `compressed_size`；
8. 按 `encoding` 解压，解码过程使用 **输出上限**  
   `min(original_size, max_original_size（若设）, 运行时参数上限)`，以流式方式限制膨胀，缓解压缩炸弹（须在完整输出物化前生效，而非仅事后比对长度）；
9. 校验最终内容 `content_sha256` / `original_size`（含上限）；
10. 原子写文件（tmp + fsync + rename）。

### sidecar（运行时）

沿用 init 同一条流水线，并增加：

- 版本未变化直接跳过；
- 同一时刻只允许一个 reconcile 在跑；
- 失败保留旧文件；下一次由定时轮询或新的 index 事件触发。**分片 ConfigMap 的 GET** 对可重试错误采用 **指数退避 + 抖动**（见 `aggregator` 实现）。

## 大规模场景下的 apiserver 压力控制

在“每 Pod 独立拉取”约束下，必须做：

1. **禁止 shard list/watch**：按 index manifest 精准 `GET`；
2. **小并发拉取**：每 Pod 2~4 worker；
3. **client-go 限流**：例如 `QPS=2~5`、`Burst=4~10`；
4. **抖动削峰**：启动/更新触发增加随机延迟；
5. **事件防抖**：合并短时间内重复 index 变更；
6. **指数退避重试**：对分片 **GET** 在限流、超时、连接重置、5xx 等可重试错误上退避（参考实现已做）；
7. **版本幂等**：同版本不重复重建；
8. **增量优化**：在 `changed_shards` 为合法 JSON 且与 `ranktable_prev_version` 匹配时，可跳过已缓存的未变更分片。

## 完整性与安全

切换文件前必须完成：

- 分片级校验（`size` + `sha256`）；
- 压缩流级校验（`compressed_*`）；
- 解压内容级校验（`content_*`）。

同时要求：

- 拒绝未知 `encoding`；
- 拒绝不支持的 `protocol_version`；
- **解码过程中**即限制解压输出上限（防压缩炸弹），并与 `original_size` / `max_original_size` / 运行时上限取最小值；
- 拒绝清单不一致（如 `original_size` > `max_original_size`、分片 id 重复、`namespace`/`name` 为空）；
- 始终原子替换；
- 失败不删除上一个可用版本文件。

## 异常处理

- **分片缺失**：退避重试，保留旧文件；
- **哈希不匹配**：判定数据损坏，不切换；
- **index 未完成**：`status=initializing` 时等待；
- **apiserver 限流/错误**：分片拉取有上限重试 + 抖动，不空转。
- **`changed_shards` 非空但 JSON 非法**：reconcile 失败，需 Producer 修正 index。

## 可观测性

**参考二进制已实现**（`--metrics-addr`，Prometheus 默认 registry）：

- `volcano_ranktable_aggregator_reconcile_total{result="success|failure|skipped"}`
- `volcano_ranktable_aggregator_reconcile_duration_seconds`
- `volcano_ranktable_aggregator_shard_fetch_total{result="success|failure"}`

可选/后续指标：

- `ranktable_shard_fetch_inflight`
- `ranktable_current_version`（gauge）
- `ranktable_bytes_downloaded_total`

建议日志字段：

- `job_id`, `namespace`, `cur_version`, `prev_version`, `total_shards`
- `changed_shards_count`, `attempt`, `latency_ms`, `error`

## RBAC 与部署要求

Pod 内 init/sidecar 的 ServiceAccount 需要在作业 namespace 内读取 ConfigMap 的权限：

- `get`（必须）
- `list/watch`（仅当启用发现兜底时）

建议最小权限原则，按 namespace + label 约束访问范围。

参考消费者编译/部署建议：

- 编译：`make vc-ranktable-aggregator`（或 `go build ./cmd/ranktable-aggregator`）。
- 镜像：由源码二进制打包，同一镜像同时用于 initContainer 与 sidecar。
- 运行参数按集群调优：`--workers`、`--kube-api-qps`、`--poll-interval`、`--startup-jitter`。
- 通过 `--metrics-addr` 暴露 `/metrics` 供 Prometheus 抓取。

## 测试计划

1. **单元测试**（`pkg/controllers/ranktable/aggregator/*_test.go`）
   - index 解析（`kind: ConfigMap` 路径、数值字段错误、`changed_shards` 错误）；
   - 字段校验（重复分片 id、namespace/name）；
   - 有界压缩/解压；
   - 原子写；
   - fake client 下单分片完整 reconcile。

2. **集成测试**
   - 发布 V1 -> V2，验证 sidecar 热更新；
   - 分片损坏时不替换旧文件；
   - `initializing` 阶段不切换。

3. **规模压测**
   - N Pod × M 分片，验证 apiserver QPS 在预算内；
   - 同步更新场景验证抖动与防抖有效。

4. **E2E 用例**（建议新增 suite：`test/e2e/ranktable/`）
   - 启动成功：发布 V1 后 init 生成本地文件，业务容器可读取；
   - 运行时刷新：V1 -> V2，sidecar 触发原子更新；
   - 分片损坏：hash 不匹配时保留旧文件并打失败指标/日志；
   - `changed_shards` 非法：JSON 解析失败，reconcile 失败且不切版本；
   - 部分发布：`status=initializing` 不允许切换输出；
   - 增量复用：`prev_version` 匹配且仅部分分片变化，只拉取变更分片。

suite 落地后示例执行：`go test ./test/e2e/ranktable -run TestE2E -v`。

## 后续演进

- shard 内容迁移到对象存储（index 仍在 ConfigMap）；
- 强化 `changed_shards` 生成准确性；
- 如元数据持续扩展，可评估 CRD 化。
