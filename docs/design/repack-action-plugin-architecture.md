# Repack Action + Plugin 架构设计

## 1. 设计目标

Repack 的扩展模型与 Volcano Scheduler 保持一致：Action 维护稳定、可读的主流程，Plugin 通过 Session 回调组合不同场景的策略，Planner 只负责高性能搜索和增量状态维护。

本次调整不再保留可配置的 Core 注册表。当前只有一种经过性能验证的惰性 Drain 搜索机制，将搜索机制再次包装成可切换 Core 只会增加调用层级，并容易把场景策略继续堆回算法内部。后续增加 Node、HyperNode、Gang、PDB 或新的接收策略时，应优先增加 Plugin 或回调，而不是复制 Action 主流程。

## 2. 分层职责

```text
Engine
  └─ OpenSession(snapshot, plugins)
       ├─ Plugin.OnSessionOpen: 注册策略回调
       └─ RunActions
            └─ Repack Action
                 ├─ 度量整理前碎片
                 ├─ 调用 Lazy Drain Planner 构建计划
                 ├─ 执行计划收益硬约束
                 ├─ 计算扰动成本
                 └─ 生成 Plan / Report

Execute 副作用仍由 Engine 在计划持久化后提交，不进入 Action 或 Plugin。
```

各层边界如下：

- **Engine**：生命周期、Session 创建、状态持久化、Execute 提交和错误处理。
- **Action**：一次 Repack 的稳定业务编排，即“度量、规划、准入、汇总、报告”。
- **Planner**：候选准备、增量状态、惰性可行性模拟和首个可行候选提交。Planner 不解释 Scope、预算、Gang 或装箱策略。
- **Plugin**：场景规则和策略，通过 `AddXxxFn` 注册到 Session。
- **Snapshot Adapter**：复用 Volcano Scheduler 的完整过滤栈完成只读调度模拟。

## 3. Action 主流程

`actions/repack` 是当前唯一生产 Action，其流程固定为：

1. 基于 Session 节点快照计算整理前碎片率；
2. 调用 `planner/drain.BuildPlan` 构建候选计划；
3. 通过 `Session.PlanAdmissible` 执行最终收益和计划约束；
4. 计算 `DisruptionCost`，记录计划与可解释日志；
5. 生成 Report，供 Engine 写入 RepackRun status。

Planner 返回 `nil` 或计划未通过约束时，Action 仍输出当前碎片率，确保 `NoFragmentation` 与 `BelowGoalThreshold` 可区分。

## 4. Plugin 扩展面

Session 提供以下正交扩展点：

| 扩展点 | 聚合语义 | 作用时机 |
|---|---|---|
| `AddMovableFn` | AND，任一插件可否决 | 准备可驱逐 Pod |
| `AddDomainFn` | Union | 枚举 Node/HyperNode 等可释放 Unit |
| `AddCandidateFilterFn` | 按规范化插件名顺序短路 | 扰动评分前的低成本硬过滤 |
| `AddDisruptionScoreFn` | 逐维归一化后加权 | 对可腾空候选做软排序 |
| `AddVictimOrderFn` | 字典序比较器链 | 决定调度模拟中的 Pod 尝试顺序 |
| `AddReceiverPoolFn` | 按规范化插件名顺序链式裁剪 | 规划开始时形成接收节点集合 |
| `AddReceiverRankFn` | 按 Stability、Disruption、Packing 阶段组成字典序 rank | 每次模拟前排列接收节点 |
| `AddConstraintFn` | AND，任一插件可否决 | 成品计划最终准入 |

`CandidateFilterFn` 返回稳定的 `Reason`，用于指标和 V4 日志。过滤条件在单轮规划内必须具有单调性，因为被拒绝的 Unit 不会在提交更多迁移后重新加入；`MarkInfeasible` 还表示该 Unit 已被证明无法腾空，其节点可作为优先 receiver。Plugin 不能修改 Planner 内部状态，只能读取 `PlanningCandidate` 和 `ReceiverCandidate` 公开的事实。

`ReceiverPoolFn` 只允许对当前接收集合做裁剪。Framework 按当前集合的节点顺序执行交集和去重，后续 Plugin 不能恢复此前已经剔除的节点、注入快照外节点或通过重复节点放大接收容量；接收顺序统一由 `ReceiverRankFn` 表达。

接收节点 rank 在排序前对每个节点、每个插件只计算一次，再进行稳定排序，避免比较器反复执行 Gang 聚合导致 `O(R log R)` 次策略计算。Framework 统一定义 Stability、Disruption、Packing 三个决策阶段；插件选择语义阶段，同一阶段内按规范化插件名顺序组合，不再通过配置排列或 10/20/30 等私有数字协调全局顺序。

## 5. 当前 Plugin 职责

