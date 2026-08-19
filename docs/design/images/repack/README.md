# Repack 设计配图

本目录存放 `docs/design/repack-policy-design.md`（运行期碎片整理 Repack）的配图，均为独立可渲染的 SVG（颜色已内联，GitHub / 浏览器 / PPT 均可直接打开）。

| 文件 | 用途 | 对应章节 |
|------|------|----------|
| `defrag-before-after.svg` | 整理前后效果：4 节点 ×8 卡，作业拢紧、腾出 2 个整空节点、作业不停 | §4.14.0 |
| `concentration-score.svg` | 集中度分数（Σused²）讲解：`6/4/4/2`(72) → `8/8/0/0`(128)，越扎堆分越高、空节点越多 | §4.14.6 |
| `algorithm-selection.svg` | 评审一页图：方案 A 节点腾空法 vs 方案 B 集中度法 + 三条选型路线 | §4.17 |
| `multi-objective-framework.svg` | 多目标泛化三轴（目标粒度×形状×域）+ NVLink/超节点 k-配额（P1） | §4.15.5 |
| `gang-damage-stepfn.svg` | 受损卡数按 gang 语义计的**阶跃函数**：没破 minAvailable=只赔搬走的卡、破了=整作业全赔、已破再搬=边际 0；8 卡 vs 1024 卡对比 | §4.16.5 |
| `repack-engine-architecture.svg` | **整体架构与扩展点**（Volcano 风格）：apiserver→复用 schedcache→引擎 Session(OpenSession→repack action(Core=drain)→CloseSession)→movecost/node/gang 插件注册的扩展点函数；底部含 4 回调 / 3 接缝 / 3 组件注册表 | 引擎扩展模型 |
| `repack-end-to-end.svg` | **端到端使用与模块交互**（讲解用，①–⑦）：用户建 RepackRun/RepackPolicy → admission 校验 → engine 规划(DryRun 报告 / Execute 驱逐+提名) → 各负载各自控制器重建 pod → repack-controller 打 nominatedNodeName → scheduler 落位 + 排队作业占空位 → 用户取结果 | 端到端使用 |
| `repack-controller-architecture.svg` | **repack-controller 架构与职责**（Volcano 风格）：左=apiserver(RepackRun/RepackPolicy P1/替身 Pod)→中=控制器三个 reconciler（① RunGC·TTL、② 提名 reconciler、③ RepackPolicy 生成 P1）+ `pkg/state` 纯函数→右=协作产出（引擎写 relocations、patch pod.nominatedNodeName、scheduler honor、DELETE/CREATE Run）；底部收纳边界(不做准入/不做驱逐)、部署(shim 或独立)、配置 | repack-controller |
| `core-invocation.svg` | **图1 · core 外层调用链**：engine reconcile→gate→process → ①调度器 Session(OpenSession/ResolveScope/Snapshot) → ②引擎 Session(framework.OpenSession 注册回调 + RunActions) → ③action 里 `GetCore(drain).Plan(esn)`，Execute 再 CommitPlan(Evict/Nominate) → engine 写回 status | 整理算法详解 |
| `core-drain-flow.svg` | 历史流程草图：字典序增量代价版本，已被“多策略扰动预排序 + 惰性完整调度校验”替代，不再作为当前实现说明 | 历史存档 |
| `core-unit-cost.svg` | 历史评分草图：三元组字典序版本，已被 `DisruptionScores` 的多维归一化加权评分替代，不再作为当前实现说明 | 历史存档 |
| `heuristic-limit-01-budget.svg` | 局部低成本候选消耗 PodGroup 预算，对比复用同一 PodGroup 的全局序列 | 启发式规划 §5.1 |
| `heuristic-limit-02-first-fit.svg` | first-fit 抢占受限 Pod 唯一落点，对比联合 Pod—Node 映射 | 启发式规划 §5.2 |
| `heuristic-limit-03-best-fit.svg` | best-fit 填充未来腾空目标，对比保留目标后的两节点释放路径 | 启发式规划 §5.3 |
| `heuristic-limit-04-tie-break.svg` | 同分候选名称排序，对比考虑后续潜力的选择 | 启发式规划 §5.4 |
| `heuristic-limit-05-normalization.svg` | 每轮相对归一化，对比固定目标下的完整路径成本 | 启发式规划 §5.5 |
| `heuristic-limit-06-search-space.svg` | 预定义单元与单跳路径，对比临时缓冲和多跳联合移动 | 启发式规划 §5.6 |
| `heuristic-limit-07-cache.svg` | 不可行缓存跳过候选，对比非单调条件变化后的重新评估 | 启发式规划 §5.7 |
| `heuristic-limit-08-gate.svg` | 唯一贪心路径未达收益门槛，对比可通过门槛的另一序列 | 启发式规划 §5.8 |
| `heuristic-limit-09-runtime-drift.svg` | 稳定快照计划与实时 Execute 漂移对比 | 启发式规划 §5.9 |
