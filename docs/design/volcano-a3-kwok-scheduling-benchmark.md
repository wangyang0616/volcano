# Volcano A3 超节点持续调度与 Repack 资源效率验证方案

## 1. 文档目的

本文描述一套基于现有 Kubernetes 集群、Volcano 和 KWOK 的持续调度验证方案。方案使用 240 个 KWOK 节点模拟 5 个昇腾 A3 超节点，持续回放一周或一个月的训练、推理混部业务曲线，对比以下两种策略：

- 基线方案：关闭 Binpack 和 Repack；
- 实验方案：开启 Binpack，并根据碎片率和超节点容量状态自动触发 Repack。

验证重点不是某次 Repack 前后的瞬时提升，而是在任务持续提交、持续结束并且一直存在 Pending 资源需求的情况下，观察整个业务周期的 NPU 资源分配率曲线。

目标是在本文限定的负载、拓扑和可迁移范围内，验证实验方案相对基线方案的长期时间加权 NPU 调度分配率能否提升 20% 以上，同时降低大规格训练任务排队时间，并将 Repack 的迁移影响控制在预设范围内。

工程同时面向后续生产轨迹复用：当生产环境积累任务投递、启动、完成、扩缩容和拓扑约束历史后，可以将其脱敏并转换为标准 Trace，在本地 KWOK 环境中按需筛选、加速回放和比较不同 Volcano 调度策略，而不需要为每次验证重新编写测试脚本。

## 2. 背景

AI 集群通常同时运行多种资源形态的工作负载：

- 整节点、整机架或整超节点的大模型训练；
- 中小规模微调和评测任务；
- 单卡、双卡或四卡推理服务；
- 持续扩缩容和滚动升级的在线业务；
- 运行时间差异较大的离线推理任务。

任务不断提交、结束、失败重试或扩缩容，会持续改变集群中的资源布局。即使集群总空闲 NPU 数量较多，也可能因为空闲卡分散在不同节点、机架或超节点中，无法满足大规格训练任务的拓扑要求。

典型场景如下：

```text
集群总空闲 NPU 超过 384 卡
        ↓
空闲卡分散在 5 个超节点
        ↓
没有任何一个超节点具备 48 个完整空闲节点
        ↓
384 卡整超节点训练任务持续 Pending
        ↓
账面空闲资源充足，但有效可调度容量不足
```

Binpack 可以在新任务调度时优先填充已占用节点，减少新碎片产生，但无法主动调整已经运行的 Pod，也无法完全消除任务结束、扩缩容和滚动升级带来的运行时碎片。

Repack 用于补充这一能力。当碎片达到一定程度时，Repack 迁移允许中断的小规格工作负载，将零散占用集中到部分节点，释放完整节点或超节点容量。

本次验证关注以下闭环：

```text
Binpack 减缓新碎片产生
        +
Repack 周期性整理运行时碎片
        ↓
形成满足拓扑要求的完整超节点容量
        ↓
持续排队的大规格任务消费释放容量
        ↓
长期 NPU 分配率和任务吞吐提高
```

## 3. 验证目标与边界

### 3.1 核心目标

本方案验证以下假设：

1. 训练与推理混部、任务持续进出时，会形成明显的节点和超节点碎片。
2. 开启 Binpack 后，新任务分布更加紧凑。
3. 当碎片率超过阈值时，Repack 能够迁移允许移动的工作负载。
4. Repack 能够将分散空闲资源转换为满足大规格训练要求的完整超节点容量。
5. 排队中的 384 卡训练任务能够及时占用释放出的超节点。
6. 在完整的一周或一个月业务周期内，实验方案的时间加权 NPU 分配率相对基线提升 20% 以上。
7. 分配率提升能够同时体现为任务吞吐提高和排队时间下降，而不是仅表现为 Pod 占用资源。
8. Repack 不会迁移受保护业务，被迁移工作负载的替身 Pod 能够正常恢复。

### 3.2 验证边界

KWOK 负责模拟 Kubernetes Node 和 Pod 生命周期。本方案可以验证：

- Volcano 对 NPU 扩展资源的调度；
- Gang 准入、Binpack 节点选择和拓扑约束；
- 训练与推理混部形成的资源碎片；
- Repack 规划、Eviction 和替身 Pod 重新调度；
- 完整节点或超节点释放；
- Kubernetes/Volcano 视角的长期 NPU 调度分配率；
- 240 节点规模下的调度器、控制器和 API Server 吞吐。

本方案不验证：

- 真实 Ascend Device Plugin 行为；
- NPU 驱动、固件或硬件故障；
- HCCL 通信和真实网络性能；
- AICore 实际利用率；
- 真实模型训练吞吐、加载时间和推理时延；
- vNPU 的显存、Core 等多维资源组合。

因此，本文中的“NPU 分配率”特指 Kubernetes 和 Volcano 视角的 NPU 扩展资源调度分配率，不等同于真实 NPU 计算利用率。

第一阶段只验证整卡标量扩展资源。实际 ResourceName 应与目标生产环境保持一致，以下统一记为 `R_NPU`，例如 `huawei.com/Ascend910`。

## 4. 模拟集群设计

### 4.1 A3 拓扑

使用 240 个 KWOK 节点模拟 5 个 A3 超节点：

```text
集群
└── 5 个 A3 超节点
    └── 每个超节点 8 个机架
        └── 每个机架 6 个节点
            └── 每个节点 8 张 NPU
```

整体容量如下：

| 层级 | 数量 | 单位容量 | 总容量 |
| --- | ---: | ---: | ---: |
| 集群 | 1 | 5 个超节点 | 1920 卡 |
| 超节点 | 5 | 48 个节点 | 384 卡 |
| 机架 | 40 | 6 个节点 | 48 卡 |
| 节点 | 240 | 8 卡 | 1920 卡 |