默认启用的插件集合如下，展示顺序仅用于表达处理层次：

```text
{workloadscope, repackbudget, nodeconsolidation, workloaddisruption, gangdisruption, binpack}
```

- `workloadscope`：把 RepackRun 中已经解析的工作负载范围注册成 Movable 授权边界。
- `repackbudget`：以“已提交迁移 + 当前候选”为完整计划检查 `maxPerRun.podGroups` 和目标资源迁移量。
- `nodeconsolidation`：只为目标资源部分占用节点贡献单节点 `FreeableUnit`；空节点保持空闲，满卡节点视为已经完成装箱，不作为待腾空源节点。未来 HyperNode 插件可通过同一 Domain 扩展点贡献其他 Unit。
- `workloaddisruption`：工作负载数量、迁移资源量和迁移 Pod 数等通用中断成本评分。
- `gangdisruption`：评估 `minAvailable` 是否被突破和受损资源量；接收节点排序时优先填充未来腾空会造成更高 Gang 代价的节点。
- `binpack`：按目标资源请求量从大到小排列 victim，优先填充确定会保持占用的节点，最后按目标资源 best-fit 排序；它不再承担基础接收节点合法性判断。

这些 Plugin 只负责策略，不调用 `FeasibleRelocation`，也不决定 Planner 的循环终止条件。

节点分类和接收端目标资源总容量预检是所有装箱策略共享的必要条件，属于 Planner 的不可关闭能力。Planner 在插件和候选评分前只保留 `0 < Used[R] < Allocatable[R]` 且存在 scheduler-visible slack 的接收节点；因此空节点不会被新点亮，满卡节点不会进入接收排序或完整调度模拟。总容量预检随后在中断成本评分前排除必然失败的候选；通过预检只代表总量可能容纳，最终可行性仍由 `FeasibleRelocation` 的完整调度过滤栈决定。

Gang 接收策略使用的“某节点未来被腾空时将影响哪些 PodGroup”由 `gangdisruption` Plugin 按实际进入排序的 receiver 惰性建立缓存。空节点、满卡节点、不可用节点及已被裁剪节点不会触发任务扫描或缓存分配。Drain Planner 只提供节点、剩余容量和动态占用状态，不持有 PodGroup 聚合字段；未启用 `gangdisruption` 时也不会执行这部分计算。

### 5.1 Plugin 配置

Repack Engine 使用独立的 `--repack-conf` 管理 Action 和 Plugin；生产部署中该普通 YAML 文件由 ConfigMap 挂载，不是 CR，也没有对应 CRD。`--scheduler-conf` 只负责提供与 Volcano Scheduler 一致的 tiers、plugins 和调度过滤栈。Repack Plugin 支持与 Scheduler 类似的 `name + arguments` 配置模型：

```yaml
# 与 Scheduler actions 的形式一致：双引号字符串，多个 Action 以逗号分隔。
actions: "repack"

plugins:
  - name: workloadscope
  - name: repackbudget
  - name: nodeconsolidation
  - name: workloaddisruption
    arguments:
      affectedPodGroupsWeight: 10
      movedResourceWeight: 3
      movedPodsWeight: 1
  - name: gangdisruption
    arguments:
      gangBreachesWeight: 8
      damagedResourceWeight: 6
  - name: binpack
```

Plugin 列表按能力集合解释，配置排列不参与策略优先级。`OpenSession` 会复制配置并按插件名建立规范化顺序，再执行 `OnSessionOpen`；因此交换任意两个插件的位置不会改变过滤、评分、Domain 合并、victim 顺序或 receiver 排序。Action 列表仍是有序执行管线，不适用这一规则。

未指定 `--repack-conf` 时使用内置 Action 和 Plugin 默认值。`--repack-actions`、`--repack-plugins` 可用于命令行覆盖，并优先于独立配置文件；需要结构化 `arguments` 时使用 `repack-conf`。配置采用严格 YAML 解析，未知或拼写错误的顶层字段、Plugin 字段及参数都会导致加载失败，不会静默回退默认行为。`workloadscope`、`repackbudget`、`workloaddisruption`、`gangdisruption` 和 `binpack` 都是独立可选能力，删除后只关闭对应授权、预算、评分或装箱策略；空/满节点裁剪和完整调度校验仍然生效。`repack` Action 通过 Capability 而不是插件名要求至少一个 Domain provider；当前 `nodeconsolidation` 提供 `domain`，未来可由 HyperNode Domain 替代。Engine 在配置加载阶段检查静态 Capability 组合，并在每次 Session 打开后确认实际注册了对应回调；缺少 Domain provider、插件参数非法、能力依赖不满足或只声明能力但未注册回调时均失败关闭，避免静默产生空计划。静态编译进二进制的插件仍需在 `cmd/volcano-repack-engine/main.go` 导入注册，这一点与 Volcano Scheduler 的内置插件一致，但启停、排序和参数调整不再需要修改代码。

