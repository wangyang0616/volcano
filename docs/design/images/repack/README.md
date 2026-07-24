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
| `core-drain-flow.svg` | **图2 · drain 内部流程**（单趟动态贪心·字典序增量代价·唯一 plan）：①挑增量代价最小单元→②可腾空→③取 victims→④INV-RESCHED(FFD+best-fit+回溯)→⑤预算→⑥提交并记入已破组→⑦动态重选→⑧freed≥Min→⑨输出唯一 plan；右列关键能力 + 底部"搭便车"（B 先腾）例子 | 整理算法详解 |
| `core-unit-cost.svg` | **图3 · 选单元=字典序增量代价**：每提交后重算每个候选单元的三元组（①增量破组受损卡[已破组记0] → ②搬走卡 → ③搬走 pod），逐位比较取最小；示例 n1(0,2,1)&lt;n3(0,4,2)&lt;n2(16,4,1) 选 n1；含动态重算/搭便车来源说明 | 整理算法详解 |