### 4.2 节点编号

```text
supernode-00：node-000 ～ node-047
supernode-01：node-048 ～ node-095
supernode-02：node-096 ～ node-143
supernode-03：node-144 ～ node-191
supernode-04：node-192 ～ node-239
```

每个超节点内部按 6 个节点划分为一个机架。

### 4.3 节点资源与标签

每个 KWOK Node 至少包含以下信息：

```yaml
apiVersion: v1
kind: Node
metadata:
  name: node-000
  annotations:
    kwok.x-k8s.io/node: fake
  labels:
    kwok.x-k8s.io/node: fake
    accelerator-pool: ascend-a3-kwok
    accelerator-type: Ascend-A3
    benchmark.volcano.sh/supernode: supernode-00
    benchmark.volcano.sh/rack: supernode-00-rack-00
spec:
  taints:
    - key: kwok.x-k8s.io/node
      value: fake
      effect: NoSchedule
status:
  capacity:
    cpu: "192"
    memory: 1024Gi
    pods: "110"
    huawei.com/Ascend910: "8"
  allocatable:
    cpu: "192"
    memory: 1024Gi
    pods: "110"
    huawei.com/Ascend910: "8"
```

所有 Benchmark 工作负载必须带有相应的 NodeSelector/Affinity 和 Toleration，防止真实业务 Pod 或系统组件被调度到 KWOK 节点。

## 5. 业务任务模型

### 5.1 任务规格

围绕节点、机架和超节点构造以下训练规格：

| 业务类型 | Worker 规格 | NPU 总量 | 拓扑要求 |
| --- | ---: | ---: | --- |
| 单节点训练 | 1 × 8 卡 | 8 卡 | 单节点 |
| 小型训练 | 2 × 8 卡 | 16 卡 | 同机架 |
| 机架训练 | 6 × 8 卡 | 48 卡 | 同机架 |
| 中型训练 | 12 × 8 卡 | 96 卡 | 同超节点 |
| 半超节点训练 | 24 × 8 卡 | 192 卡 | 同超节点 |
| 整超节点训练 | 48 × 8 卡 | 384 卡 | 同超节点 |
| 双超节点训练 | 96 × 8 卡 | 768 卡 | 两个完整超节点 |
| 推理/评测 | 每 Pod 1、2、4 卡 | 动态 | 节点、机架或超节点范围 |

### 5.2 建议任务比例

按 NPU 卡时建议采用以下初始比例：

| 工作负载 | 卡时占比 | 业务生命周期 | Repack 策略 |
| --- | ---: | --- | --- |
| 384 卡整超节点训练 | 25% | 6～24 小时 | protected |
| 96/192 卡训练 | 25% | 2～12 小时 | protected 或检查点后 eligible |
| 16/48 卡训练 | 20% | 30 分钟～6 小时 | 部分 eligible |
| 在线推理 | 20% | 长时间运行并周期扩缩容 | protected |
| 评测与离线推理 | 10% | 10 分钟～3 小时 | eligible |

### 5.3 持续碎片事件

任务轨迹持续包含：

- 大中型训练任务完成；
- 单卡、双卡推理扩缩容；
- 在线推理滚动升级；
- 离线推理和评测任务短周期进出；
- 少量任务失败重试；
- 每日固定时段的大训练提交高峰；
- 已运行 384 卡任务完成后，新的 384 卡任务继续进入队列。

### 5.4 持续排队条件

整个测量周期必须保持：

```text
总体需求负载率：集群容量的 120%～130%
Pending NPU 需求不低于 384 卡的时间比例：不低于 95%
Pending 队列中持续存在至少一个 384 卡同超节点任务
```

如果实验方案启动了一个 384 卡任务，轨迹中应继续存在后续大规格任务，避免实验组清空队列后因需求不足导致分配率下降。

Pending 超过任务自身 `queueDeadline` 后可以清理，但必须计入任务超时率和未满足需求，不能从统计中忽略。

## 6. 固定任务轨迹

正式验证前生成不可变的任务事件轨迹。每条任务至少包含：

```yaml
eventId: event-000123
submitAt: P2DT14H30M
action: Submit
workloadId: training-0031
workloadType: FullSupernodeTraining
workerReplicas: 48
npuPerWorker: 8
duration: 12h
priority: high
topology: SameSupernode
repackEligible: false
queueDeadline: 24h
```

轨迹生成后记录：

- Trace ID；
- SHA256；
- 随机种子；
- 业务周期；
- 任务和事件数量；
- 总请求 NPU 卡时；
- 各规格任务占比。

回放必须遵循以下规则：

1. 任务按固定业务时间提交，不依赖当前是否有资源。
2. Pending 期间不计算任务运行时长。
3. Gang 全部进入 Running 后开始计算服务时间。
4. 达到逻辑运行时长后结束工作负载。
5. 扩缩容、滚动升级和失败重试按固定事件发生。
6. 两种方案不允许提交轨迹之外的额外任务。
7. 基线和实验必须使用相同的 Trace Hash 和随机种子。

如果能够获得脱敏生产数据，应优先保留任务提交时间、卡数、Pod 数量、运行时间和拓扑要求之间的联合关系，而不是分别随机生成这些参数。

## 7. 对比实验设计

只设置两个实验方案：

| 方案 | Binpack | Repack |
| --- | ---: | ---: |
| 基线方案 | 关闭 | 关闭 |
| 实验方案 | 开启 | 根据碎片率和超节点容量自动触发 |

两种方案必须使用：

- 相同集群容量和节点拓扑；
- 相同 Volcano 版本；
- 相同 Queue、PriorityClass、Gang 和 Affinity 配置；
- 相同任务轨迹；
- 相同任务提交时间和业务运行时间；
- 相同随机种子。