`workloaddisruption` 和 `gangdisruption` 的权重用于多个可腾空候选之间的中断成本排序。每个评分项先在当轮候选集合内反向归一化为 `0～100` 的整数偏好分，再按整数权重求和，总分越高越优；候选在某项上全部相同时均得 100 分，不改变相对顺序。省略字段使用上述默认值，配置为 `0` 可关闭对应评分项；小数、负数、字符串数值或未知参数会被视为配置错误。这里的权重不影响接收节点排序，接收节点仍按 `Stability → Disruption → Packing` 三个阶段进行固定字典序比较，避免不同量纲互相抵消。

- `affectedPodGroupsWeight`：影响的工作负载（内部以 PodGroup 聚合）数量；
- `movedResourceWeight`：迁移的目标资源总量；
- `movedPodsWeight`：迁移的 Pod 数量；
- `gangBreachesWeight`：迁移后低于 `minAvailable` 的工作负载数量；
- `damagedResourceWeight`：突破 `minAvailable` 后受影响工作负载对应的目标资源规模。

## 6. Lazy Drain Planner

Planner 保留以下与场景无关的搜索机制：

1. 一次性分类节点，只保留部分占用源节点及部分占用、有余量的接收节点；
2. 准备 Unit、victim 和节点资源增量数据，防御性拒绝 Domain 贡献的空/满节点；
3. 每轮对活动 Unit 执行接收总容量和 Candidate Filter；
4. 对通过过滤的候选进行多策略扰动评分；
5. 沿评分顺序惰性调用 `FeasibleRelocation`；
6. 原子提交第一个完整可行的 Unit；
7. 增量更新 drained、filled、receiver slack 和已提交 moves；
8. 没有可提交候选时结束。

Planner 不枚举全部可行计划，也不为全局最优进行回溯。其性能约束是：低成本过滤发生在评分和调度模拟之前；同一轮只为通过过滤的候选计算评分；调度模拟在找到首个可行候选后立即停止。

## 7. 新场景接入方式

新增场景应先判断它影响哪个决策点：

- “某类工作负载不可驱逐” → `MovableFn`；
- “某组节点才算一个有价值的释放目标” → `DomainFn`；
- “候选超过业务预算” → `CandidateFilterFn`；
- “同样可行时更少影响某类任务” → `DisruptionScoreFn`；
- “接收节点需要新的优先顺序” → `ReceiverRankFn`；
- “成品计划必须满足某个目标” → `ConstraintFn`。

例如增加 PDB 规划期约束时，新建 `plugins/pdb`，在 Session Open 时注册 Candidate Filter 或 Movable 回调，无需修改 `actions/repack` 和 Drain 循环。`DomainFn` 已为 HyperNode 等多节点目标预留扩展面；Node 与 HyperNode 重叠 Unit 的增量消费语义尚未纳入本阶段，正式接入 HyperNode Plugin 前需要单独补齐状态模型和端到端测试。

只有当新增能力改变“一个 RepackRun 要经历哪些业务阶段”时才新增 Action；只有当它改变底层搜索机制、且现有 Planner 无法复用时才增加新的 Planner 实现。两者都不应通过恢复 Core 注册表实现。

## 8. 目录结构

```text
pkg/repackengine/
  actions/repack/       # 稳定主流程
  planner/drain/        # 惰性搜索机制与性能基准
  plugins/
    workloadscope/      # 工作负载授权边界
    repackbudget/       # maxPerRun
    nodeconsolidation/  # Node Unit
    workloaddisruption/ # 通用中断评分
    gangdisruption/     # Gang 成本与接收策略
    binpack/             # Victim 与接收节点装箱排序
  framework/            # Session、Action、Plugin 与回调聚合
  adapter/              # scheduler Session/Snapshot 适配
```

## 9. 正确性与性能验证

测试分为四层：

- Framework 契约测试：Capability 依赖、短路、AND/Union、比较器组合、rank 阶段、回调单次求值、插件参数传递和输入切片隔离；
- Plugin/Planner 功能测试：空/满节点在评分前裁剪，预算、容量、Scope、Gang 和接收排序保持既有行为；
- Action/Engine 测试：收益门控、Report、DryRun/Execute 状态语义不变；
- Plugin 组合测试：固定一个 Domain provider 后遍历 32 种可选插件组合，验证主流程和接收边界不变；
- 规模基准：4000 节点成功与不可行场景的耗时、内存、候选数和调度模拟次数。

本次重构不得放宽 `INV-RESCHED`、`maxPerRun`、Scope 授权边界和收益门槛，也不得恢复对所有候选预先执行完整调度模拟的旧模式。
