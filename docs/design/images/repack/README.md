# Repack 设计配图

本目录保存 [Repack Proposal](../../repack-runtime-defragmentation.md) 和 [Repack 技术设计](../../repack-design.md) 使用的 SVG。面向用户的场景图保存在 `docs/images/repack/`，并由 [Repack 用户指南](../../../user-guide/how_to_use_repack.md) 引用。

当前主文档使用的设计图如下：

| 文件 | 用途 |
|---|---|
| `repack-engine-architecture.svg` | Engine、Action、Plugin、Planner 与 Scheduler Framework 架构 |
| `repack-end-to-end.svg` | DryRun/Execute 端到端组件交互 |
| `core-invocation.svg` | Action 到 Lazy Drain Planner 的调用链 |
| `gang-damage-stepfn.svg` | Gang `MinAvailable` 受损资源阶跃模型 |
| `heuristic-limit-05-normalization.svg` | 每轮相对评分与全局效用函数的边界 |

其他 SVG 为早期评审和演进方向的辅助素材，不作为当前设计事实来源。主文档中的文字、公式和状态机优先于历史图片中的旧字段或旧算法名称。