本次只评价 Binpack 与 Repack 组合后的整体收益，不拆分两者各自贡献。

## 8. Binpack 与 Repack 策略

### 8.1 Binpack

实验方案需要提高 NPU 扩展资源的装箱权重。示例：

```yaml
- name: binpack
  arguments:
    binpack.weight: 10
    binpack.cpu: 1
    binpack.memory: 1
    binpack.resources: huawei.com/Ascend910
    binpack.resources.huawei.com/Ascend910: 10
```

基线方案关闭 Binpack 或将其权重设置为 0。两次运行之间修改 Scheduler ConfigMap 后，必须确认 Volcano Scheduler 已完成配置加载或滚动更新。

### 8.2 Repack 触发器

RepackRun 是一次性资源，因此实验方案需要一个外部控制器持续执行以下闭环：

```text
检测碎片
  ↓
识别整超节点 Pending 需求
  ↓
选择目标超节点
  ↓
创建 DryRun
  ↓
审核完整超节点是否可释放
  ↓
创建新的 Execute
  ↓
进入冷却期
```

### 8.3 碎片率

目标资源的全局碎片率定义为：

```text
              当前占用节点数 - 理论最少占用节点数
碎片率 = ------------------------------------------------
                         NPU 节点总数
```

240 个节点中：

```text
1 个额外占用节点 ≈ 0.417 个百分点
10% 碎片率 ≈ 24 个额外占用节点
20% 碎片率 ≈ 48 个额外占用节点
```

### 8.4 初始触发条件

建议初始参数：

```text
检测周期：每 5 分钟业务时间
碎片率高水位：10%
持续窗口：15 分钟业务时间
碎片率低水位：5%
Execute 冷却时间：60 分钟业务时间
```

创建 DryRun 前同时检查：

- 全局碎片率超过 10%；
- 集群总空闲 NPU 不低于 384 卡；
- 存在 384 卡同超节点 Pending 任务；
- 没有任何超节点具备 48 个完整空闲节点；
- 当前没有其他 Execute 运行；
- 当前不在 Execute 冷却期。

### 8.5 目标超节点选择

触发器需要分别计算 5 个超节点的：

- 已分配和空闲 NPU；
- 部分占用节点数；
- 完整空闲节点数；
- protected 和 eligible NPU 数量；
- 预计迁移 NPU 和 PodGroup 数量；
- 其他超节点的可接收容量。

优先选择总占用较低、碎片节点较多、protected 工作负载较少且迁移成本较低的超节点。RepackRun 通过 `scope.nodes` 将可腾空源节点限定在目标超节点。

`scope.nodes` 不限制接收节点。eligible 工作负载自身的 NodeAffinity、Taint/Toleration 等约束必须允许其调度到其他超节点。

### 8.6 Execute 门槛

DryRun 完成后，仅在以下条件同时成立时创建 Execute：

- DryRun 正常完成并推荐整理；
- 预计释放 48 个节点；
- 48 个节点全部属于同一个目标超节点；
- 释放后能够承载一个 384 卡同超节点任务；
- 所有迁移 Pod 均存在可行接收节点；
- 不包含 protected 工作负载；
- 迁移 NPU 不超过 192 卡；
- 影响 PodGroup 不超过 128 个。

最终成功标准不是全局释放了 48 个节点，而是在同一个超节点内形成 48 个完整空闲节点，并使一个 384 卡同超节点任务真正进入 Running。

## 9. 加速回放

### 9.1 时间设计

一周或一个月是业务逻辑时间，实际执行采用加速回放：

| 验证阶段 | 业务时间 | 实际执行时间 | 加速倍数 |
| --- | ---: | ---: | ---: |
| 快速验证 | 7 天 | 10 分钟 | 1008 倍 |
| 正式验证 | 30 天 | 30 分钟 | 1440 倍 |

任务提交时间和运行时长按相同比例压缩：

```text
测试提交时间 = 业务提交时间 / 加速倍数
测试运行时长 = 业务运行时长 / 加速倍数
```

以下内容不能按比例缩小：

- 单 Pod 申请的 NPU 数量；
- Worker 副本数；
- Gang `minAvailable`；
- 节点和超节点容量；
- 机架及超节点拓扑约束；
- protected 和 eligible 属性。

短于最小执行窗口的业务任务可以按时间桶聚合，但必须保持总 NPU 卡时、资源规格比例和到达波峰不变，避免大量亚秒级 Pod 使结果被 API Server 或控制器延迟主导。

### 9.2 Repack 与逻辑时钟

Repack 的真实控制面耗时无法按 1000 倍压缩。建议在创建 DryRun/Execute 后暂停业务逻辑时钟，等待本次操作完成，再继续推进回放。

Repack 的真实执行耗时、替身 Pod 恢复时间和失败情况单独记录。最终资源分配率按业务逻辑时间积分，避免将 10 秒真实控制面耗时错误放大为数小时业务时间。

如需在业务曲线中体现迁移中断，可以为不同工作负载配置逻辑迁移代价，例如训练检查点恢复时间或推理副本预热时间。

## 10. 指标与结果口径

### 10.1 主指标

瞬时 NPU 调度分配率：

```text
                   已绑定且未终止 Pod 申请的 NPU 数量
NPU 分配率(t) = -----------------------------------------
                          NPU Allocatable 数量
```

全周期时间加权分配率：

```text
周期 NPU 分配率 =
  Σ allocatedNPU(t) × logicalDuration(t)
  -------------------------------------
  Σ allocatableNPU(t) × logicalDuration(t)
```

相对提升：

