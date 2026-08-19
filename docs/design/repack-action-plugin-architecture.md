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
| `AddCandidateFilterFn` | 按插件顺序短路 | 扰动评分前的低成本硬过滤 |
| `AddDisruptionScoreFn` | 逐维归一化后加权 | 对可腾空候选做软排序 |
| `AddVictimOrderFn` | 字典序比较器链 | 决定调度模拟中的 Pod 尝试顺序 |
| `AddReceiverPoolFn` | 按顺序链式裁剪 | 规划开始时形成接收节点集合 |
| `AddReceiverRankFn` | 显式优先级的字典序 rank | 每次模拟前排列接收节点 |
| `AddConstraintFn` | AND，任一插件可否决 | 成品计划最终准入 |

`CandidateFilterFn` 返回稳定的 `Reason`，用于指标和 V4 日志。过滤条件在单轮规划内必须具有单调性，因为被拒绝的 Unit 不会在提交更多迁移后重新加入；`MarkInfeasible` 还表示该 Unit 已被证明无法腾空，其节点可作为优先 receiver。Plugin 不能修改 Planner 内部状态，只能读取 `PlanningCandidate` 和 `ReceiverCandidate` 公开的事实。

接收节点 rank 在排序前对每个节点、每个插件只计算一次，再进行稳定排序，避免比较器反复执行 Gang 聚合导致 `O(R log R)` 次策略计算。多个 rank 按优先级从小到大组成字典序，优先级相同时保持插件注册顺序。

## 5. 当前 Plugin 职责

默认顺序为：

```text
resource → scope → budget → node → base → gang → binpack
```

- `resource`：目标资源总接收容量预检；按目标资源请求量从大到小排列 victim，以便尽早发现不可行装箱。
- `scope`：把 RepackRun 中已经解析的业务范围注册成 Movable 授权边界。
- `budget`：以“已提交迁移 + 当前候选”为完整计划检查 `maxPerRun.podGroups` 和目标资源迁移量。
- `node`：贡献单节点 `FreeableUnit`。未来 HyperNode 插件可通过同一 Domain 扩展点贡献其他 Unit。
- `base`：工作负载数量、迁移资源量和迁移 Pod 数等通用扰动评分。
- `gang`：评估 `minAvailable` 是否被突破和受损资源量；接收节点排序时优先填充未来腾空会造成更高 Gang 代价的节点。
- `binpack`：排除目标资源未占用节点作为 receiver；优先填充确定会保持占用的节点，最后按目标资源 best-fit 排序。

这些 Plugin 只负责策略，不调用 `FeasibleRelocation`，也不决定 Planner 的循环终止条件。

## 6. Lazy Drain Planner

Planner 保留以下与场景无关的搜索机制：

1. 一次性准备 Unit、victim 和节点资源增量数据；
2. 每轮对活动 Unit 调用 Candidate Filter；
3. 对通过过滤的候选进行多策略扰动评分；
4. 沿评分顺序惰性调用 `FeasibleRelocation`；
5. 原子提交第一个完整可行的 Unit；
6. 增量更新 drained、filled、receiver slack 和已提交 moves；
7. 没有可提交候选时结束。

Planner 不枚举全部可行计划，也不为全局最优进行回溯。其性能约束是：低成本过滤发生在评分和调度模拟之前；同一轮只为通过过滤的候选计算评分；调度模拟在找到首个可行候选后立即停止。

## 7. 新场景接入方式

新增场景应先判断它影响哪个决策点：

- “某类工作负载不可驱逐” → `MovableFn`；
- “某组节点才算一个有价值的释放目标” → `DomainFn`；
- “候选超过业务预算” → `CandidateFilterFn`；
- “同样可行时更少影响某类任务” → `DisruptionScoreFn`；
- “接收节点需要新的优先顺序” → `ReceiverRankFn`；
- “成品计划必须满足某个目标” → `ConstraintFn`。

例如增加 PDB 规划期约束时，新建 `plugins/pdb`，在 Session Open 时注册 Candidate Filter 或 Movable 回调，无需修改 `actions/repack` 和 Drain 循环。增加 HyperNode 整理时，新建 `plugins/hypernode` 贡献目标 Unit；若需要按 TP/EP 或推理 Role 卡数倍数判断收益，再注册对应的成品计划 Constraint。

只有当新增能力改变“一个 RepackRun 要经历哪些业务阶段”时才新增 Action；只有当它改变底层搜索机制、且现有 Planner 无法复用时才增加新的 Planner 实现。两者都不应通过恢复 Core 注册表实现。

## 8. 目录结构

```text
pkg/repackengine/
  actions/repack/       # 稳定主流程
  planner/drain/        # 惰性搜索机制与性能基准
  plugins/
    resource/           # 资源容量和 victim 顺序
    scope/              # 工作负载授权边界
    budget/             # maxPerRun
    node/               # Node Unit
    base/               # 通用扰动评分
    gang/               # Gang 成本与接收策略
    binpack/             # 接收集合与装箱排序
  framework/            # Session、Action、Plugin 与回调聚合
  adapter/              # scheduler Session/Snapshot 适配
```

## 9. 正确性与性能验证

测试分为四层：

- Framework 契约测试：短路、AND/Union、比较器组合、rank 优先级、回调单次求值和输入切片隔离；
- Plugin/Planner 功能测试：预算、容量、Scope、Gang 和接收排序保持既有行为；
- Action/Engine 测试：收益门控、Report、DryRun/Execute 状态语义不变；
- 规模基准：4000 节点成功与不可行场景的耗时、内存、候选数和调度模拟次数。

本次重构不得放宽 `INV-RESCHED`、`maxPerRun`、Scope 授权边界和收益门槛，也不得恢复对所有候选预先执行完整调度模拟的旧模式。