```text
实验周期分配率 - 基线周期分配率
-------------------------------- × 100%
        基线周期分配率
```

报告必须同时给出相对提升和绝对百分点提升，避免“提升 20%”产生歧义。

同时记录 Running/Ready Pod 对应的有效分配率，防止长期处于重建状态的 Pod 被计为有效收益。

### 10.2 容量和拓扑指标

- 集群 Allocatable、Allocated、ReadyAllocated 和 Pending NPU；
- 全局碎片率；
- 每个超节点已分配和空闲 NPU；
- 每个超节点完整空闲节点数；
- 完整空闲超节点数；
- 当前可调度的 384 卡同超节点任务数；
- 当前可调度的 48 卡同机架任务数。

### 10.3 作业指标

- Submitted、Pending、Running、Completed、Expired 任务数；
- 任务 P50、P95、P99 排队时间；
- 每个逻辑日启动的 384 卡任务数；
- 完成的 NPU 卡时；
- 任务完成率和超时率；
- Gang 准入失败原因。

### 10.4 Repack 指标

- DryRun 和 Execute 次数；
- 目标超节点；
- 计划和实际释放节点数；
- 迁移 Pod、PodGroup 和 NPU 数量；
- Eviction 接受和拒绝数；
- 替身 Pod 恢复时间；
- Repack 前后碎片率；
- 计划节点和实际绑定节点偏差；
- 失败迁移数量。

### 10.5 控制面指标

加速回放会显著提高 API 事件密度，还需监控：

- Volcano 调度吞吐和调度周期 P95/P99；
- Pending 调度队列长度；
- API Server 请求延迟和错误率；
- etcd 写入延迟；
- Controller 工作队列深度；
- KWOK Stage 处理延迟；
- Repack 规划和执行耗时。

如果控制面持续饱和，本次运行应判定为无效，并降低加速倍数后重新执行，不能把控制面瓶颈解释为资源碎片问题。

## 11. 基于既有 Kubernetes 集群的建设内容

### 11.1 不新增 benchmarkctl

本方案不开发 `benchmarkctl`。KWOK 节点和集群资源使用现有 Helm、kubectl 和 Makefile 管理，不重复实现 KWOK 或 Kubernetes 资源管理能力。

`kwokctl scale node --replicas=240` 主要用于由 `kwokctl create cluster` 创建和管理的模拟集群。本方案复用既有 Kubernetes 集群，因此采用以下方式：

1. 在既有集群中部署 KWOK Controller 和 Stage；
2. 生成 240 个带 A3 拓扑和 NPU Capacity 的 Node 对象；
3. 使用 Helm 或 `kubectl apply` 创建这些 Node；
4. 由 KWOK Controller 维护节点 Ready 状态和 Pod 生命周期。

### 11.2 前置能力检查

现有集群需要确认：

- Volcano Scheduler、Controller 和 Admission 正常运行；
- VCJob、PodGroup 和 Queue CRD 正常；
- Gang 和 Binpack 插件可用；
- Volcano 能识别 KWOK Node 上的 NPU 扩展资源；
- RepackRun CRD、Repack Engine、Repack Controller 和相关 RBAC/Webhook 已部署；
- DryRun、Execute、Eviction 和替身 Pod 重建链路可用；
- Prometheus 能抓取 Volcano、Repack 和 Benchmark 指标。

Repack 并非所有 Volcano 版本都默认提供。如果集群中不存在 `repackruns.repack.volcano.sh`，需要先构建并部署包含 Repack 能力的 Volcano 版本。

### 11.3 KWOK 安装和节点创建

在既有集群部署 KWOK：

```bash
kubectl apply -f <kwok-release>/kwok.yaml
kubectl apply -f <kwok-release>/stage-fast.yaml
```

生产化执行应固定 KWOK 版本和清单校验值，不在正式测试中动态使用 `latest`。

240 个 A3 Node 可以通过一个小型 Helm Chart 渲染：

```text
charts/kwok-a3-nodes/
├── Chart.yaml
├── values.yaml
└── templates/
    └── nodes.yaml
```

建议参数：

```yaml
nodeCount: 240
supernodeCount: 5
nodesPerSupernode: 48
nodesPerRack: 6
npuPerNode: 8
npuResourceName: huawei.com/Ascend910
```

创建和删除节点：

```bash
helm upgrade --install kwok-a3-nodes ./charts/kwok-a3-nodes
helm uninstall kwok-a3-nodes
```

该 Chart 只负责渲染 Node API 对象，不实现任何 KWOK 逻辑。也可以预生成一个静态 `kwok-a3-nodes.yaml`，通过 `kubectl apply/delete` 管理。

### 11.4 KWOK Pod Stage

需要提供 Benchmark 专用 Stage：

1. `benchmark-pod-ready`：Pod 被绑定到 KWOK Node 后标记为 Running/Ready；
2. `benchmark-pod-delete`：处理删除和 Repack Eviction；
3. `benchmark-pod-failure`：按事件模拟失败和重试。

任务何时完成不由 KWOK 自动决定，而由工作负载回放器根据业务逻辑运行时间控制。应避免默认 Job 自动完成 Stage 与 Benchmark 生命周期发生冲突。

### 11.5 需要开发的核心组件

自研能力收敛为三个常驻组件和一个离线分析工具。

#### workload-replayer

职责：

- 读取固定任务轨迹；
- 维护加速业务逻辑时钟；
- 创建 VCJob 和推理 Deployment；
- 观察任务何时真正进入 Running；
- 按逻辑运行时长结束任务；
- 执行扩缩容、滚动升级、失败和重试事件；
- 清理超过 QueueDeadline 的任务；
- 记录任务提交、开始、完成和超时事件。

示例：

```bash
workload-replayer \
  --trace weekly-trace.jsonl \
  --strategy baseline \
  --business-duration 7d \
  --wall-duration 10m
```

#### repack-trigger

只在实验方案运行，职责包括：

- 计算全局和超节点碎片；
- 检查 384 卡 Pending 任务；
- 选择目标超节点；
- 创建并观察 DryRun；
- 校验是否能够释放同一个超节点的 48 个节点；
- 满足门槛后创建 Execute；
- 管理冷却状态；
- 记录每次决策和执行结果。

#### benchmark-exporter

职责包括：

- 统计 Allocatable、Allocated、ReadyAllocated 和 Pending NPU；
- 统计全局及超节点碎片；
- 统计完整空闲节点、机架和超节点；
- 统计任务吞吐、超时和排队时间；
- 暴露 Prometheus 指标；
- 输出带逻辑时间的结果快照。

#### result-analyzer

离线读取两种方案的逻辑时间快照和任务事件，计算：

- 周期分配率及相对提升；
- 每逻辑日分配率；
- P5/P50/P95 分配率；
- Pending NPU 卡时；
- 任务完成率、超时率和排队时间；
- 384 卡任务启动数量；
- 完整超节点释放次数；
- Repack 迁移成本；
- 多次回放的 95% 置信区间。

### 11.6 逻辑时间结果记录

Prometheus 使用真实时间戳，而正式结果按业务逻辑时间计算，因此 Benchmark Exporter 还需要输出 CSV、JSONL 或 Parquet 快照：

```json
{
  "logicalTimeSeconds": 302400,
  "wallTime": "2026-09-01T10:03:28Z",
  "allocatedNPU": 1440,
  "readyAllocatedNPU": 1432,
  "allocatableNPU": 1920,
  "pendingNPU": 768,
  "fragmentation": 0.15,
  "fullFreeSupernodes": 0
}
```

Prometheus/Grafana 用于实时观察，逻辑时间快照用于最终 AUC 和验收计算。

### 11.7 配置和资产

建议新增以下目录：

```text
benchmark/a3-kwok/
├── Makefile
├── charts/
│   └── kwok-a3-nodes/
├── kwok/
│   └── benchmark-pod-stages.yaml
├── volcano/
│   ├── scheduler-baseline.yaml
│   └── scheduler-binpack.yaml
├── workloads/
│   ├── vcjob-8.yaml
│   ├── vcjob-48.yaml
│   ├── vcjob-192.yaml
│   ├── vcjob-384.yaml
│   └── inference-deployment.yaml
├── repack/
│   └── repack-run-template.yaml
├── monitoring/
│   ├── service-monitor.yaml
│   ├── prometheus-rules.yaml
│   └── grafana-dashboard.json
└── traces/
    └── weekly-trace.example.jsonl
```

## 12. 面向生产历史的可复用回放工程

### 12.1 工程定位

本测试工程不应只服务于一份人工构造的 A3 测试轨迹。长期目标是形成一个可复用的生产任务轨迹回放与调度策略评估工具：

```text
生产任务历史
        ↓
脱敏、标准化和完整性校验
        ↓
按时间、租户、Queue、任务类型选择回放范围
        ↓
映射到本地 KWOK ClusterProfile
        ↓
使用同一 Trace 回放不同 StrategyProfile
        ↓
比较分配率、排队时间、任务吞吐和迁移成本
```

建议将用户侧工具命名为 `volcano-replay`。它负责 Trace 管理、回放和报告，不重新实现 KWOK 节点控制，也不替代 Helm 或 kubectl。

### 12.2 目标使用体验

#### 导入生产历史

```bash
volcano-replay import \
  --source production-jobs.csv \
  --from 2026-08-01 \
  --to 2026-08-08 \
  --output traces/prod-week-20260801
```

#### 校验轨迹

```bash
volcano-replay trace validate traces/prod-week-20260801
```

预期输出：

```text
业务周期：7 天
任务数量：12438
总请求 NPU 卡时：287360
峰值需求：2584 卡
平均需求：2031 卡
整超节点任务：37 个
核心字段完整率：99.2%
拓扑字段完整率：96.8%
```

#### 本地策略回放

```bash
volcano-replay run \
  --trace traces/prod-week-20260801 \
  --cluster-profile profiles/a3-240.yaml \
  --strategies baseline,binpack-repack \
  --wall-duration 10m \
  --repeat 10
```

#### 生成报告

```bash
volcano-replay report --run runs/prod-week-20260801
```

### 12.3 生产数据来源

Kubernetes API 不会长期保存完整的任务投递历史。生产历史可以依次从以下数据源导入：

1. AI 平台任务数据库；
2. Volcano Job/PodGroup 历史数据库；
3. Prometheus 长期存储；
4. Kubernetes Audit 日志；
5. Kubernetes Events 归档；
6. 平台计费或资源使用记录。

如果现有平台已经记录任务提交、开始和结束时间，应优先开发离线 Importer，不需要立即在生产集群部署新组件。

如果历史字段不足，可以后续部署只读的轻量 Collector，持续记录：

- VCJob 创建、更新和删除；
- PodGroup 状态；
- Pod 提交、绑定、Running 和终止；
- Queue、Priority 和资源请求；
- Gang 和拓扑约束；
- 扩缩容、滚动升级和失败重试事件。

Collector 只采集调度语义，不采集 Secret、环境变量、命令行、模型名称、数据集路径或镜像凭证。

### 12.4 标准 Trace 模型

Trace Schema 是整个回放工程的稳定接口，应与具体生产数据源解耦。

每次任务投递至少包含：

```yaml
jobId: job-00123
submitTime: 2026-08-01T10:05:00Z
startTime: 2026-08-01T10:35:00Z
completionTime: 2026-08-01T18:35:00Z
workloadType: DistributedTraining
queue: training
priority: high
tasks:
  - role: worker
    replicas: 48
    npuPerPod: 8
    cpuPerPod: 64
    memoryPerPod: 512Gi
resourceName: huawei.com/Ascend910
topology: SameSupernode
minAvailable: 48
result: Succeeded
retryCount: 0
```

回放时遵循以下语义：

- 使用原始 `submitTime` 决定提交顺序；
- 使用 `completionTime - startTime` 作为任务服务时间；
- 不回放生产环境原始排队时间；
- 新的调度策略重新决定任务何时开始；
- 新排队时间是被测策略的结果。

```text
生产排队时间 = 历史结果，不直接回放
任务服务时间 = 工作负载属性，应当保留
新排队时间   = 被测调度策略产生的结果
```

弹性训练和推理还需要独立事件流：

```yaml
- businessTime: P1DT08H00M
  workloadId: serving-001
  action: Submit
  replicas: 8
- businessTime: P1DT12H00M
  workloadId: serving-001
  action: Scale
  replicas: 16
- businessTime: P1DT18H00M
  workloadId: serving-001
  action: Scale
  replicas: 6
- businessTime: P3DT10H00M
  workloadId: serving-001
  action: RollingUpdate
```

### 12.5 Trace 存储格式

每个标准 Trace 使用独立目录：

```text
traces/prod-week-20260801/
├── manifest.yaml
├── jobs.parquet
├── events.parquet
├── cluster-snapshot.yaml
├── resource-mapping.yaml
├── summary.json
└── checksums.txt
```

`manifest.yaml` 示例：

```yaml
apiVersion: replay.volcano.sh/v1alpha1
kind: WorkloadTrace
metadata:
  name: prod-week-20260801
spec:
  startTime: 2026-08-01T00:00:00Z
  endTime: 2026-08-08T00:00:00Z
  source: production-history
  anonymized: true
  resourceMapping:
    ascend910: huawei.com/Ascend910
```

大规模任务数据使用 Parquet 或其他列式文件保存，不把完整 Trace 存入 Kubernetes CRD。CRD 或 YAML 只保存元数据、引用和运行配置。

### 12.6 数据脱敏

生产历史导入时必须自动脱敏：

```text
namespace  → ns-<hash>
user       → user-<hash>
jobName    → job-<sequence>
podName    → pod-<sequence>
queueName  → queue-<hash>
nodeName   → node-<topology-index>
image      → 删除
command    → 删除
env/secret → 删除
volumePath → 删除或只保留调度属性
```

必须保留：

- CPU、内存和 NPU 请求；
- Pod 副本数和角色；
- Gang 语义；
- Queue 和 Priority 的相对关系；
- NodeSelector、Affinity 和 Taint/Toleration；
- 机架、超节点等拓扑要求；
- 提交时间、服务时间和 Deadline；
- 扩缩容、重试和滚动升级事件。

### 12.7 按需筛选回放

Importer 应支持按以下维度选取生产历史：

- 时间范围；
- Namespace 或租户；
- Queue；
- Priority；
- NPU 型号；
- 训练或推理；
- 单任务 NPU 规模；
- 成功、失败或重试任务；
- 资源池或超节点范围；
- 是否具有拓扑约束。

示例：

```bash
volcano-replay import \
  --source production-jobs.csv \
  --from 2026-08-01 \
  --to 2026-08-08 \
  --queue training \
  --npu-model Ascend910 \
  --min-npu 8 \
  --output traces/training-peak-week
```

这使得同一工具能够分别回放全量流量、训练高峰、推理扩容高峰、特定租户、大规格任务积压或故障时间窗口。

### 12.8 ClusterProfile

生产 Trace 与本地 KWOK 环境通过 ClusterProfile 解耦：

```yaml
apiVersion: replay.volcano.sh/v1alpha1
kind: ClusterProfile
metadata:
  name: a3-240
spec:
  nodes: 240
  npuPerNode: 8
  topology:
    supernodes: 5
    nodesPerSupernode: 48
    racksPerSupernode: 8
    nodesPerRack: 6
  resources:
    npu: huawei.com/Ascend910
    cpuPerNode: 192
    memoryPerNode: 1024Gi
  kwok:
    nodeSelector:
      accelerator-pool: ascend-a3-kwok
```

后续可以增加不同资源池：

```text
profiles/a3-240.yaml
profiles/a3-480.yaml
profiles/910b-128.yaml
profiles/mixed-910b-310p.yaml
```

如果本地集群容量与生产环境不同，应优先筛选或抽样生产任务，保持单任务资源形状不变。不能简单把每个任务的 NPU 请求等比例缩小，否则会改变 Gang 和拓扑调度语义。

### 12.9 StrategyProfile

调度策略也应配置化：

```yaml
apiVersion: replay.volcano.sh/v1alpha1
kind: StrategyProfile
metadata:
  name: binpack-repack
spec:
  scheduler:
    binpack:
      enabled: true
      weight: 10
      resources:
        huawei.com/Ascend910: 10
  repack:
    enabled: true
    fragmentationHighWatermark: 0.10
    fragmentationLowWatermark: 0.05
    pendingDemandThreshold: 384
    minFreedNodes: 48
    maxMovedNPU: 192
    maxAffectedPodGroups: 128
    cooldown: 1h
```

基线策略：

```yaml
apiVersion: replay.volcano.sh/v1alpha1
kind: StrategyProfile
metadata:
  name: baseline
spec:
  scheduler:
    binpack:
      enabled: false
  repack:
    enabled: false
```

当前正式验收仍只比较 `baseline` 和 `binpack-repack`。Profile 机制用于后续按需验证不同 Binpack 权重、Repack 预算、Queue 或 Priority 策略，无需修改回放器代码。

### 12.10 用户入口与内部组件

`volcano-replay` 是统一用户入口，但不负责实现 KWOK 节点生命周期。它调用 Helm、kubectl 和 Kubernetes API，完成以下工作：

- 导入、脱敏和校验 Trace；
- 选择 ClusterProfile 和 StrategyProfile；
- 创建 Replay Runner；
- 等待运行完成；
- 收集结果并生成报告。

运行时组件可以进一步收敛为：

```text
replay-runner   任务回放、逻辑时钟、生命周期和指标快照
repack-trigger  实验方案中的碎片检测与 Repack 触发
```

`benchmark-exporter` 可以在首版独立部署，也可以后续合并进 `replay-runner`，减少长期维护的组件数量。

### 12.11 可复现运行包

每次运行生成完整 Bundle：

```text
runs/run-20260901-001/
├── run.yaml
├── trace/
├── cluster-profile.yaml
├── strategies/
├── environment/
│   ├── kubernetes-version.txt
│   ├── volcano-version.txt
│   ├── kwok-version.txt
│   └── component-images.txt
├── baseline/
│   ├── logical-metrics.parquet
│   ├── job-events.parquet
│   └── logs/
├── binpack-repack/
│   ├── logical-metrics.parquet
│   ├── job-events.parquet
│   ├── repack-events.parquet
│   └── logs/
└── report/
    ├── summary.json
    ├── report.html
    └── charts/
```

Bundle 必须记录 Trace Hash、随机种子、Profile、组件版本和镜像摘要，使历史结论可以重放和审计。

### 12.12 易用性演进路线

第一阶段先支持手工或合成 Trace、A3-240 ClusterProfile、两个 StrategyProfile 和命令行报告。

第二阶段定义稳定 Trace Schema，实现生产 CSV/数据库 Importer、自动脱敏、字段完整性校验和按需筛选。

第三阶段增加推理扩缩容、滚动升级、失败重试、PDB、多资源池、多型号 NPU 和更复杂的 Queue/拓扑策略。

第四阶段提供一键 Helm 部署和可选 Web 页面，用于选择 Trace、Profile、Strategy，查看运行进度和管理历史报告。

首版期望体验：

```bash
helm upgrade --install volcano-replay ./deploy/helm/volcano-replay

volcano-replay import \
  --source jobs.csv \
  --output traces/prod-week

volcano-replay run \
  --trace traces/prod-week \
  --cluster-profile a3-240 \
  --strategies baseline,binpack-repack \
  --wall-duration 10m

volcano-replay report --latest
```

## 13. 执行流程

### 12.1 环境准备

1. 确认当前 kubeconfig 指向专用测试集群。
2. 检查 Volcano、Repack 和 Prometheus 组件。
3. 安装 KWOK Controller 和 Benchmark Pod Stage。
4. 创建 240 个 A3 KWOK Node。
5. 校验拓扑和总资源。

预期结果：

```text
Ready KWOK 节点数 = 240
超节点数 = 5
每超节点节点数 = 48
机架数 = 40
每机架节点数 = 6
集群 Allocatable NPU = 1920
```

建议 Makefile 入口：

```text
make install-kwok
make create-a3-nodes
make verify-topology
```

### 12.2 冒烟验证

正式回放前依次验证：

1. 8 卡单节点任务能够被 Volcano 调度到 KWOK Node；
2. 48 卡任务能够调度到同机架的 6 个节点；
3. 384 卡任务能够调度到同超节点的 48 个节点；
4. Pod 能够进入 Running/Ready；
5. VCJob 结束后资源能够释放；
6. Repack DryRun 能够生成计划；
7. Execute 能够 Evict、重建并重新调度替身 Pod；
8. Exporter 统计结果与 Kubernetes API 快照一致。

### 12.3 轨迹生成和预评估

1. 生成固定一周任务轨迹；
2. 记录 Trace ID、Hash 和随机种子；
3. 回放逻辑 48 小时基线；
4. 周期性执行 DryRun 但不执行迁移；
5. 计算可释放完整超节点和反事实分配率上限；
6. 校准负载率、碎片阈值和迁移预算。

只有理论提升上限达到 25%～30% 时，才进入正式 20% 目标验证。若理论上限低于 20%，应调整业务场景或降低目标，不应通过截取瞬时峰值宣称达标。

### 12.4 基线方案

1. 关闭 Binpack；
2. 不运行 repack-trigger；
3. 清理 Benchmark Namespace 和历史 RepackRun；
4. 校验 240 个节点恢复到初始状态；
5. 加载固定 Trace；
6. 启动 workload-replayer 和 benchmark-exporter；
7. 等待回放完成；
8. 归档逻辑时间快照、任务事件和控制面指标。

建议入口：

```text
make configure-baseline
make run-baseline TRACE=weekly-trace.jsonl
```

### 12.5 环境重置

Reset 只允许删除带有 Benchmark `run-id` 的资源：

- Benchmark Namespace 中的工作负载；
- 本轮测试产生的 RepackRun；
- Benchmark 状态对象和临时结果；
- 本轮测试的 Prometheus/本地结果目录。

不得删除用户业务、Volcano 组件或其他无关资源。

建议入口：

```text
make reset-workloads RUN_ID=<run-id>
```

### 12.6 实验方案

1. 启用 NPU Binpack；
2. 确认 Scheduler 已加载配置；
3. 恢复与基线相同的初始节点状态；
4. 加载相同 Trace 和随机种子；
5. 启动 workload-replayer；
6. 启动 benchmark-exporter；
7. 启动 repack-trigger；
8. 等待回放完成；
9. 归档所有 DryRun、Execute、逻辑快照和任务事件。

建议入口：

```text
make configure-treatment
make run-treatment TRACE=weekly-trace.jsonl
```

### 12.7 结果分析

```text
make report BASELINE_RUN=<run-id> TREATMENT_RUN=<run-id>
```

报告至少输出：

- `summary.json`；
- `allocation-comparison.csv`；
- `job-comparison.csv`；
- `repack-events.csv`；
- `report.html`；
- `grafana-dashboard.json`；
- `run-manifest.yaml`。

## 14. 20% 目标的可行性

集群总容量为 1920 卡，一个完整超节点为 384 卡，占集群容量 20 个百分点。

| 基线分配率 | 多运行一个 384 卡任务后 | 相对提升 |
| ---: | ---: | ---: |
| 60% | 80% | 33.3% |
| 70% | 90% | 28.6% |
| 75% | 95% | 26.7% |
| 80% | 100% | 25.0% |
| 85% | 最高 100% | 17.6% |

当基线分配率长期处于 70%～80% 时，如果实验方案能够持续比基线多运行一个整超节点任务，理论上具备相对提升 20% 的空间。

20% 并非默认保证，实际结果取决于：

- 基线是否存在足够的可恢复碎片；
- 目标超节点上的 Pod 是否允许迁移；
- 其他超节点是否有接收容量；
- Pending 队列是否持续存在能够消费完整超节点的任务；
- CPU、内存、Queue、Quota、拓扑或控制面是否成为真正瓶颈；
- Repack 执行预算是否足以形成完整超节点，而不是只释放零散节点。

## 15. 验收标准

正式验收同时满足：

1. 实验方案全周期 NPU 分配率相对基线提升不低于 20%。
2. 两种方案中 Pending NPU 需求不低于 384 卡的时间比例均达到 95%。
3. 实验方案至少 6/7 个逻辑日或 25/30 个逻辑日的日均分配率高于基线。
4. 分配率提升不能依赖单次 Repack 后的短期峰值。
5. 实验方案完成的 NPU 卡时和 384 卡训练任务数高于基线。
6. 实验方案任务超时率和大任务排队时间低于基线。
7. Repack 能够重复形成完整空闲超节点。
8. protected 工作负载驱逐数量为 0。
9. 被迁移工作负载的替身 Pod 恢复成功率为 100%。
10. Running/Ready 有效分配率与声明分配率的变化方向一致。
11. 多个固定随机种子的平均提升达到 20%，并报告 95% 置信区间。
12. 控制面未因加速回放持续饱和。
13. 基线和实验的 Trace Hash、节点容量和非目标调度配置完全一致。

## 16. 分阶段实施计划

### 阶段一：基础环境

- 部署和验证 Repack 能力；
- 安装 KWOK；
- 创建 240 个 A3 KWOK Node；
- 配置 5 超节点、40 机架拓扑；
- 验证 Volcano 能识别 1920 卡扩展资源。

### 阶段二：生命周期与工作负载

- 实现 Benchmark KWOK Pod Stage；
- 提供 8/48/192/384 卡 VCJob 模板；
- 实现 workload-replayer；
- 跑通任务提交、Pending、Running 和完成。

### 阶段三：指标和结果

- 实现 benchmark-exporter；
- 输出逻辑时间快照；
- 建立 Prometheus 和 Grafana 面板；
- 实现 result-analyzer。

### 阶段四：自动 Repack

- 实现全局和超节点碎片检测；
- 实现目标超节点选择；
- 自动创建和审核 DryRun；
- 满足门槛后自动创建 Execute；
- 记录完整决策链路。

### 阶段五：预评估

- 回放逻辑 48 小时基线；
- 计算理论收益上限；
- 校准 Offered Load、碎片阈值和 Repack 预算；
- 固化正式测试参数。

### 阶段六：正式验证

- 一周业务曲线压缩为 10 分钟；
- 两种方案使用相同轨迹和多个固定随机种子重复运行；
- 通过后将一个月业务曲线压缩为 30 分钟运行；
- 输出长期分配率、任务吞吐、排队时间和 Repack 成本报告。

## 17. 最小可运行版本

首个 MVP 只实现：

1. 240 个 A3 KWOK Node；
2. Pod Running/Ready/Delete Stage；
3. 固定一周任务轨迹；
4. 8/48/192/384 卡 VCJob 模板；
5. workload-replayer；
6. Binpack 配置切换；
7. repack-trigger；
8. benchmark-exporter 和逻辑时间结果文件；
9. 基线与实验 AUC 比较脚本。

MVP 首先跑通以下闭环：

```text
小任务持续制造碎片
        ↓
384 卡任务持续 Pending
        ↓
碎片率和超节点容量满足触发条件
        ↓
DryRun 识别可腾空超节点
        ↓
Execute 迁移 eligible Pod
        ↓
形成完整空闲超节点
        ↓
384 卡任务进入 Running
        ↓
长期分配率和任务吞吐提高
```

MVP 通过后再增加推理扩缩容、滚动升级、失败重试、PDB、超节点故障和月度轨迹。

## 18. 预期结论

本方案希望形成以下可审计结论：

> 在 240 个 KWOK 节点模拟的 5 个昇腾 A3 超节点环境中，面对训练推理混部、任务持续投递与结束、整超节点训练任务持续排队的业务场景，开启 Binpack 和基于碎片率及超节点容量自动触发的 Repack 后，相较关闭两项能力的基线，长期时间加权 NPU 调度分配率相对提升达到 20% 以上，同时改善 384 卡训练任务的准入率和排队时间。

该结论仅适用于本文定义的集群规模、负载强度、任务结构、可迁移比例和拓扑条件，不应扩展为所有 NPU 集群都能固定提升 20%。
