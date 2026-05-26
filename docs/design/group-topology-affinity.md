# PodGroup / SubGroup 组间拓扑亲和设计（group-topology-affinity）

| 项 | 内容 |
|----|------|
| Status | Draft |
| 插件 | `group-topology-affinity`（组间）+ `network-topology-aware`（组内） |
| 关联设计 | [Network Topology Aware Scheduling](./Network%20Topology%20Aware%20Scheduling.md)、[Preempt Action Support Topology](./preempt-action-support-topology.md) |

## 文档结构

| 章节 | 内容 |
|------|------|
| [概述](#概述) | 背景、设计目标、范围 |
| [用户场景与能力对照](#用户场景与能力对照) | 实例 1–7、配置示例 |
| [API Design](#api-design) | PodGroup 字段、类型、Webhook 要点 |
| [Plugin Architecture](#plugin-architecture) | 插件职责、gradient 聚合、资源预筛 |
| [架构与时序图](#架构与时序图) | 端到端流程与时序 |
| [竞品与标准对齐](#竞品与标准对齐) | 友商洞察、用法示例、字段映射 |
| [Implementation Phases](#implementation-phases) | 分阶段交付 |
| [Validation Rules (Webhook)](#validation-rules-webhook) | Admission 校验 |
| [Status (Optional)](#status-optional) | 不可满足 Condition |
| [References](#references) | 外部文档链接 |

> 正文中的对象与插件名均使用 **全称**（如 PodGroup、SubGroup、`network-topology-aware`），不使用 PG、GTA 等缩写。

# 概述

## 背景与问题

Volcano 在 [Network Topology Aware Scheduling](./Network%20Topology%20Aware%20Scheduling.md) 中已具备：

- 基于 **HyperNode 树** 的多级网络拓扑；
- **PodGroup / SubGroup** Gang 与 `subGroupPolicy`、`matchLabelKeys` 拆 SubJob；
- **`networkTopology`**：在 Job 或 SubJob **内部** 做域内聚合（不跨 tier）。

上述能力解决的是「**一组 Pod 聚在同一拓扑域**」。生产中还普遍存在另一类诉求：**多组之间** 要「尽量靠近」或「刻意拆开」——且分组单位是 **PodGroup（多副本 instance）** 或 **同一 PodGroup 内的不同 SubGroup（如 prefill / decode、多分片）**，而不是单个 Pod 的 `podAffinity`。

| 现状缺口 | 典型后果 |
|----------|----------|
| 无声明式 **跨 PodGroup** 拓扑互斥/亲和 | 多个 inference instance 落在同一超节点，单点故障拖垮全部在线副本 |
| 无声明式 **同 PodGroup 内跨 SubGroup** 拓扑关系 | Prefill-Decode 分片无法「分片分机柜、整体共超节点」；只能靠人工拆多个 PodGroup 或 Pod 级规则凑 |
| 组内与组间规则混在一处 | 用 Pod `topologySpread` / 注解难以表达 Gang + 多级 HyperNode + SubJob 语义，运维成本高 |

本设计在 **不替代** 现有 `network-topology-aware` 的前提下，补齐 **组间（inter-group）** 拓扑调度能力：在 PodGroup 上声明 **跨 PodGroup** 与 **同 PodGroup 内跨 SubGroup** 的亲和/反亲和。配置示例见 [#用户场景与能力对照](#用户场景与能力对照)；实现与流程见 [Plugin Architecture](#plugin-architecture)、[#架构与时序图](#架构与时序图)。

## 设计目标

| 目标 | 说明 |
|------|------|
| **组间可声明** | 用 `topologyAffinity`、`subGroupTopologyAffinity` 表达跨组拓扑关系，对齐 HyperNode `tierName`（`separationTierName`） |
| **作用域清晰** | 跨 PodGroup 与跨 SubGroup 分字段；组内仍用 `networkTopology`，不与组间混用 |
| **Hard / Soft 可区分** | 组间 hard/soft 由 `required` / `preferred` 列表表达（对齐 K8s PodAffinity）；`networkTopology` 单独使用 `mode` |
| **与 network-topology-aware 可组合** | 新插件 `group-topology-affinity` 负责组间；hard 拓扑 gradient **多插件交集** 后统一分层；容量在 allocate **资源预筛** |
| **可验证、可演进** | Admission Webhook 校验；API 以 optional 字段 additive 扩展；Phase 1 交付主路径（见 [Implementation Phases](#implementation-phases)） |

```mermaid
flowchart TB
    subgraph L1 ["组内 · network-topology-aware"]
        A1["PodGroup / SubJob networkTopology"]
    end
    subgraph L2 ["同 PodGroup 组间 · group-topology-affinity"]
        A2["subGroupTopologyAffinity"]
    end
    subgraph L3 ["跨 PodGroup 组间 · group-topology-affinity"]
        A3["topologyAffinity"]
    end
    R["HyperNode 树 + Domain_T"] --> L1
    R --> L2
    R --> L3
```

## 范围

### 目标内

- **PodGroup API**（作用域分离）：
  - `topologyAffinity`：**跨 PodGroup**（`topologyGroup` / `podGroupSelector`）
  - `subGroupTopologyAffinity`：**同一 PodGroup 内、跨 `subGroupPolicy`（SubJob）**；**不**跨 PodGroup
- **插件**：`group-topology-affinity`（组间 hard gradient + soft order）+ 现有 `network-topology-aware`（组内 Gang / binpack）
- **Framework**：拓扑类 `HyperNodeGradient` 多插件 **集合交集 + 按 tier 重分层**（不含资源判断）
- **allocate**：`filterGradientsByMinResource`（与 `HyperNodeGradientFor*Fn` 解耦）
- **Admission Webhook** 校验
- **Phase 1**：hard（`required` → gradient 剪枝）+ soft（`preferred` + `weight` → order 打分）

### 目标外

| 项 | 说明 |
|----|------|
| 组内 Gang / 不跨 tier | 继续由 `PodGroupSpec.networkTopology`、`subGroupPolicy[].networkTopology` + network-topology-aware 承担 |
| SubJob 内逐 Pod spread | 使用组内 `networkTopology` 或 K8s Pod 拓扑，不在此设计扩展 |
| 跨 Namespace 的 SubGroup 对等 | 不支持 |
| 用 `topologyGroup` 表达同 PodGroup 内 prefill/decode | 应使用 `subGroupTopologyAffinity` |
| `preempt` / `backfill` 拓扑一致 | Phase 2+ |
| Batch Job API 与 `PartitionPolicy` 同步 | Phase 2+，可后续同路径接入 |
| PodGroup `TopologyUnsatisfiable` Condition | Phase 2（可选，见 [Status](#status-optional)） |

# 用户场景与能力对照

本章按 **场景 → 业务价值 → 配置能力 → HyperNode 调度结果** 组织，便于对照选型。所有图示使用同一套 **多级 HyperNode 树**（与集群 CR 一致；`tierName` 以实际为准，下文用 `supernode` / `cabinet` 作示例）。

## 三类能力与作用域

| 能力 | 配置位置 | 作用域 | 解决什么问题 |
|------|----------|--------|--------------|
| Job / PodGroup 级域内聚合 | `PodGroupSpec.networkTopology` | **整个 PodGroup（Job）** | 全 Job 不跨越某 tier（如共 **一个 supernode**） |
| SubJob 内 Gang | `subGroupPolicy[].networkTopology` | 同一 SubJob 内 Pod | 一组 Pod **聚在** 某层拓扑域（如单机柜） |
| 组间互斥 / 共域（同 PodGroup） | `subGroupTopologyAffinity` | 不同 `subGroupPolicy` 拆出的 SubJob 之间 | 分片 **互斥**、角色间 **共域**（见实例 4） |
| 跨 PodGroup | `topologyAffinity` | 不同 PodGroup（多 instance） | 多副本服务 **故障域** 隔离 |

> `separationTierName` 必须等于 HyperNode `spec.tierName`，见 [#hypernode-层级与-separationtiername](#hypernode-层级与-separationtiername)。

## 如何阅读调度结果图

图中 **方框 = HyperNode**（按 `tierName` 分层），**最底层 = Node / Pod**；**虚线框 = 同一 `Domain_T`**（在该 tier 上被视为同一调度域）。

```mermaid
flowchart TB
    ROOT["Cluster 根"]
    ROOT --> SN["HyperNode · tierName=supernode<br/>Domain_supernode 在此层比较"]
    SN --> C1["HyperNode · tierName=cabinet<br/>Domain_cabinet 在此层比较"]
    SN --> C2["HyperNode · tierName=cabinet"]
    C1 --> N1["Node → Pod"]
    C2 --> N2["Node → Pod"]
```

**图例：**

| 符号 | 含义 |
|------|------|
| 同色 cabinet 下多个 Pod | 组内 Gang（`networkTopology`） |
| 多个 cabinet 同属一个 supernode | 见 [实例 4](#实例-4分布式-prefill-decode-推理推荐)（方式一或方式二） |
| 同一 supernode 下不同 cabinet | `subGroupAntiAffinity` @ cabinet（policy 内分片互斥） |
| 两个 supernode 各放一个 PodGroup | `topologyAffinity` @ supernode（跨 instance） |

## 场景总览

| 编号 | 场景 | 业务价值 | 使用能力 | 默认推荐 |
|------|------|----------|----------|----------|
| [实例 1](#实例-1训练-job组内-gang) | 训练 / 同步训练 Worker Gang | 机内/柜内 NVLink 或高带宽域训练 | 仅 `networkTopology` | 训练默认 |
| [实例 2](#实例-2多-inference-instance故障隔离) | 多推理 instance 并行 | 单超节点故障不拖垮全部在线副本 | `topologyAffinity` | 多副本 serving |
| [实例 3](#实例-3单模板在线推理) | 无 Prefill-Decode 拆分 | 配置简单、整组 Gang | 仅 `networkTopology` | 小模型推理 |
| [实例 4](#实例-4分布式-prefill-decode-推理推荐) | 4×prefill + 2×decode 分片 | 分片故障隔离 + Prefill-Decode 低延迟（共超节点） | `subGroupPolicy` + `matchLabelKeys` + 分片反亲和；共超节点二选一（见实例 4） | **Prefill-Decode 生产默认** |
| [实例 5](#实例-5多-instance--pd-组合) | 实例 4 + 多 instance | 容量扩展 + 双层故障域 | `topologyAffinity` + 实例 4 | 生产全栈 |
| [实例 6](#实例-6可选prefill-与-decode-跨角色分机柜) | prefill 与 decode 强制分柜 | 角色级资源/故障硬隔离（牺牲局部性） | 跨 policy `subGroupAntiAffinity` | **仅特殊需求** |
| [实例 7](#实例-7subgroup-软性反亲和可选) | Prefill-Decode 分片 **尽量** 分机柜 | 资源紧时仍可调度 | `subGroupAntiAffinity.preferred` + `weight` | 非关键 SLO |

**不推荐作为 Prefill-Decode 默认：** prefill 与 decode **跨角色分机柜**（实例 6）——与「共超节点、降低 Prefill-Decode 通信代价」通常相反；**推荐** policy 内 prefill↔prefill、decode↔decode 分机柜（实例 4）。

---

## 场景实例

> 每个实例包含：**场景与价值** → **配置要点** → **调度结果（HyperNode 树）** → **与其它实例差异**。

### 实例 1：训练 Job — 组内 Gang

**场景：** 单 PodGroup，多 Worker（如 4 组 × 8 GPU）在同一训练 Job 内 Gang 调度。  
**业务价值：** 同 SubJob 内 Pod 落在 **同一机柜（或更低 tier）**，提高机内/柜内通信带宽，满足 AllReduce 等集合通信。  
**能力：** `networkTopology`（**不**使用 `topologyAffinity` / `subGroupTopologyAffinity`）。

| 配置选择 | 适用 |
|----------|------|
| `subGroupPolicy[].networkTopology` @ cabinet | 多 SubJob / 分区，每区 8 Pod 聚柜 |
| `PodGroupSpec.networkTopology` @ supernode | 整 Job 共超节点（无分片互斥时） |

```yaml
spec:
  minMember: 32
  # 可选：整 Job 不跨 supernode
  # networkTopology: { mode: hard, highestTierName: supernode }
  subGroupPolicy:
    - name: workers
      subGroupSize: 8
      networkTopology:
        mode: hard
        highestTierName: cabinet
```

**调度结果（HyperNode 树）：** 每个 SubJob（8 Pod）占 **一个** cabinet，组内 Pod 不跨柜。

```mermaid
flowchart TB
    ROOT[Cluster]
    ROOT --> SN[supernode SN-train]
    SN --> C1["cabinet-1 · SubJob workers-0<br/>8 Pods Gang"]
    SN --> C2["cabinet-2 · SubJob workers-1<br/>8 Pods Gang"]
    SN --> C3["cabinet-3 · SubJob workers-2"]
    SN --> C4["cabinet-4 · SubJob workers-3"]
    C1 --> N1[Nodes]
```

**差异：** 无组间拓扑 API；若需多 Job 互斥，另见实例 2。

---

### 实例 2：多 inference instance — 故障隔离

**场景：** 同一模型 `llama-70b` 起 3 个 PodGroup（instance-0/1/2），同时 serving。  
**业务价值：** 任意 **单个超节点故障** 只影响一个 instance，其余 instance 仍可服务。  
**能力：** `topologyAffinity.podGroupAntiAffinity` @ `supernode` + `topologyGroup` label。

```yaml
metadata:
  labels:
    volcano.sh/topology-group: llama-70b-serving
spec:
  topologyAffinity:
    podGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - topologyGroup: llama-70b-serving
          separationTierName: supernode
```

**调度结果（HyperNode 树）：** 比较在 **PodGroup 级** `Domain_supernode`；三个 instance 落在 **三个不同** supernode。

```mermaid
flowchart TB
    ROOT[Cluster]
    ROOT --> SN0["supernode SN-A<br/>PodGroup instance-0"]
    ROOT --> SN1["supernode SN-B<br/>PodGroup instance-1"]
    ROOT --> SN2["supernode SN-C<br/>PodGroup instance-2"]
    SN0 --> C0A[cabinet · Pods]
    SN1 --> C1A[cabinet · Pods]
    SN2 --> C2A[cabinet · Pods]
```

**差异：** 约束 **PodGroup 之间**；instance 内 Prefill-Decode 拓扑见实例 4。

---

### 实例 3：单模板在线推理

**场景：** 一种 Pod 模板，无 prefill/decode 拆分，整组副本 Gang。  
**业务价值：** 配置成本最低；适合无 PD、无分片的在线推理。  
**能力：** `PodGroupSpec.networkTopology` 和/或 `subGroupPolicy[].networkTopology`（二选一或组合）。

```yaml
spec:
  minMember: 8
  networkTopology:
    mode: hard
    highestTierName: cabinet
  # 或 subGroupPolicy:
  #   - name: infer
  #     subGroupSize: 8
  #     networkTopology: { mode: hard, highestTierName: cabinet }
```

**调度结果（HyperNode 树）：**

```mermaid
flowchart TB
    ROOT[Cluster]
    ROOT --> SN[supernode SN-1]
    SN --> CAB["cabinet-1 · 全部 8 Pods"]
    CAB --> N[Nodes]
```

**差异：** 仅 1 条 policy 且无组间诉求时，**不要**配 `subGroupTopologyAffinity`。

---

### 实例 4：分布式 Prefill-Decode 推理（推荐）

**场景：** 一个 inference instance：4 个 prefill 分片（各 8 Pod）+ 2 个 decode 分片（各 6 Pod）；用 **`prefill` / `decode` 两条 `subGroupPolicy`** + **`matchLabelKeys`** 表达分片，**不要**为每个分片单独建 policy 名（不写 `prefill-0` 等）。

**业务目标（两种方式一致）：**

| 约束 | 对业务的意义 |
|------|----------------|
| 同角色各分片 **不同机柜** | 单柜故障不会打掉全部 prefill 或全部 decode |
| prefill + decode **落在同一超节点** | Prefill-Decode 跨阶段流量尽量在超节点内，降低时延 |
| prefill 与 decode **不强制分机柜** | 允许某 decode 与某 prefill 同柜，调度更灵活 |
| 每个分片内 Pod **同柜 Gang** | 分片内 8/6 Pod 仍走高速域 |

**共同配置（两种方式都要写）：** `subGroupPolicy`（含 `matchLabelKeys`、组内 `networkTopology` @ cabinet）+ **`subGroupAntiAffinity`**（prefill↔prefill、decode↔decode 分机柜）。  
**仅「共超节点」的写法二选一**（见下）。

---

#### 方式一：在 PodGroup 上声明「整个推理实例共超节点」

适合：按 **instance / PodGroup** 管理资源域，希望顶层 YAML 一眼看出「这一副本不跨超节点」。

```yaml
spec:
  minMember: 44
  networkTopology:
    mode: hard
    highestTierName: supernode
  subGroupPolicy:
    - name: prefill
      labelSelector:
        matchLabels: { volcano.sh/role: prefill }
      matchLabelKeys: [volcano.sh/shard-id]
      subGroupSize: 8
      minSubGroups: 4
      networkTopology: { mode: hard, highestTierName: cabinet }
    - name: decode
      labelSelector:
        matchLabels: { volcano.sh/role: decode }
      matchLabelKeys: [volcano.sh/shard-id]
      subGroupSize: 6
      minSubGroups: 2
      networkTopology: { mode: hard, highestTierName: cabinet }
  subGroupTopologyAffinity:
    subGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - subGroupSelector:
            matchSubGroupPolicyNames: [prefill]
          antiSubGroupSelector:
            matchSubGroupPolicyNames: [prefill]
          separationTierName: cabinet
        - subGroupSelector:
            matchSubGroupPolicyNames: [decode]
          antiSubGroupSelector:
            matchSubGroupPolicyNames: [decode]
          separationTierName: cabinet
```

---

#### 方式二：在组间拓扑里声明「prefill 与 decode 共超节点」

适合：习惯在 **`subGroupTopologyAffinity`** 里集中写 Prefill-Decode 的 **角色间关系**（分片互斥 + 共超节点都在同一节）。

```yaml
spec:
  minMember: 44
  subGroupPolicy:
    - name: prefill
      labelSelector:
        matchLabels: { volcano.sh/role: prefill }
      matchLabelKeys: [volcano.sh/shard-id]
      subGroupSize: 8
      minSubGroups: 4
      networkTopology: { mode: hard, highestTierName: cabinet }
    - name: decode
      labelSelector:
        matchLabels: { volcano.sh/role: decode }
      matchLabelKeys: [volcano.sh/shard-id]
      subGroupSize: 6
      minSubGroups: 2
      networkTopology: { mode: hard, highestTierName: cabinet }
  subGroupTopologyAffinity:
    subGroupAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - matchSubGroupPolicyNames: [prefill, decode]
          separationTierName: supernode
    subGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - subGroupSelector:
            matchSubGroupPolicyNames: [prefill]
          antiSubGroupSelector:
            matchSubGroupPolicyNames: [prefill]
          separationTierName: cabinet
        - subGroupSelector:
            matchSubGroupPolicyNames: [decode]
          antiSubGroupSelector:
            matchSubGroupPolicyNames: [decode]
          separationTierName: cabinet
```

---

#### 两种方式：业务上的相同点与不同点

| | 说明 |
|---|------|
| **相同点** | 目标拓扑一致：6 个分片占 6 个机柜，且 **同属一个超节点**；分片内 Gang、分片间互斥、prefill/decode 不强制分柜；**分片互斥与组内同柜的 YAML 完全相同**，与「共超节点」写法无关（见下图）。 |
| **不同点 · 配置意图** | **方式一**：整份 PodGroup（推理 instance）不跨超节点。**方式二**：仅 `matchSubGroupPolicyNames` 点名的 policy（本例 prefill + decode）共超节点。 |
| **不同点 · 适用范围** | **方式一** 约束 **本 PodGroup 内全部 workload**（若以后在同一 PodGroup 里增加其它 `subGroupPolicy`，默认也受「整实例不跨超节点」约束）。**方式二** 只约束 affinity term 里点名的 policy（本例为 `[prefill, decode]`）；未写进 term 的其它 policy **不受这条共超节点约束**。 |
| **不同点 · 配置习惯** | **方式一** 共超节点写在 `spec` 顶层，与实例 3 等「整组 Gang」风格一致。**方式二** 共超节点与分片互斥同在 `subGroupTopologyAffinity`，便于只维护一块「组间规则」。 |
| **选用建议** | 本场景仅 prefill/decode、且希望表达 **整实例边界** 时，**更推荐方式一**；若团队规范要求所有组间拓扑只写在 `subGroupTopologyAffinity`，可用 **方式二**，但 **不要与方式一重复配置** 同一超节点约束。 |

**预期部署形态（两种方式相同）：**

```mermaid
flowchart TB
    ROOT[Cluster]
    ROOT --> SN["supernode SN-1 · 本 inference instance"]
    SN --> PA["cabinet-A · prefill 分片 0 · 8 Pods"]
    SN --> PB["cabinet-B · prefill 分片 1 · 8 Pods"]
    SN --> PC["cabinet-C · prefill 分片 2 · 8 Pods"]
    SN --> PD["cabinet-D · prefill 分片 3 · 8 Pods"]
    SN --> EA["cabinet-E · decode 分片 0 · 6 Pods"]
    SN --> EB["cabinet-F · decode 分片 1 · 6 Pods"]
```

| 在集群里看到 | 配置含义 |
|--------------|----------|
| 每个机柜内 8 或 6 个 Pod | 各 `subGroupPolicy.networkTopology` @ cabinet（分片内 Gang） |
| 4 个 prefill 机柜互不相同 | `subGroupAntiAffinity`：两侧均为 `[prefill]` |
| 2 个 decode 机柜互不相同 | `subGroupAntiAffinity`：两侧均为 `[decode]` |
| 全部在 SN-1 下 | 方式一：`PodGroup.networkTopology`；方式二：`subGroupAffinity` `[prefill, decode]` @ supernode |
| decode 可与某 prefill 同柜 | **未配置** prefill 与 decode 之间的分机柜规则 |

**填写提醒：** `matchSubGroupPolicyNames` 只写 policy 名 **`prefill` / `decode`**，不要写 `prefill-0` 等分片后缀。

---

### 实例 5：多 instance + Prefill-Decode 组合

**场景：** 实例 4 × N 个 PodGroup。  
**业务价值：** 外层超节点故障隔离 + 内层分片机柜隔离与共超节点 PD。  
**能力：** `topologyAffinity` + 实例 4 全部配置。

```yaml
metadata:
  labels:
    volcano.sh/topology-group: llama-70b-serving
spec:
  topologyAffinity:
    podGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - topologyGroup: llama-70b-serving
          separationTierName: supernode
  # subGroupPolicy + 共超节点 + subGroupAntiAffinity：同实例 4（推荐方式一）
```

**调度结果（HyperNode 树）：**

```mermaid
flowchart TB
    ROOT[Cluster]
    ROOT --> SN0["supernode SN-A · instance-0"]
    ROOT --> SN1["supernode SN-B · instance-1"]
    SN0 --> P0A[cabinet prefill/decode 分片…]
    SN1 --> P1A[cabinet prefill/decode 分片…]
```

**差异：** `topologyAffinity` 保证 SN-A ≠ SN-B；**每个 supernode 内部** 复现实例 4 的六柜结构。

---

### 实例 6（可选）：prefill 与 decode **跨角色** 分机柜

**场景：** 在实例 4 之外，强制 prefill 柜组与 decode 柜组 **不相交**。  
**业务价值：** 角色级 GPU/TOR 硬隔离、合规分域；**代价**是跨角色通信更易跨柜。  
**何时用：** 运维明确要求；**非 Prefill-Decode 默认**（与降低 Prefill-Decode 延迟常冲突）。  
**能力：** 共 supernode 配置同 [实例 4](#实例-4分布式-prefill-decode-推理推荐)；另增跨角色 `subGroupAntiAffinity`（prefill vs decode 分机柜）。

```yaml
spec:
  networkTopology:
    mode: hard
    highestTierName: supernode
  subGroupTopologyAffinity:
    subGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - subGroupSelector:
            matchSubGroupPolicyNames: [prefill]
          antiSubGroupSelector:
            matchSubGroupPolicyNames: [decode]
          separationTierName: cabinet
```

**调度结果对比（HyperNode 树）：**

```mermaid
flowchart LR
    subgraph I4 ["实例 4 默认"]
        direction TB
        SN4[supernode]
        SN4 --> P4[prefill 分片 · 多 cabinet]
        SN4 --> D4[decode 分片 · 可与 prefill 同柜域]
    end
    subgraph I6 ["实例 6 可选"]
        direction TB
        SN6[supernode]
        SN6 --> BP[cabinet 区 P · 仅 prefill]
        SN6 --> BD[cabinet 区 D · 仅 decode]
    end
```

**差异：** 实例 6 **叠加**在实例 4 上；多数部署仅保留 policy 内 `[prefill]` / `[decode]` 互斥即可。

---

### 实例 7：SubGroup 软性反亲和（可选）

**场景：** 与 [实例 4](#实例-4分布式-prefill-decode-推理推荐) 相同的 4 Prefill 分片 + 2 Decode 分片 Prefill-Decode 布局，但机柜资源紧张：希望 prefill / decode **各分片尽量落在不同机柜**，**若做不到也不要阻塞调度**。  
**业务价值：** 在故障隔离与上线率之间折中——有柜可分时仍打散分片；无柜可分时允许临时同柜，避免 PodGroup 长期 Pending。

#### 与实例 4（`required` 反亲和）的对比

| | 实例 4（`requiredDuringSchedulingIgnoredDuringExecution`） | 实例 7（`preferredDuringSchedulingIgnoredDuringExecution` + `weight`） |
|---|-------------------------------------|--------------------------------------|
| **业务语义** | 分片 **必须** 分机柜，否则不满足 | 分片 **优先** 分机柜，不满足仍可调度 |
| **适用** | 生产默认、SLO 要求分片级故障域 | 灰度、扩容、机柜余量不足、非关键批推理 |
| **共超节点** | 仍建议方式一 `PodGroup.networkTopology` @ supernode（与实例 4 相同） | 同左 |

#### 配置示例

在实例 4 **方式一** 基础上，将 `subGroupAntiAffinity` 的 `required` 改为 `preferred`，并为 term 设置 `weight`（越大表示「避开同柜」的偏好越强）：

```yaml
spec:
  minMember: 44
  networkTopology:
    mode: hard
    highestTierName: supernode
  subGroupPolicy:
    - name: prefill
      labelSelector:
        matchLabels: { volcano.sh/role: prefill }
      matchLabelKeys: [volcano.sh/shard-id]
      subGroupSize: 8
      minSubGroups: 4
      networkTopology: { mode: hard, highestTierName: cabinet }
    - name: decode
      labelSelector:
        matchLabels: { volcano.sh/role: decode }
      matchLabelKeys: [volcano.sh/shard-id]
      subGroupSize: 6
      minSubGroups: 2
      networkTopology: { mode: hard, highestTierName: cabinet }
  subGroupTopologyAffinity:
    subGroupAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          term:
            subGroupSelector:
              matchSubGroupPolicyNames: [prefill]
            antiSubGroupSelector:
              matchSubGroupPolicyNames: [prefill]
            separationTierName: cabinet
        - weight: 100
          term:
            subGroupSelector:
              matchSubGroupPolicyNames: [decode]
            antiSubGroupSelector:
              matchSubGroupPolicyNames: [decode]
            separationTierName: cabinet
```

> **说明：** 软性反亲和 **仅** 通过 `preferredDuringSchedulingIgnoredDuringExecution` + `weight` 表达，**不要** 在 term 上再写 `mode: soft`（与 `required`/`preferred` 重复）。`term` 内只需 `subGroupSelector`、`antiSubGroupSelector`、`separationTierName`（或 `separationTier`）。

#### 预期行为（业务视角）

| 集群状况 | 典型结果 |
|----------|----------|
| 超节点内 **≥6 个可用机柜** | 与实例 4 相近：6 分片各占一柜（调度器优先选择「与已放置分片不同柜」的候选） |
| 可用机柜 **不足 6 个** | 仍可能调度成功：部分分片 **同柜** 放置，整体得分偏低；不满足 hard 失败条件 |
| 仅要求共超节点 | 由 `networkTopology` @ supernode 保证；soft 反亲和 **不替代** 共超节点 hard 约束 |

```mermaid
flowchart LR
    subgraph ideal ["优先达到（与实例 4 一致）"]
        SN1[supernode SN-1]
        SN1 --> C1[cabinet-A]
        SN1 --> C2[cabinet-B]
        SN1 --> C6[cabinet-F · 六柜各一分片]
    end
    subgraph fallback ["机柜不足时的可接受结果"]
        SN2[supernode SN-1]
        SN2 --> CX[cabinet-X · 多分片同柜]
    end
```

**填写提醒：**

- policy 内互斥：两侧 selector 仍写 **同名** `[prefill]` 或 `[decode]`（与实例 4 相同），**不要**写成 prefill vs decode。
- **勿** 将共超节点改为 soft：Prefill-Decode 低延迟路径通常仍对 supernode 使用 **hard** `networkTopology`（或实例 4 方式二 hard `subGroupAffinity`）。
- 可与 required 类约束 term **混用**（例如 supernode 用 `required` hard，分机柜仅用 `preferred` soft）；Webhook 校验 tier 关系时以 **required** 类 term 为准。

**跨 PodGroup 的 soft 反亲和**（多 instance 尽量分超节点、但不硬失败）可类比为 `topologyAffinity.podGroupAntiAffinity.preferred`，思路与上表相同，场景见 [实例 2](#实例-2多-inference-instance故障隔离)。

---

# API Design

本章定义 PodGroup API 与能力边界。场景 YAML 见 [#用户场景与能力对照](#用户场景与能力对照)；HyperNode 层级填写见 [#hypernode-层级与-separationtiername](#hypernode-层级与-separationtiername)。

## PodGroupSpec 新增字段

```go
type PodGroupSpec struct {
    // ... existing fields ...

    // TopologyAffinity expresses topology affinity/anti-affinity between THIS PodGroup and OTHER PodGroups
    // (selected by topologyGroup label or podGroupSelector). Evaluated at Job / HyperNodeGradientForJob scope.
    // Does NOT apply to relationships between subGroupPolicy entries within the same PodGroup;
    // use SubGroupTopologyAffinity for that.
    // +optional
    TopologyAffinity *PodGroupTopologyAffinitySpec `json:"topologyAffinity,omitempty"`

    // SubGroupTopologyAffinity expresses topology affinity/anti-affinity between SubGroupPolicies
    // defined in THIS PodGroup's subGroupPolicy list only. Evaluated per SubJob at
    // HyperNodeGradientForSubJob scope; peers are other SubJobs of the same JobInfo (same PodGroup UID).
    // Cannot reference PodGroups in other namespaces or other topologyGroups.
    // Requires subGroupPolicy; ignored (webhook reject) if subGroupPolicy is empty.
    // +optional
    SubGroupTopologyAffinity *SubGroupTopologyAffinitySpec `json:"subGroupTopologyAffinity,omitempty"`
}
```

## 核心类型

```go
// TopologySeparationSpec defines the HyperNode tier used as the separation/comparison boundary.
type TopologySeparationSpec struct {
    // SeparationTier: compare scheduling domains at HyperNode.spec.tier (integer).
    // Must match the numeric tier of a HyperNode layer in this cluster. Mutually exclusive with SeparationTierName.
    // +kubebuilder:validation:Minimum=0
    // +optional
    SeparationTier *int `json:"separationTier,omitempty"`

    // SeparationTierName: compare scheduling domains at HyperNode.spec.tierName (string).
    // The value MUST be identical to tierName configured on HyperNode CRs in the cluster (case-sensitive).
    // Scheduler resolves it via Session HyperNodeTierNameMap (same source as networkTopology.highestTierName).
    // Example: if cabinet HyperNodes use spec.tierName: cabinet, set separationTierName: cabinet here.
    // Mutually exclusive with SeparationTier.
    // +optional
    SeparationTierName string `json:"separationTierName,omitempty"`
    // Note: hard vs soft for topologyAffinity / subGroupTopologyAffinity is NOT expressed here.
    // Use requiredDuringSchedulingIgnoredDuringExecution (hard) vs
    // preferredDuringSchedulingIgnoredDuringExecution (soft), aligned with Kubernetes PodAffinity.
}

// NetworkTopologySpec (PodGroup / SubGroupPolicy): domain aggregation with explicit mode.
// +kubebuilder:validation:Enum=hard;soft
type NetworkTopologySpec struct {
    Mode               NetworkTopologyMode `json:"mode,omitempty"`
    HighestTierName    string              `json:"highestTierName,omitempty"`
    HighestTierAllowed *int                `json:"highestTierAllowed,omitempty"`
}
```

> `TopologySeparationSpec` 的层级字段与 HyperNode CR 的对应关系、填写步骤与示例树，见 [#hypernode-层级与-separationtiername](#hypernode-层级与-separationtiername)。

### `required` / `preferred` 与 `mode`（不重复）

| API | 如何表达 hard / soft | 是否在 term 上写 `mode` |
|-----|----------------------|-------------------------|
| `topologyAffinity` / `subGroupTopologyAffinity` | **`requiredDuringSchedulingIgnoredDuringExecution`** = 必须满足（hard）；**`preferredDuringSchedulingIgnoredDuringExecution`** = 尽量满足（soft，`weight` 越大偏好越强） | **否**（与 Kubernetes `PodAffinity` / `PodAntiAffinity` 一致） |
| `PodGroupSpec.networkTopology`、`subGroupPolicy[].networkTopology` | 字段 **`mode: hard \| soft`**（无 required/preferred 列表） | **是**（仅此两类配置使用 `mode`） |

**为何不在 term 上保留 `mode`：** 若同时在 `preferred` 列表里写 `mode: soft`，或在 `required` 里写 `mode: hard`，与列表语义重复，且可能出现 `required` + `mode: soft` 等矛盾。Webhook 对组间拓扑 term **拒绝或忽略** `separationTier` 内的 `mode` 字段。

**实现约定：** `ContainsHardCrossSubGroupTopology` / `ContainsHardCrossPodGroupTopology` 仅看是否存在 **非空 `required`** 列表；`preferred` 条目只注册 `HyperNodeOrderFn`。

### YAML 书写约定

- **组间 term**（`topologyAffinity` / `subGroupTopologyAffinity`）：下文示例为可读性将 `separationTierName` 与 selector **写在 term 同级**；与 Go 类型等价于嵌套 `separationTier: { separationTierName: ... }`。
- **组内**（`networkTopology`）：使用 `mode: hard | soft` 与 `highestTierName`，**无** `required` / `preferred` 列表。

```go
// PodGroupTopologyAffinitySpec: cross-PodGroup scope only.
type PodGroupTopologyAffinitySpec struct {
    PodGroupAntiAffinity *TopologyAntiAffinitySpec `json:"podGroupAntiAffinity,omitempty"`
    PodGroupAffinity     *TopologyAffinitySpec     `json:"podGroupAffinity,omitempty"`
}

// SubGroupTopologyAffinitySpec: intra-PodGroup, cross-subGroupPolicy scope only.
type SubGroupTopologyAffinitySpec struct {
    SubGroupAntiAffinity *SubGroupAntiAffinitySpec `json:"subGroupAntiAffinity,omitempty"`
    SubGroupAffinity     *SubGroupAffinitySpec     `json:"subGroupAffinity,omitempty"`
}

// SubGroupAntiAffinitySpec / SubGroupAffinitySpec: term lists for SubGroup peers (not PodGroup selectors).
type SubGroupAntiAffinitySpec struct {
    RequiredDuringSchedulingIgnoredDuringExecution  []SubGroupTopologyAntiAffinityTerm `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
    PreferredDuringSchedulingIgnoredDuringExecution []WeightedSubGroupTopologyAntiAffinityTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

type SubGroupAffinitySpec struct {
    RequiredDuringSchedulingIgnoredDuringExecution  []SubGroupTopologyAffinityTerm `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
    PreferredDuringSchedulingIgnoredDuringExecution []WeightedSubGroupTopologyAffinityTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

type TopologyAntiAffinitySpec struct {
    RequiredDuringSchedulingIgnoredDuringExecution  []TopologyAntiAffinityTerm `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
    PreferredDuringSchedulingIgnoredDuringExecution []WeightedTopologyAntiAffinityTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

type TopologyAffinitySpec struct {
    RequiredDuringSchedulingIgnoredDuringExecution  []TopologyAffinityTerm `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
    PreferredDuringSchedulingIgnoredDuringExecution []WeightedTopologyAffinityTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

// Cross-PodGroup terms
type TopologyAntiAffinityTerm struct {
    PodGroupSelector    *metav1.LabelSelector `json:"podGroupSelector,omitempty"`
    TopologyGroup       string                `json:"topologyGroup,omitempty"`
    NamespaceSelector   *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
    SeparationTier      TopologySeparationSpec `json:"separationTier"`
}

type TopologyAffinityTerm struct {
    PodGroupSelector  *metav1.LabelSelector `json:"podGroupSelector,omitempty"`
    TopologyGroup     string                `json:"topologyGroup,omitempty"`
    NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
    SeparationTier    TopologySeparationSpec `json:"separationTier"`
}

// Cross-SubGroup terms (intra-PodGroup only).
// matchSubGroupPolicyNames ALWAYS refers to subGroupPolicy[].name (policy name), NOT shard suffixes in SubJobID.
type SubGroupTopologyAntiAffinityTerm struct {
    // SubGroupSelector: applies when the SubJob being scheduled belongs to one of these policy names.
    SubGroupSelector SubGroupSelectorSpec `json:"subGroupSelector"`
    // AntiSubGroupSelector: peer SubJobs to compare against (already placed in this PodGroup).
    AntiSubGroupSelector SubGroupSelectorSpec `json:"antiSubGroupSelector"`
    SeparationTier       TopologySeparationSpec `json:"separationTier"`
}

type SubGroupTopologyAffinityTerm struct {
    // MatchSubGroupPolicyNames: policy names (subGroupPolicy[].name). All SubJobs under ANY listed policy
    // must share Domain_T at SeparationTier (e.g. [prefill, decode] @ supernode covers 4+2 SubJobs).
    // Must list >= 2 distinct policy names.
    MatchSubGroupPolicyNames []string `json:"matchSubGroupPolicyNames"`
    SeparationTier           TopologySeparationSpec `json:"separationTier"`
}

// SubGroupSelectorSpec selects SubJobs by policy name (and optional pod labelSelector).
type SubGroupSelectorSpec struct {
    // MatchSubGroupPolicyNames: subGroupPolicy[].name. When matchLabelKeys splits one policy into multiple
    // SubJobs (SubJobID = <JobID>/<name>-<matchValues>), ALL such SubJobs match this selector.
    MatchSubGroupPolicyNames []string `json:"matchSubGroupPolicyNames,omitempty"`
    LabelSelector            *metav1.LabelSelector `json:"labelSelector,omitempty"`
}

type WeightedSubGroupTopologyAntiAffinityTerm struct {
    Weight int32                              `json:"weight"`
    Term   SubGroupTopologyAntiAffinityTerm   `json:"term"`
}

type WeightedSubGroupTopologyAffinityTerm struct {
    Weight int32                            `json:"weight"`
    Term   SubGroupTopologyAffinityTerm    `json:"term"`
}
```

## subGroupPolicy.name 与 SubJob（`matchLabelKeys`）

一条 `subGroupPolicy` 可对应 **多个 SubJob**，无需为每个分片单独建 policy。规则与 [Network Topology Aware Scheduling](./Network%20Topology%20Aware%20Scheduling.md#SubJobID) 一致：

| 配置 | 效果 |
|------|------|
| `name: prefill` + `labelSelector`（角色） | 选中所有 prefill Pod |
| `matchLabelKeys: [volcano.sh/shard-id]` | 按 shard 标签值拆成多个 SubJob |
| `subGroupSize: 8` | 每个 SubJob 内 8 Pod 为一组 Gang |
| `minSubGroups: 4` | 至少 4 个这样的 SubJob 才触发 prefill 侧调度 |

**SubJobID 示例：** `my-job/prefill-0`、`my-job/prefill-1`、…（`PolicyName` = `prefill`，`MatchValues` 来自 label）。

**`subGroupTopologyAffinity` 中的名字：** 只写 **`prefill` / `decode`**（policy name），**不要**写 `prefill-0` 等 SubJobID 后缀。

| Term 写法 | 语义 |
|-----------|------|
| `matchSubGroupPolicyNames: [prefill, decode]`（affinity） | 所有 prefill SubJob **与** 所有 decode SubJob 共 `Domain_supernode` |
| `subGroupSelector` / `antiSubGroupSelector` 均为 `[prefill]`（antiAffinity） | **同一 policy 下任意两个不同 SubJob**（如 `prefill-0` vs `prefill-1`）`Domain_cabinet` 互异 |
| 同上，写入 `preferred` + `weight`（**不写** `mode`） | **优先** 分机柜，无法满足仍可调度（[实例 7](#实例-7subgroup-软性反亲和可选)） |
| `subGroupSelector: [prefill]`、`antiSubGroupSelector: [decode]` | **跨 policy** 的两个 SubJob 互异（实例 6） |

**实现（group-topology-affinity）：** 将 `SubJobInfo` 映射到 `policyName`（解析 `SubJobID` 或 Job 内索引）；selector 按 policyName 匹配；affinity term 对 listed policies 的 SubJob 集合求并集后比较 `Domain_T`。

> **API 可扩展性：** `SubGroupTopologyAffinitySpec` 采用 `subGroupAffinity` / `subGroupAntiAffinity` **独立子容器**；后续新组间语义应 **additive** 增加 optional 子字段或新 Term 类型，**不修改** 既有 Term 字段语义（实现与 CRD 须保持向前兼容）。

## API 命名约定

不确定用哪个字段时，先看 [#用户场景与能力对照](#用户场景与能力对照)。

组间拓扑的外层容器字段统一使用 **`*TopologyAffinity`** 后缀，对齐 Kubernetes `affinity` / `antiAffinity`（required / preferred、hard / soft）；内层子字段用作用域前缀区分对象：

| 作用域 | `PodGroupSpec` 字段 | 内层子字段 |
|--------|---------------------|------------|
| 跨 PodGroup | `topologyAffinity` | `podGroupAffinity` / `podGroupAntiAffinity` |
| 跨 SubGroup（**仅同 PodGroup**） | `subGroupTopologyAffinity` | `subGroupAffinity` / `subGroupAntiAffinity` |

> 详细能力范围、非目标与 Webhook 规则见 [#subgrouptopologyaffinity-能力范围同-podgroup跨-subgroup](#subgrouptopologyaffinity-能力范围同-podgroup跨-subgroup)。

与已有字段的边界：

| 字段 | 语义 |
|------|------|
| `PodGroupSpec.networkTopology` | **Job 级别** 域内聚合（整 Job 不跨 tier） |
| `subGroupPolicy[].networkTopology` | **组内** Gang：不跨越 `highestTierAllowed`（域内聚合） |
| `topologyAffinity` / `subGroupTopologyAffinity` | **组间**：在 `separationTier` 上同域或异域 |

Go 类型：`SubGroupTopologyAffinitySpec`（容器）与 `SubGroupTopologyAffinityTerm`（单条亲和 term）并存，与 K8s `PodAffinity` / `PodAffinityTerm` 命名方式一致。

> 曾用名 `subGroupTopologyConstraints` 已废弃，统一为 `subGroupTopologyAffinity`，与 `topologyAffinity` 对称。

## 能力边界：多层级拓扑关系

Volcano 在本特性下将拓扑关系划分为 **四个互不替代的作用域**。配置时必须先确认需求落在哪一层，再选用对应字段。

| 层级 | API | 比较对象 | 典型场景 | 调度锚点 |
|------|-----|----------|----------|----------|
| **PodGroup（Job）内** | `PodGroupSpec.networkTopology` | 整个 Job 全部 Pod / SubJob | Job 级别 envelope（如不跨 supernode） | `HyperNodeGradientForJobFn`（network-topology-aware）→ `allocateForJob` |
| **SubGroup 内** | `subGroupPolicy[].networkTopology` | 同一 SubJob 内 Pod / Task | 组内 Gang、不跨机柜 | `HyperNodeGradientForSubJobFn`（network-topology-aware）+ `subGroupSize` |
| **同 PodGroup、跨 SubGroup** | `subGroupTopologyAffinity` | 不同 policy 的 **SubJob** | **互斥** / **共域**（`subGroupAntiAffinity` / `subGroupAffinity`） | `HyperNodeGradientForSubJobFn`（group-topology-affinity） |
| **跨 PodGroup** | `topologyAffinity` | 其它 PodGroup | 多 instance 占不同超节点 | `HyperNodeGradientForJobFn`（group-topology-affinity）+ `TopologyOccupancyIndex` |

```mermaid
flowchart TB
    subgraph PodGroupNode["PodGroup (一个 inference instance)"]
        direction TB
        SG1["subGroupPolicy: prefill<br/>networkTopology 组内"]
        SG2["subGroupPolicy: decode<br/>networkTopology 组内"]
        SGA["subGroupTopologyAffinity<br/>prefill ↔ decode"]
        SG1 --- SGA
        SG2 --- SGA
    end
    PodGroup2["其它 PodGroup (instance-1)"]
    PGA["topologyAffinity<br/>podGroupAntiAffinity @ supernode"]
    PodGroupNode -.->|跨 PodGroup，仅 topologyAffinity| PGA
    PodGroup2 -.-> PGA
```

---

## `topologyAffinity` 能力范围（跨 PodGroup）

**做什么：** 描述 **本 PodGroup** 与 **集群内其它 PodGroup** 在 `separationTier` 上的同域（`podGroupAffinity`）或异域（`podGroupAntiAffinity`）。

**能力范围（In scope）：**

- 通过 `metadata.labels[volcano.sh/topology-group]` 或 `podGroupSelector` / `namespaceSelector` 选中对端 PodGroup；
- Hard / soft；占用索引 `TopologyOccupancyIndex` 记录已调度 PodGroup 的 `Domain_T`；
- 在 **Job 级别** `allocateForJob` 之前参与 `HyperNodeGradientForJobFn` 剪枝。

**非目标（Out of scope）：**

- **不能**表达同一 PodGroup 内 prefill / decode 等 SubGroup 之间的关系 → 使用 `subGroupTopologyAffinity`；
- **不能**表达 SubGroup 内 Gang / `highestTierAllowed` → 使用 `subGroupPolicy[].networkTopology`。

---

## `subGroupTopologyAffinity` 能力范围（同 PodGroup、跨 SubGroup）

**做什么：** 描述 **当前 PodGroup 内**，由 `subGroupPolicy` 划分的 **多个 SubGroup（SubJob）之间** 在 HyperNode 树 `separationTier` 上的拓扑亲和或反亲和。

**核心语义：** 调度器为每个 `subGroupPolicy` 生成一个 **SubJob**；`subGroupTopologyAffinity` 约束的是这些 SubJob 的 `AllocatedHyperNode` 在 `Domain_T(·)` 上的关系，**不是** Pod 级 `podAffinity`，也 **不是** 跨 PodGroup 关系。

### 能力范围（In scope）

| 能力 | 说明 |
|------|------|
| 多个 SubJob 共超节点 | `PodGroupSpec.networkTopology` 或 `subGroupAffinity`（见 [实例 4](#实例-4分布式-prefill-decode-推理推荐)） |
| 同角色多分片机柜互斥 | `subGroupAntiAffinity` @ `cabinet`，selector 两侧为 **同名** policy（如均为 `[prefill]`） | [实例 4](#实例-4分布式-prefill-decode-推理推荐) |
| 两 **不同 policy** 的 SubJob 异域 | `subGroupAntiAffinity` + `subGroupSelector` / `antiSubGroupSelector`（policy 名 **不相交**） | [实例 6](#实例-6可选prefill-与-decode-跨角色分机柜) |
| 同一 policy 内 Pod 分机柜（非 SubJob 间） | **非本字段** | `matchLabelKeys` 拆 SubJob + policy 内 anti，或 Pod spread |
| prefill 与 decode 无要求 | 不写跨角色 `subGroupAntiAffinity` | 实例 4 仅 prefill↔prefill、decode↔decode |
| Hard / soft | Hard → `HyperNodeGradientForSubJobFn` 剪枝；Soft → `HyperNodeOrderFn`（见 [实例 7](#实例-7subgroup-软性反亲和可选)） |
| 与组内 Gang 叠加 | 各 SubGroup 仍可独立配置 `networkTopology.highestTierAllowed` |
| SubJob 调度顺序 | Hard `subGroupAntiAffinity` 时，被引用为 `subGroupSelector` 的 policy 对应 SubJob **优先** 调度（见 §10.1） |
| 部分调度 | 一方 SubJob 已分配 `AllocatedHyperNode` 后，另一方须满足 `Domain_T` 关系再选 HyperNode |

### 非目标（Out of scope）

| 非目标 | 应使用的 API / 机制 |
|--------|---------------------|
| 跨 PodGroup / 跨 instance 互斥或共域 | `topologyAffinity`（`podGroupAntiAffinity` / `podGroupAffinity`） |
| 同一 SubGroup 内 Pod 不跨 tier | `subGroupPolicy[].networkTopology` |
| 选择「任意其它 PodGroup」的 SubGroup | **不支持**；selector 仅解析本 PodGroup 的 `subGroupPolicy` |
| 跨 Namespace 的 SubGroup 对等 | **不支持** |
| 无 `subGroupPolicy` 时定义 SubGroup 间关系 | **无效**；Webhook 拒绝 |
| 用 `topologyGroup` 表达同 PodGroup 内 prefill/decode | **错误**；`topologyGroup` 仅用于跨 PodGroup |

### 前置条件

1. `spec.subGroupPolicy` **非空**，且至少 **2 条** `subGroupPolicy`（单 SubGroup 无「组间」对象，配置 `subGroupTopologyAffinity` 无意义）。
2. Term 中的 `matchSubGroupPolicyNames` / `matchSubGroupPolicyNames`（selector 内）必须 **全部出现在本 PodGroup** 的 `subGroupPolicy[].name` 中。
3. 对应 SubGroup 的 Pod 须能通过各 policy 的 `labelSelector`（及可选 `matchLabelKeys`）正确归属到 SubJob。

### 调度语义与实现锚点

| 项 | 行为 |
|----|------|
| 比较主体 | `JobInfo.SubJobs`（按 `subGroupPolicy.name` 区分），非 `JobInfo` 与其它 Job |
| Gradient 注册 | 仅当 `ContainsHardCrossSubGroupTopology(job)` 为真时，group-topology-affinity 注册 `HyperNodeGradientForSubJobFn` |
| 与 Job 级别 PodGroup 约束 | 先 `HyperNodeGradientForJobFn`（含跨 PodGroup hard）→ `allocateForJob` 选定 Job 级别 HyperNode 域 → 再对各 SubJob 应用 `subGroupTopologyAffinity` |
| Occupancy 索引 | **不**写入跨 PodGroup 索引；仅在同 PodGroup 的 SubJob 已分配域上在 Session 内做 peer 查询 |
| 忽略执行期变更 | `*IgnoredDuringExecution`：已放置 SubJob 不因 PodGroup Spec 变更被驱逐（与 PodAffinity 一致） |

### 设计考量与限制

1. **Peer 依赖与顺序：** Hard `subGroupAffinity` 要求 peer SubJob 已有 `AllocatedHyperNode`（或同轮内先调度方）；因此 `organizeJobWorksheet` 需保证至少一个 peer 先进入 `allocateForSubJob`。
2. **Tier 一致性：** 若同时配置 hard `subGroupAffinity` @ supernode 与 hard `subGroupAntiAffinity` @ cabinet，Webhook 要求 affinity 的 tier **不低于** antiAffinity 的 tier（数值更大或 tierName 更靠近根），避免逻辑矛盾。
3. **与 `topologyAffinity` 并用：** 可同时配置实例级 `topologyAffinity`；`subGroupTopologyAffinity` 仅在有 **角色间** 需求时添加（实例 6），与「角色内副本打散」（实例 4）正交。
4. **仅 HyperNode 域：** 约束在 HyperNode 树 `Domain_T` 上表达；SubJob 选定 HyperNode 后，组内 Pod 仍由 `networkTopology` + Node `predicate` 落位。
5. **Phase 1：** `preempt` / `backfill` 不保证重算 SubGroup 间拓扑；Occupancy 以 Session 内已运行 Job 为准。

### 配置反例

```yaml
# 错误：用 subGroupAntiAffinity(prefill, decode) 表达「4 个 prefill 彼此分机柜」
subGroupTopologyAffinity:
  subGroupAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - subGroupSelector:
          matchSubGroupPolicyNames: [prefill]
        antiSubGroupSelector:
          matchSubGroupPolicyNames: [decode]   # ❌ 这是角色间，不是副本内

# 正确：prefill 与 decode 无要求 → 不配上述 term
# 正确：prefill 副本间 → 见实例 4（policy 内 `[prefill]` antiAffinity + matchLabelKeys）
```

---

## Label 常量

```go
const TopologyGroupLabelKey = GroupName + "/topology-group" // volcano.sh/topology-group
```

## HyperNode 层级与 separationTierName

组间亲和/反亲和不直接使用 Node label，而是基于集群中的 **HyperNode CR** 树。用户配置的 `separationTierName`（或 `separationTier`）必须能对应到树上某一 **逻辑层级**，该层级由 HyperNode 资源定义。

### HyperNode 上定义什么

每个 HyperNode 资源（`topology.volcano.sh/v1alpha1`）在 `spec` 中描述自己处于哪一层：

| HyperNode 字段 | 含义 | 与 PodGroup 的对应 |
|----------------|------|-------------------|
| `spec.tier` | 层级 **序号**（非负整数，集群内统一递增约定，越大越靠近根） | `separationTier: <int>` |
| `spec.tierName` | 层级 **可读名称**（集群内约定，如 `cabinet`、`supernode`、`rack`） | `separationTierName: "<string>"` |

示例（集群侧，与 PodGroup 无关）：

```yaml
apiVersion: topology.volcano.sh/v1alpha1
kind: HyperNode
metadata:
  name: supernode-sn-1
spec:
  tier: 2
  tierName: supernode          # ← PodGroup 里 separationTierName 必须写 supernode
  members:
    - type: HyperNode
      selector:
        exactMatch:
          name: cabinet-a
    - type: HyperNode
      selector:
        exactMatch:
          name: cabinet-b
---
apiVersion: topology.volcano.sh/v1alpha1
kind: HyperNode
metadata:
  name: cabinet-a
spec:
  tier: 1
  tierName: cabinet            # ← separationTierName: cabinet 时，Domain 在机柜层比较
  members:
    - type: Node
      selector: { ... }
```

调度器在 Session 启动时扫描全部 HyperNode，构建 **`HyperNodeTierNameMap`**：`tierName → tier`（与 `network-topology-aware` 解析 `highestTierName` 使用同一映射）。Webhook 应校验 PodGroup 中出现的 `separationTierName` **存在于该映射**。

### 用户如何填写 separationTierName

1. **先查集群**：`kubectl get hypernodes -o custom-columns=NAME:.metadata.name,TIER:.spec.tier,TIERNAME:.spec.tierName`（或运维文档中的「拓扑层级对照表」）。
2. **再写 PodGroup**：`separationTierName` 与目标层 HyperNode 的 **`spec.tierName` 字符串完全一致**（区分大小写）；不要自造文档中未在 HyperNode 出现的名字。
3. **二选一**：`separationTier`（整数）与 `separationTierName`（字符串）**互斥**；推荐运维使用 **tierName**，与 `networkTopology.highestTierName` 一致，便于跨集群对照。
4. **与组内字段区别**：`networkTopology.highestTierAllowed` / `highestTierName` 限制 **组内** Pod 不跨层；`separationTier( Name)` 定义 **组间** 在那一层上「算同一个域」还是「必须不同域」。

| 用户写法 | 调度器解析为 |
|----------|--------------|
| `separationTierName: supernode` | 在 HyperNode 树上取 `spec.tierName == "supernode"` 的祖先节点作为域 ID |
| `separationTier: 2` | 在 HyperNode 树上取 `spec.tier == 2` 的祖先节点作为域 ID |

若集群中没有任何 HyperNode 的 `tierName: foo`，则 `separationTierName: foo` **无效**（Webhook 拒绝或调度期视为不可满足）。

### 与示例拓扑的对应

**名称以集群 HyperNode `spec.tierName` 为准。**

| 业务说法 | 示例 `separationTierName` | API |
|----------|---------------------------|-----|
| 不同 inference instance 不占同一超节点 | `supernode` | `topologyAffinity.podGroupAntiAffinity` |
| 4 个 prefill（或 decode）彼此分机柜 | `cabinet` | 实例 4：`matchLabelKeys` + policy 内 `[prefill]`/`[decode]` anti；**不是** prefill↔decode |
| prefill 与 decode **无**拓扑要求 | — | **不配置** `subGroupTopologyAffinity` |
| （可选）prefill 与 decode 分机柜且共超节点 | `cabinet` + `supernode` | 实例 6：`subGroupAntiAffinity` + `subGroupAffinity` |

---

## 语义：分离域 Domain_T

对候选 HyperNode `H` 与用户配置的分离层级（`separationTier` 或 `separationTierName`）：

```
Domain_T(H) = 从 H 沿 HyperNode 父链向上，第一个满足下列之一的祖先 HyperNode 的 metadata.name：
              · spec.tier == separationTier（整数模式）
              · spec.tierName == separationTierName（字符串模式，与 HyperNode CR 配置一致）
```

| 约束 | Hard 语义 |
|------|-----------|
| 反亲和 | `Domain_T(A) ≠ Domain_T(B)` |
| 亲和 | `Domain_T(A) == Domain_T(B)`（一方未分配时，后分配方须落入已分配域） |

与现有 `NetworkTopologySpec` 的区别：

| 字段 | 语义 | 层级来源 |
|------|------|----------|
| `highestTierAllowed` / `highestTierName`（已有） | **组内** Gang：整组 Pod 不跨越该层 | 同上 HyperNode `tier` / `tierName` |
| `separationTier` / `separationTierName`（本设计） | **组间**：在该层比较 Domain 相同或不同 | 同上，**必须与集群 HyperNode 定义一致** |

## Hard 约束优先级

1. PodGroup hard antiAffinity（跨 instance）
2. SubGroup hard antiAffinity（**SubJob 之间**，含实例 4 policy 内分片互斥）
3. SubGroup hard affinity（**不同 policy / SubJob 之间** 共域，如实例 6）
4. SubGroup / Job hard `networkTopology`（组内 Gang，已有）
5. Soft → `HyperNodeOrderFn` 加权

## 配置示例（多 Instance + Prefill-Decode 角色内打散）

组间拓扑见实例 4；此处仅示 **跨 instance 互斥** + 两条角色 policy（**无** prefill↔decode 组间 term）。

```yaml
apiVersion: scheduling.volcano.sh/v1beta1
kind: PodGroup
metadata:
  name: pd-instance-0
  labels:
    volcano.sh/topology-group: llama-70b-serving
spec:
  minMember: 8
  queue: default

  topologyAffinity:
    podGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - topologyGroup: llama-70b-serving
          separationTierName: supernode

  subGroupPolicy:
    - name: prefill
      labelSelector:
        matchLabels:
          volcano.sh/role: prefill
      # 分片与组间拓扑：见实例 4（matchLabelKeys + subGroupTopologyAffinity）
    - name: decode
      labelSelector:
        matchLabels:
          volcano.sh/role: decode
      subGroupSize: 1

  # 角色内 prefill-0..3 / decode-0..3 互斥 @ cabinet：subGroupTopologyAffinity（省略）
  # 若需 prefill 与 decode 整机柜分离：另见实例 6
```

# Plugin Architecture

## 职责划分

| 插件 / 模块 | Hard（gradient） | Soft（order） | 资源预筛 |
|-------------|------------------|---------------|----------|
| `allocate` action（新增职责） | — | — | **HyperNode `minResource` vs idle/futureIdle**（与 Node predicate 前资源判断同级） |
| `network-topology-aware`（现有） | BFS 梯度、`highestTierAllowed`（**不含**资源判断） | HyperNode binpack、LCA tier 打分 | 仅维护 Session 级 HyperNode 资源账面（供 allocate 读取） |
| `group-topology-affinity`（新增） | Job 级别：跨 PodGroup hard；SubJob 级别：同 PodGroup、跨 SubGroup hard | 上述两类 soft 打分 | — |
| `framework`（聚合） | **仅拓扑插件**：集合交集 + 统一按 tier 重分层 | 多插件分数累加（已有） | **不参与** |

Hard / Soft 分工：

- **Hard 拓扑**（`topologyAffinity` / `subGroupTopologyAffinity` 的 **`required`** 列表，或 `networkTopology.mode: hard`）：各拓扑插件独立计算 `HyperNodeGradient`，Framework **求候选 HyperNode 集合交集** 后 **统一分层**。
- **Hard 容量**：在 **`allocate.go`** 对 gradient 候选做 **`minResource` 预筛**（**不**放入 `HyperNodeGradientForJobFn` 聚合逻辑）。
- **Soft 拓扑**（上述 API 的 **`preferred`** 列表 + `weight`，或 `networkTopology.mode: soft`）：仅 `HyperNodeOrderFn` 加权，不进入 gradient 交集。

## 框架约束：HyperNode Gradient 多插件聚合

### 现状与目标

| 项 | 现状 | 本设计 |
|----|------|--------|
| `HyperNodeGradientForJobFn` / `HyperNodeGradientForSubJobFn` | **仅第一个注册插件生效** | **所有启用 `enabledHyperNodeGradient` 的插件均参与** |
| 多 hard 约束组合 | 无法组合 | **集合交集（AND）** |
| `HyperNodeOrderFn` | 多插件分数累加 | 不变 |

### 聚合流程（拓扑插件交集 + allocate 资源预筛）

```mermaid
flowchart TB
    subgraph plugins [拓扑插件 - 仅拓扑]
        G1["network-topology-aware: [][]HyperNode"]
        G2["group-topology-affinity: [][]HyperNode"]
    end
    subgraph fw [Framework Session]
        U1["eligible_p = ⋃_k gradient_p[k]"]
        I["topologyEligible = ⋂_p eligible_p"]
        R["rebuildGradientsByTier"]
    end
    subgraph alc [allocate.go - 与 Gradient 回调解耦]
        RES["filterGradientsByMinResource\n(job/subJob, gradients)"]
        DRY["按 tier 升序 dry-run / predicate 选 Node"]
    end

    G1 --> U1
    G2 --> U1
    U1 --> I
    I --> R
    R --> RES
    RES --> DRY
```

**Framework 侧（仅拓扑，不含资源）：**

1. 各拓扑插件返回 `[][]*HyperNodeInfo`（满足「插件不变式」）。
2. `topologyEligible = ⋂_p ⋃_k gradient_p[k]`，再 `rebuildGradientsByTier`。
3. **不在此阶段做** `minResource` 判断。

**allocate 侧（容量预筛，类比 Node predicate 前资源判断）：**

4. `allocateForJob` / `allocateForSubJob` 在拿到 `hyperNodeGradients` 之后、进入 dry-run 循环之前，调用 **`filterGradientsByMinResource`**，剔除资源不足的 HyperNode。
5. 仅对过滤后的 HyperNode 做 subJob dry-run；进入 `allocateResourcesForTasks` 后，仍由现有 **`alloc.predicate`** 对 Node 做 `FutureIdle` 检查（双层：HyperNode 整组容量 → Node 单 Pod 容量）。

**与 Node 路径对照：**

| 层级 | 容量预筛位置 | 细粒度调度 |
|------|--------------|------------|
| Node | `allocate.predicate`：`InitResreq` vs `node.FutureIdle()` | `PredicateForAllocateAction` |
| HyperNode | **`allocate.filterGradientsByMinResource`**：`GetMinResources()` vs HyperNode idle/futureIdle | 其下对每个 Node 仍走 `predicate` |

**空结果：**

- 拓扑交集为空 → 拓扑不可满足；
- 资源过滤后为空 → 容量不可满足（可与 `NotEnoughResources` 等原因区分记录）。

**为何不采用「按 gradient 下标逐层交集」：**

各插件 BFS/剪枝路径不同，同一 HyperNode 可能落在不同下标层。按下标对齐交集会产生假阴性（某轮为空但并集交集非空）。**先集合交集、再统一分层** 语义稳定，且与 allocate「先低 tier、后高 tier」一致。

### 插件返回 gradient 的不变式

每个注册 `HyperNodeGradientFor*Fn` 的插件应保证：

1. `len(gradients) >= 1`；若无可行 HyperNode，返回空 slice 或 `nil`（Framework 视为 `eligible_p = ∅`）。
2. **层间 tier 单调**：对任意 `i < j`，`gradients[i]` 中任意 HyperNode 的 `tier` ≤ `gradients[j]` 中任意 HyperNode 的 `tier`（tier 越小表示域越贴近、越优先尝试）。
3. 同一 gradient 层内 HyperNode 名称不重复。

Framework **不**假设各插件 tier 分桶完全一致；最终以 **Session 统一重分层** 为准。

### 未注册 gradient 的插件

- 未注册 `HyperNodeGradientFor*Fn` 的插件：**不参与交集**（对该插件无 hard gradient 约束）。
- 某 Job/SubJob **无任何** required 组间拓扑约束时，可仅 `network-topology-aware` 注册；`group-topology-affinity` 在无 required 类约束 term 时可不注册 gradient，仅注册 Order（或整插件跳过）。

### Framework API（`session_plugins.go`）

```go
// 各插件仍通过 AddHyperNodeGradientForJobFn / AddHyperNodeGradientForSubJobFn 注册。

// HyperNodeGradientForJobFn 聚合逻辑（伪代码）：
func (ssn *Session) HyperNodeGradientForJobFn(job *api.JobInfo, root *api.HyperNodeInfo) [][]*api.HyperNodeInfo {
    var perPlugin [][]*api.HyperNodeInfo
    for _, plugin := range ssn.enabledGradientPlugins() {
        g, err := ssn.hyperNodeGradientForJobFns[plugin](job, root)
        if err != nil { /* 记录错误，该插件视为 ∅ 或整 Job 失败，策略可配置 */ }
        perPlugin = append(perPlugin, g)
    }
    if len(perPlugin) == 0 {
        return [][]*api.HyperNodeInfo{{root}}
    }
    if len(perPlugin) == 1 {
        return perPlugin[0]
    }
    eligible := intersectHyperNodeSets(perPlugin) // ⋂_p ⋃_k gradient_p[k]
    if len(eligible) == 0 {
        return nil
    }
    return rebuildGradientsByTier(ssn.HyperNodes, eligible)
}
```

辅助函数（建议放在 `pkg/scheduler/api` 或 `pkg/scheduler/framework`）：

```go
func unionGradientHyperNodeNames(gradients [][]*api.HyperNodeInfo) sets.Set[string]
func intersectHyperNodeSets(perPlugin [][]*api.HyperNodeInfo) sets.Set[string]
func rebuildGradientsByTier(hyperNodes api.HyperNodeInfoMap, eligible sets.Set[string]) [][]*api.HyperNodeInfo
```

`HyperNodeGradientForSubJobFn` 使用相同聚合逻辑；SubJob 上下文需传入 `hyperNodeForJob`（父域），各插件在子树内计算 gradient。

### Hard / Soft 与 Order 的配合

```text
拓扑 Hard:  HyperNodeGradient（多拓扑插件）→ Framework 交集 → rebuildGradientsByTier
容量 Hard:  allocate.filterGradientsByMinResource（不经过 HyperNodeGradientFor*Fn 聚合）
调度:       allocate 逐层 dry-run → allocateResourcesForTasks → predicate(Node)
Soft:       HyperNodeOrderFn（多插件分数相加）→ selectBestHyperNodeForSubJob
```

`group-topology-affinity`：**`required` term** → `HyperNodeGradientFor*Fn`；**`preferred` term** → `HyperNodeOrderFn`（term 内 **无** `mode` 字段）。

## allocate Action：HyperNode 资源预筛（与 Gradient 回调解耦）

### 设计原则

- **资源是否够 Gang / SubGroup**：属于 **allocate action** 的调度路径决策，与「拓扑插件如何产 gradient」正交。
- **不**在 `HyperNodeGradientForJobFn` / `HyperNodeGradientForSubJobFn` 的 Framework 聚合里做 `minResource` 过滤，避免 group-topology-affinity、network-topology-aware 在 BFS 中对资源不足 HyperNode 重复计算，也避免与回调生命周期耦合。
- 判断逻辑从 `network-topology-aware.isEligibleHyperNode` **迁出容量分支**，改为 allocate 内显式调用；network-topology-aware **仅保留 tier / `highestTierAllowed`** 等拓扑条件。

### 调用位置

在 `allocateForJob` 中（`allocateForSubJob` 同理，使用 `subJob.GetMinResources()`）：

```go
// 1. 拓扑：仅通过 Framework 插件回调（多插件交集 + 重分层）
hyperNodeGradients := ssn.HyperNodeGradientForJobFn(job, hyperNodeToAllocate)

// 2. 容量：allocate 本地过滤，与 HyperNodeGradientForJobFn 无关
hyperNodeGradients = alloc.filterGradientsByMinResource(job, nil, hyperNodeGradients)

for gradient, hyperNodes := range hyperNodeGradients {
    for _, hyperNode := range hyperNodes {
        // 3. dry-run subJobs（内部 allocateResourcesForTasks → predicate(Node)）
    }
}
```

SubJob 级别在 `allocateForSubJob` 内、调用 `HyperNodeGradientForSubJobFn` 之后同样执行 `filterGradientsByMinResource(job, subJob, gradients)`。

### `filterGradientsByMinResource` 语义

```go
// pkg/scheduler/actions/allocate/hypernode_resource.go（建议新文件或 allocate.go 内）

func (alloc *Action) filterGradientsByMinResource(
    job *api.JobInfo,
    subJob *api.SubJobInfo, // nil 表示 Job 级别过滤
    gradients [][]*api.HyperNodeInfo,
) [][]*api.HyperNodeInfo
```

| 规则 | 行为 |
|------|------|
| `minResource` | `subJob != nil` → `subJob.GetMinResources()`；否则 `job.GetMinResources()` |
| 比较口径 | 与现 `isEligibleHyperNode` 一致：`minResource.LessEqual(idle)` **或** `minResource.LessEqual(futureIdle)` 则保留 |
| 部分已调度 | `job.AllocatedHyperNode != ""`（Job 级别）或 `subJob.AllocatedHyperNode != ""`（SubJob 级别）时 **跳过** 资源过滤，与现网「partial 不预筛资源」一致 |
| 数据来源 | 读取 **Session 级** HyperNode 资源账面（见下），allocate **不**实现 cache 更新 |

过滤实现：对 `gradients` 每层 `hyperNodes` 原地剔除不满足的项；若某层为空则跳过该层（与现 BFS 不产出该层效果一致）。

### Session 级 HyperNode 资源账面

将 `network-topology-aware` 中的 `hyperNodeResourceCache` **提升为 Session 可读**（名称示例 `ssn.HyperNodeResourceStatus`），仍由 network-topology-aware 在 `OnSessionOpen` + `Allocate`/`Deallocate` EventHandler 维护；allocate 只读。

```go
// api 或 framework.Session
type HyperNodeResourceStatus struct {
    Allocatable, Used, Idle, FutureIdle *api.Resource
}

func (ssn *Session) HyperNodeSatisfiesMinResource(
    hyperNodeName string,
    minResource *api.Resource,
) bool
```

便于单测：对 `filterGradientsByMinResource` 注入 mock Session 账面，无需拉起 gradient 插件。

### `network-topology-aware` 配套改动

```go
// isEligibleHyperNode：删除 minResource / hyperNodeResourceCache 分支，仅保留：
// - tier <= highestAllowedTier
// - （不再在此处做 idle/futureIdle 判断）

func (networkTopologyAware *networkTopologyAwarePlugin) isEligibleHyperNode(
    hn *api.HyperNodeInfo,
    highestAllowedTier int,
    allocatedHyperNode string,
) bool {
    if hn.Tier() > highestAllowedTier {
        return false
    }
    if allocatedHyperNode != "" {
        return true
    }
    return true // 拓扑 BFS 默认展开；资源由 allocate 预筛
}
```

`hyperNodeGradientFn` 签名可去掉 `minResource` 参数；`HyperNodeGradientForJobFn` / `HyperNodeGradientForSubJobFn` 注册函数不再传入 `job.GetMinResources()`。

### 为何放在 allocate 更合适

| 点 | 说明 |
|----|------|
| 与 Node 一致 | 容量先筛、再 predicate，都在 **action** 层，而非 scheduler plugin 回调聚合 |
| 解耦 | Framework `HyperNodeGradientFor*Fn` 只表达 **拓扑可行域**；容量是 Job/SubJob 调度上下文 |
| 性能 | group-topology-affinity / network-topology-aware 的 BFS 不再遍历「注定容量不够」的 HyperNode；过滤在 gradient 列表上 **O(n)** 一次完成 |
| 可测 | allocate 单测覆盖资源过滤，无需 mock 多插件 gradient |

## 推荐 Scheduler 配置

```yaml
actions: "enqueue, allocate, backfill"
tiers:
- plugins:
  - name: gang
  - name: predicates
  - name: group-topology-affinity
    arguments:
      group-topology-affinity.weight: 10
  - name: network-topology-aware
    arguments:
      weight: 10
```

两者均开启 `enabledHyperNodeGradient` 与 `enabledHyperNodeOrder`（与现有 e2e 配置一致）。tier 内插件顺序不影响交集交换律，但影响 **Order 分数相加顺序**（加法可交换，无影响）。

## group-topology-affinity 扩展点

| 扩展点 | 用途 |
|--------|------|
| `OnSessionOpen` | 构建 `TopologyOccupancyIndex` |
| `AddHyperNodeGradientForJobFn` | Hard 跨 PodGroup 约束下的 Job 级别梯度 |
| `AddHyperNodeGradientForSubJobFn` | Hard 跨 SubGroup 约束下的 SubJob 级别梯度（含同 Job 已分配 SubJob 域） |
| `AddHyperNodeOrderFn` | Soft 跨组拓扑亲和/反亲和偏好 |
| `AddJobValidFn`（可选 P2） | 全局域耗尽预检 |

## network-topology-aware 改动（最小）

- **保留** `hyperNodeGradientFn` BFS；`isEligibleHyperNode` **仅拓扑**（tier / partial 场景），**移除** `minResource` 判断。
- **保留** `hyperNodeResourceCache` 维护，但升级为 **Session 可读**，供 allocate 使用。
- 与 `group-topology-affinity` 通过 Framework **拓扑 gradient 交集**协作；**不**在插件内做容量预筛。

## allocate Action（其它）

- `RequiresHyperNodeAllocate()`：`ContainsHardTopology() \|\| ContainsSubJobPolicy() \|\| ContainsHardCrossPodGroupTopology() \|\| ContainsHardCrossSubGroupTopology()`
- `ContainsHardCrossPodGroupTopology(job)`：非空 `topologyAffinity` 且存在非空 `podGroupAffinity` / `podGroupAntiAffinity` 的 **`required`** 列表
- `ContainsHardCrossSubGroupTopology(job)`：非空 `subGroupTopologyAffinity` 且存在 hard `subGroupAffinity` / `subGroupAntiAffinity` term
- `organizeJobWorksheet`：含 hard subGroup antiAffinity 的 SubJob 稳定排序（被依赖方先调度）
- 主路径：`allocateForJob` →（`HyperNodeGradientForJobFn` → **`filterGradientsByMinResource`**）→ `allocateForSubJob` →（`HyperNodeGradientForSubJobFn` → **资源过滤**）→ `selectBestHyperNodeForSubJob` → `allocateResourcesForTasks` → `predicate(Node)`

## 实现文件（Phase 1）

| 路径 | 说明 |
|------|------|
| `staging/.../scheduling/v1beta1/types.go` | API |
| `pkg/scheduler/api/topology_constraint.go` | 解析结构 |
| `pkg/scheduler/api/topology_occupancy.go` | 占用索引 |
| `pkg/scheduler/api/hyper_node_info.go` | `GetAncestorAtTier` |
| `pkg/scheduler/api/hyper_node_gradient.go` | `union` / `intersect` / `rebuildGradientsByTier` |
| `pkg/scheduler/api/hyper_node_resource.go` | HyperNode 资源账面类型 + `SatisfiesMinResource` |
| `pkg/scheduler/plugins/group-topology-affinity/` | 新插件（topology gradient + order） |
| `pkg/scheduler/plugins/network-topology-aware/` | 去掉 gradient 内资源预筛；Session 资源 cache |
| `pkg/scheduler/framework/session.go` | 暴露 HyperNode 资源账面 |
| `pkg/scheduler/framework/session_plugins.go` | **仅拓扑** Gradient 多插件交集 + 重分层 |
| `pkg/scheduler/actions/allocate/allocate.go` | `filterGradientsByMinResource`；Job/SubJob 调用点 |
| `pkg/scheduler/actions/allocate/hypernode_resource_test.go` | 资源过滤单测 |
| `pkg/webhooks/.../validate_podgroup.go` | 校验 |
| `pkg/scheduler/framework/session_plugins_test.go` | 拓扑交集/重分层单测 |

---

# 架构与时序图

本章给出端到端调度路径的**流程图**与**时序图**，便于实现与评审时对齐模块边界。图中 **Framework** = Framework Session；**network-topology-aware**、**group-topology-affinity**、**allocate** 均使用插件/模块全称。时序图里 `allocateAction` 的**自环**表示 allocate 模块内部步骤，不是跨参与者的递归调用。

**阅读顺序建议：** 图例与分层 → §4 Framework gradient 聚合 → §5 `allocateForJob` → §6–§8 SubJob / 跨 PodGroup 示例。

## 图例与分层

```mermaid
flowchart LR
    subgraph L1 [配置与准入]
        API[PodGroup API]
        WH[Admission Webhook]
        Cache[Scheduler Cache / JobInfo]
    end
    subgraph L2 [Session 周期]
        Open[Session Open]
        Plugins[Plugins OnSessionOpen]
        Actions[Actions Execute]
        Close[Session Close / JobUpdater]
    end
    subgraph L3 [allocate 拓扑路径]
        Grad[HyperNodeGradient 聚合]
        ResF[filterGradientsByMinResource]
        DryRun[dry-run + Statement]
        NodeP[predicate + Bind]
    end
    API --> WH --> Cache --> Open --> Plugins --> Actions
    Actions --> Grad --> ResF --> DryRun --> NodeP --> Close
```

| 符号 | 含义 |
|------|------|
| 实线箭头 | 同步调用 / 顺序执行 |
| 虚线箭头 | 可选路径或条件分支 |
| `Domain_T(H)` | HyperNode H 在分离 tier T 上的祖先域 |

---

## 1. 调度周期总览

Volcano 一个 scheduling cycle 内，与本文相关的组件协作关系如下。

```mermaid
flowchart TB
    Start([Scheduler RunOnce]) --> CacheSync[Cache 同步 PodGroup / Pod / HyperNode]
    CacheSync --> OpenSession[framework.OpenSession]
    OpenSession --> PluginOpen[各 Plugin OnSessionOpen]
    PluginOpen --> NTAInit[network-topology-aware: 初始化 HyperNode 资源账面]
    PluginOpen --> GTAInit[group-topology-affinity: 构建 TopologyOccupancyIndex]
    PluginOpen --> Actions[按序执行 Actions]
    Actions --> Enqueue[enqueue 可选]
    Enqueue --> Allocate[allocate]
    Allocate --> Backfill[backfill 可选]
    Backfill --> CloseSession[Session Close]
    CloseSession --> JobUpdater[JobUpdater 写回 PodGroup Status / Annotation]
    JobUpdater --> End([周期结束])
```

---

## 2. Session Open 时序图

```mermaid
sequenceDiagram
    autonumber
    participant Sch as Volcano Scheduler
    participant Cache as SchedulerCache
    participant Framework as Framework Session
    participant network-topology-aware as network-topology-aware
    participant group-topology-affinity as group-topology-affinity
    participant Gang as gang

    Sch->>Cache: 构建 Jobs / Nodes / HyperNodes
    Sch->>Framework: OpenSession(cache)
    Framework->>network-topology-aware: OnSessionOpen(ssn)
    network-topology-aware->>network-topology-aware: initHyperNodeResourceCache<br/>汇总 Node → HyperNode 资源
    network-topology-aware->>Framework: 写入 ssn.HyperNodeResourceCache
    network-topology-aware->>Framework: Register HyperNodeOrderFn / GradientFn / EventHandler
    Framework->>group-topology-affinity: OnSessionOpen(ssn)
    group-topology-affinity->>group-topology-affinity: TopologyOccupancyIndex.Build<br/>已运行 Job 的 Domain 占用
    group-topology-affinity->>Framework: Register HyperNodeGradientFn / OrderFn
    Framework->>Gang: OnSessionOpen(ssn)
    Note over Framework: 其他插件略
    Sch->>Framework: Execute allocate action
```

---

## 3. allocate Action 总流程

```mermaid
flowchart TB
    Start([allocate.Execute]) --> BuildCtx[buildAllocateContext<br/>按 Queue 组织 JobWorksheet]
    BuildCtx --> PopQueue{queues 非空?}
    PopQueue -->|否| End([结束])
    PopQueue -->|是| PopJob[取 Job]
    PopJob --> CheckPath{RequiresHyperNodeAllocate?<br/>hard topology / subGroupPolicy<br/>/ hard cross topology}
    CheckPath -->|否| NormalPath[tasksNoHardTopology 队列<br/>allocateResourcesForTasks 直调]
    CheckPath -->|是| TopoPath[allocateForJob]
    NormalPath --> PushQueue[queue 重新入队]
    TopoPath --> PushQueue
    PushQueue --> PopQueue
```

**`RequiresHyperNodeAllocate()` 判定（流程图）：**

```mermaid
flowchart LR
    J[JobInfo] --> A{ContainsHardTopology?}
    A -->|是| Y[走 allocateForJob]
    A -->|否| B{ContainsSubJobPolicy?}
    B -->|是| Y
    B -->|否| C{ContainsHardCrossPodGroupTopology<br/>or SubGroupPolicy?}
    C -->|是| Y
    C -->|否| N[走普通 Node 分配路径]
```

---

## 4. Framework：HyperNodeGradient 聚合

### 4.1 聚合流程图

```mermaid
flowchart TB
    Entry([HyperNodeGradientForJobFn<br/>或 ForSubJobFn]) --> LoopP[遍历 enabledHyperNodeGradient 插件]
    LoopP --> CallP[调用 plugin.gradientFn<br/>得到 gradients_p]
    CallP --> UnionP["eligible_p = ⋃_k gradients_p[k]"]
    UnionP --> MoreP{还有插件?}
    MoreP -->|是| LoopP
    MoreP -->|否| OneP{仅 1 个插件?}
    OneP -->|是| RetSingle[直接返回该 gradients]
    OneP -->|否| Intersect["topologyEligible = ⋂_p eligible_p"]
    Intersect --> Empty{交集为空?}
    Empty -->|是| RetNil[返回 nil / 空 gradient]
    Empty -->|否| Rebuild[rebuildGradientsByTier<br/>按 tier 升序分层]
    Rebuild --> RetGrad[返回 [][]HyperNode]
```

### 4.2 聚合时序图（Job 级别示例）

```mermaid
sequenceDiagram
    autonumber
    participant allocateAction as allocate.allocateForJob
    participant Framework as Framework Session
    participant network-topology-aware as network-topology-aware
    participant group-topology-affinity as group-topology-affinity

    allocateAction->>Framework: HyperNodeGradientForJobFn(job, clusterRoot)
    Framework->>network-topology-aware: gradientJob(job, root)
    network-topology-aware->>network-topology-aware: hyperNodeGradientFn BFS<br/>tier / highestTierAllowed
    network-topology-aware-->>Framework: gradients_network_topology_aware
    Framework->>group-topology-affinity: gradientJob(job, root)
    group-topology-affinity->>group-topology-affinity: 剪枝: 跨 PodGroup antiAffinity 等
    group-topology-affinity-->>Framework: gradients_group_topology_affinity
    Framework->>Framework: eligible_network_topology_aware = ⋃ gradients_network_topology_aware
    Framework->>Framework: eligible_group_topology_affinity = ⋃ gradients_group_topology_affinity
    Framework->>Framework: eligible = eligible_network_topology_aware ∩ eligible_group_topology_affinity
    Framework->>Framework: rebuildGradientsByTier(eligible)
    Framework-->>allocateAction: hyperNodeGradients
```

---

## 5. allocateForJob 完整流程

### 5.1 流程图

```mermaid
flowchart TB
    Start([allocateForJob]) --> Snap[SnapshotSubJobStatus]
    Snap --> G1[HyperNodeGradientForJobFn<br/>拓扑聚合]
    G1 --> R1[filterGradientsByMinResource<br/>job, nil, gradients]
    R1 --> EmptyR{过滤后为空?}
    EmptyR -->|是| FailJob([返回 nil<br/>Job 不可调度])
    EmptyR -->|否| LoopGrad[遍历 gradient 层 g]
    LoopGrad --> LoopHN[遍历该层每个 hyperNode H]
    LoopHN --> Reset[ResetFitErr + Clone jobWorksheet]
    Reset --> LoopSJ[遍历 subJobs<br/>organizeJobWorksheet 顺序]
    LoopSJ --> AFSJ[allocateForSubJob<br/>subJob, worksheet, H]
    AFSJ --> Merge{stmt 非空?}
    Merge -->|是| AccScore[累计分数 / 检查 JobReady]
    Merge -->|否| LoopSJ
    AccScore --> Recover[RecoverSubJobStatus]
    Recover --> DryDiscard[Statement Discard<br/>dry-run 不落库]
    DryDiscard --> LoopHN
    LoopHN --> HasSol{本层有可行解?}
    HasSol -->|否| LoopGrad
    HasSol -->|是| BestJob[selectBestHyperNodeForJob]
    BestJob --> Commit[RecoverOperations + Commit]
    Commit --> Recorder[Recorder 更新 AllocatedHyperNode]
    Recorder --> RetStmt([返回 Statement])
```

### 5.2 时序图

```mermaid
sequenceDiagram
    autonumber
    participant allocateAction as allocateForJob
    participant Framework as Framework Session
    participant network-topology-aware as network-topology-aware
    participant group-topology-affinity as group-topology-affinity
    participant AFSJ as allocateForSubJob

    allocateAction->>allocateAction: SnapshotSubJobStatus(job)
    allocateAction->>Framework: HyperNodeGradientForJobFn(job, root)
    Note over Framework,group-topology-affinity: 见「4.2 聚合时序图」
    Framework-->>allocateAction: jobGradients
    allocateAction->>allocateAction: filterGradientsByMinResource(job, nil, jobGradients)
    allocateAction->>Framework: 读 HyperNodeResourceCache
    loop 每个 gradient 层 / 每个 hyperNode H
        allocateAction->>allocateAction: jobWorksheetCopy = Clone()
        loop 每个 subJob
            allocateAction->>AFSJ: allocateForSubJob(subJob, ws, H)
            AFSJ-->>allocate: stmt, score
            allocateAction->>allocateAction: Discard(stmt) dry-run
        end
        allocateAction->>allocateAction: selectBestHyperNodeForJob(scores)
    end
    allocateAction->>allocateAction: Commit 最优 Statement
    allocateAction->>allocateAction: Recorder.UpdateDecisionToJob
```

---

## 6. HyperNode 最小资源预筛（allocate）

与 `HyperNodeGradientFor*Fn` **解耦**，在 allocate 内完成。

### 6.1 流程图

```mermaid
flowchart TB
    Start([filterGradientsByMinResource]) --> Partial{job/subJob 已有<br/>AllocatedHyperNode?}
    Partial -->|是| Skip[跳过资源过滤<br/>返回原 gradients]
    Partial -->|否| MinR[minResource = job 或 subJob<br/>.GetMinResources]
    MinR --> LoopG[遍历每层 gradient]
    LoopG --> LoopH[遍历层内每个 hn]
    LoopH --> Read[读 ssn.HyperNodeResourceCache hn]
    Read --> Check{minResource <= idle<br/>OR <= futureIdle?}
    Check -->|是| Keep[保留 hn]
    Check -->|否| Drop[剔除 hn]
    Keep --> LoopH
    Drop --> LoopH
    LoopH --> PruneEmpty[剔除空层]
    PruneEmpty --> Ret([返回过滤后 gradients])
```

### 6.2 时序图

```mermaid
sequenceDiagram
    autonumber
    participant allocateAction as allocate action
    participant Framework as Framework Session
    participant Cache as HyperNodeResourceCache

    allocateAction->>allocateAction: 若 AllocatedHyperNode 非空则直接返回
    allocateAction->>allocateAction: minResource = GetMinResources()
    loop 每个 hyperNode hn in gradients
        allocateAction->>Framework: HyperNodeResourceCache[hn]
        Framework->>Cache: idle / futureIdle
        Cache-->>allocate: 资源账面
        allocateAction->>allocateAction: HyperNodeSatisfiesMinResource?
    end
    allocate-->>allocate: 返回过滤后的 gradients
```

---

## 7. allocateForSubJob 与选优

### 7.1 流程图

```mermaid
flowchart TB
    Start([allocateForSubJob]) --> G2[HyperNodeGradientForSubJobFn<br/>subJob, hyperNodeForJob]
    G2 --> R2[filterGradientsByMinResource<br/>job, subJob, gradients]
    R2 --> LoopG2[遍历 gradient / hyperNode 候选]
    LoopG2 --> ARF[allocateResourcesForTasks<br/>tasks, hyperNode]
    ARF --> HasStmt{有分配操作?}
    HasStmt -->|是| Backup[stmtBackup + worksheetBackup]
    HasStmt -->|否| LoopG2
    Backup --> Discard2[Discard dry-run]
    Discard2 --> LoopG2
    LoopG2 --> Select[selectBestHyperNodeForSubJob<br/>HyperNodeOrderMapFn 分数累加]
    Select --> LCA[更新 subJob.AllocatedHyperNode<br/>LCA 合并]
    LCA --> Ret([返回 bestStmt, score])
```

### 7.2 时序图（含 HyperNodeOrder）

```mermaid
sequenceDiagram
    autonumber
    participant AFSJ as allocateForSubJob
    participant Framework as Framework Session
    participant network-topology-aware as network-topology-aware
    participant group-topology-affinity as group-topology-affinity
    participant ARF as allocateResourcesForTasks

    AFSJ->>Framework: HyperNodeGradientForSubJobFn(subJob, parentHN)
    Framework-->>AFSJ: subJobGradients
    AFSJ->>AFSJ: filterGradientsByMinResource(job, subJob, ...)
    loop 每个候选 hyperNode
        AFSJ->>ARF: allocateResourcesForTasks(subJob, tasks, hn)
        ARF-->>AFSJ: stmt / empty
        AFSJ->>AFSJ: stmt.Discard()
    end
    AFSJ->>Framework: HyperNodeOrderMapFn(subJob, candidateNodes)
    Framework->>network-topology-aware: HyperNodeOrderFn → scores_network_topology_aware
    Framework->>group-topology-affinity: HyperNodeOrderFn → scores_group_topology_affinity
    Framework->>Framework: 分数相加
    Framework-->>AFSJ: bestHyperNode, score
    AFSJ->>AFSJ: RecoverOperations(bestStmt)
```

---

## 8. Task 级：allocateResourcesForTasks 与 Node predicate

HyperNode 选定后的 **Node 级**路径（与 HyperNode 资源预筛分层）。

```mermaid
flowchart TB
    Start([allocateResourcesForTasks]) --> Nodes[RealNodesList hyperNode]
    Nodes --> PopT{tasks 非空?}
    PopT -->|否| RetStmt([返回 Statement])
    PopT -->|是| PopTask[Pop Task]
    PopTask --> QueueOK{ssn.Allocatable?}
    QueueOK -->|否| PopT
    PopTask --> PrePred[PrePredicateFn]
    PrePred --> PredRes[predicate: InitResreq vs<br/>node.FutureIdle]
    PredRes --> PredPlugins[PredicateForAllocateAction<br/>各插件]
    PredPlugins --> HasNode{有可行 Node?}
    HasNode -->|否| FitErr[记录 FitErrors]
    HasNode -->|是| Order[prioritizeNodes / NodeOrderFn]
    Order --> Pipeline[Statement.Pipeline / Commit op]
    Pipeline --> SubReady{SubJobReady?}
    SubReady -->|是| PopT
    SubReady -->|否| PopT
```

```mermaid
sequenceDiagram
    autonumber
    participant ARF as allocateResourcesForTasks
    participant Framework as Framework Session
    participant Pred as predicates 等

    loop 每个 Task
        ARF->>Framework: Allocatable(queue, task)
        ARF->>Framework: PrePredicateFn(task)
        ARF->>ARF: predicate(task, node)
        Note over ARF: 先 FutureIdle 资源判断
        ARF->>Framework: PredicateForAllocateAction(task, node)
        Framework->>Pred: 插件 Predicate
        ARF->>Framework: prioritizeNodes
        ARF->>ARF: allocateResourcesForTask → Pipeline
    end
```

---

## 9. group-topology-affinity：占用索引与跨 PodGroup 反亲和

### 9.1 OccupancyIndex 构建（Session Open）

```mermaid
flowchart LR
    subgraph Input
        Jobs[Running/Inqueue Jobs]
        Ann[job.AllocatedHyperNode]
        PGSpec[topologyAffinity terms]
    end
    subgraph Build
        TierMap[解析 separationTier]
        Domain[Domain_T AllocatedHN]
        Idx[topologyGroup → domain → Set JobUID]
    end
    Jobs --> Ann --> Domain --> Idx
    PGSpec --> TierMap --> Idx
```

### 9.2 跨 PodGroup 反亲和判定（hard）

```mermaid
flowchart TB
    Start([候选 HyperNode H<br/>调度 Job J]) --> TG{J 配置了<br/>topologyGroup / selector?}
    TG -->|否| Pass[eligible]
    TG -->|是| DomH[计算 Domain_T(H)]
    DomH --> Lookup[OccupancyIndex 查询<br/>同 topologyGroup 已占用 domain]
    Lookup --> Conflict{存在其他 Job<br/>且 domain 相同?}
    Conflict -->|是| Reject[从 gradient 剔除 H]
    Conflict -->|否| Pass
```

```mermaid
sequenceDiagram
    autonumber
    participant group-topology-affinity as group-topology-affinity
    participant Idx as TopologyOccupancyIndex
    participant Framework as HyperNodes

    group-topology-affinity->>Framework: gradientJob(job, root)
    loop BFS 每个候选 hn
        group-topology-affinity->>group-topology-affinity: Domain_T(hn) at separationTier
        group-topology-affinity->>Idx: IsDomainOccupied(topologyGroup, domain, job.UID)
        Idx-->>group-topology-affinity: occupied / free
    end
    group-topology-affinity-->>group-topology-affinity: 输出剪枝后 gradients_group_topology_affinity
```

---

## 10. 跨 SubGroup 约束（SubJob 之间）

> **作用域提醒：** 约束在 **SubJob** 之间；同一 `subGroupPolicy.name` 下可有多个 SubJob（`matchLabelKeys`）。**policy 内**互斥：selector 同名（实例 4）；**跨 policy**互斥：selector 不同名（实例 6）。**不**涉及 `topologyAffinity`。

### 10.1 SubJob 调度顺序（organizeJobWorksheet）

含 hard `subGroupAntiAffinity` 时，**被依赖方先调度**（如先 prefill，再 decode）。

```mermaid
flowchart LR
    WS[JobWorksheet.subJobs 优先队列] --> Req[未满足 minSubGroups 的 GID 优先]
    Req --> Anti[antiAffinity 中作为 SubGroupSelector<br/>的 policy 对应 subJob 优先]
    Anti --> Order[SubJobOrderFn gang ready]
```

### 10.2 跨 SubGroup 亲和 / 反亲和判定

```mermaid
flowchart TB
    Start([SubJob S 候选 hn]) --> Aff{hard subGroupAffinity?}
    Aff -->|是| PeerA[取 peer subJobs 已分配<br/>AllocatedHyperNode]
    PeerA --> SameDom{Domain_T(hn) ==<br/>Domain_T(peer)?}
    SameDom -->|否| RejA[剔除]
    SameDom -->|是| Anti
    Aff -->|否| Anti{hard subGroupAntiAffinity?}
    Anti -->|是| PeerB[取 antiSubGroupSelector<br/>对应已分配 peer]
    PeerB --> DiffDom{Domain_T(hn) !=<br/>Domain_T(peer)?}
    DiffDom -->|否| RejB[剔除]
    DiffDom -->|是| OK[保留]
    Anti -->|否| OK
```

### 10.3 时序示例：实例 6（prefill 与 decode 跨角色分机柜）

仅当配置了 prefill↔decode 的 `subGroupTopologyAffinity` 时适用。

```mermaid
sequenceDiagram
    autonumber
    participant allocateAction as allocateForJob
    participant AFSJ as allocateForSubJob
    participant group-topology-affinity as group-topology-affinity

    allocateAction->>AFSJ: allocateForSubJob(prefill)
    AFSJ->>AFSJ: 选定 SubJob prefill，Domain_cabinet=A
    allocateAction->>AFSJ: allocateForSubJob(decode)
    group-topology-affinity->>group-topology-affinity: subGroupAntiAffinity: Domain_cabinet(decode) != A
    group-topology-affinity->>group-topology-affinity: subGroupAffinity: Domain_super(decode) == Domain_super(prefill)
```

### 10.4 拓扑结构对照

**实例 4（Prefill-Decode 默认）：** prefill 分片占不同 cabinet，decode 分片占不同 cabinet，prefill 与 decode 无柜级约束。

```mermaid
flowchart TB
    subgraph PF[prefill 副本]
        P0[prefill-0 @ cabinet-1]
        P1[prefill-1 @ cabinet-2]
        P2[prefill-2 @ cabinet-3]
        P3[prefill-3 @ cabinet-4]
    end
    subgraph DC[decode 副本]
        D0[decode-0 @ cabinet-5]
        D1[decode-1 @ cabinet-6]
    end
```

**实例 6（可选）：** prefill 柜组与 decode 柜组分离，仍共 supernode。

```mermaid
flowchart TB
    subgraph SN[supernode S]
        CA[cabinet A — prefill SubJob 4 Pods]
        CB[cabinet B — decode SubJob 4 Pods]
    end
```

| 场景 | 约束 |
|------|------|
| 实例 4：prefill 分片互斥 | `subGroupAntiAffinity` @ cabinet，selector 均为 `[prefill]` |
| 实例 4：prefill vs decode | **无** |
| 实例 6：prefill vs decode | `Domain_cabinet` 互异且 `Domain_super` 相同 |

---

## 11. 多 Instance：跨 PodGroup 反亲和

```mermaid
flowchart TB
    subgraph Cluster
        subgraph SN1[超节点 SN-1]
            PG1[PodGroup instance-0]
        end
        subgraph SN2[超节点 SN-2]
            PG2[PodGroup instance-1]
        end
        subgraph SN3[超节点 SN-3]
            PG3[PodGroup instance-2]
        end
    end
    PG1 -.->|topologyGroup 相同<br/>Domain 互斥| PG2
    PodGroup2 -.->|Domain 互斥| PG3
```

```mermaid
sequenceDiagram
    autonumber
    participant PodGroup1 as PodGroup inst-0
    participant PodGroup2 as PodGroup inst-1
    participant group-topology-affinity as group-topology-affinity
    participant Idx as OccupancyIndex

    PodGroup1->>group-topology-affinity: 调度完成 Domain_T=SN-1
    group-topology-affinity->>Idx: Register(inst-0, SN-1)
    PodGroup2->>group-topology-affinity: gradient 候选
    group-topology-affinity->>Idx: SN-1 occupied by inst-0
    group-topology-affinity->>group-topology-affinity: 仅尝试 SN-2, SN-3, ...
    PodGroup2->>PodGroup2: 落入 SN-2
    group-topology-affinity->>Idx: Register(inst-1, SN-2)
```

---

## 12. Hard 约束综合决策（单候选 HyperNode）

对单个候选 `hn` 在进入 dry-run 前的逻辑合并视图（实现可分散在 group-topology-affinity / network-topology-aware / allocate，语义如下）。

```mermaid
flowchart TB
    H[候选 HyperNode hn] --> R1{allocate:<br/>minResource 满足?}
    R1 -->|否| X[剔除]
    R1 -->|是| R2{network-topology-aware:<br/>tier / highestTierAllowed?}
    R2 -->|否| X
    R2 -->|是| R3{group-topology-affinity:<br/>跨 PodGroup antiAffinity?}
    R3 -->|冲突| X
    R3 -->|否| R4{group-topology-affinity:<br/>跨 SubGroup affinity / antiAffinity?}
    R4 -->|不满足| X
    R4 -->|满足| OK[进入 dry-run]
```

---

## 13. 失败路径与 Status 回写

```mermaid
sequenceDiagram
    autonumber
    participant allocateAction as allocate action
    participant Framework as Framework
    participant Gang as gang OnSessionClose
    participant JU as JobUpdater
    participant PodGroupAPI as PodGroup API

    allocateAction->>allocateAction: gradient 交集为空 / 资源过滤为空
    allocate-->>Framework: 本周期未 Commit
    Framework->>Gang: OnSessionClose
    Gang->>PodGroupAPI: 可选 Unschedulable Condition
    Framework->>JU: UpdateAll
    JU->>PodGroupAPI: Phase / Conditions<br/>TopologyUnsatisfiable 等
    JU->>PodGroupAPI: Annotation job-allocated-hypernode
```

---

## 14. 与现有实现对齐说明

| 图中步骤 | 代码锚点（当前/目标） |
|----------|----------------------|
| allocate.Execute | `pkg/scheduler/actions/allocate/allocate.go` |
| HyperNodeGradientForJobFn | `pkg/scheduler/framework/session_plugins.go`（目标：多插件交集） |
| filterGradientsByMinResource | `allocate.go`（**待实现**） |
| hyperNodeGradientFn BFS | `pkg/scheduler/plugins/network-topology-aware/network_topology_aware.go` |
| isEligibleHyperNode 资源分支 | 同上（**待迁出**至 allocate） |
| allocateResourcesForTasks + predicate | `allocate.go` |
| HyperNodeOrderMapFn | `session_plugins.go` + `util.PrioritizeHyperNodes` |

---

# 竞品与标准对齐

本章从 **友商产品用法**、**与本设计的能力差距**、**字段映射** 三方面展开，便于对外沟通、迁移评估与 API 对齐。各小节均附 **官方资料链接**；文中 YAML 为从文档摘录的 **示意配置**（层级名、label 需与目标集群一致）。

## 能力总览

| 能力维度 | kube-scheduler | Kueue | KAI Scheduler | Koordinator | Volcano（现状） | Volcano（本设计） |
|----------|----------------|-------|---------------|-------------|-----------------|-------------------|
| Gang / PodGroup | PodGroup `gang.minCount` | Workload + PodSet 准入 | PodGroup + pod-grouper | Coscheduling PodGroup | PodGroup + gang | 不变 |
| 组内拓扑共域 | `schedulingConstraints.topology` | PodSet 注解 `podset-required-topology` | `TopologyConstraint` / Job 注解 | PodGroup 注解 `network-topology-spec` | `networkTopology` + HyperNode | 不变 |
| 组内 SubGroup | 无（Workload 多模板） | `podset-group-name` 多 PodSet | Hierarchical `subGroups[]` | 多 PodGroup + `groups` 注解 | `subGroupPolicy` | 不变 |
| 跨 PodGroup / 多 instance 打散 | Pod `topologySpreadConstraints` | 多 Workload / 多 PodSet 组 | 多 PodGroup + 各 constraint | 多 PodGroup `groups` + gather | 无 | `topologyAffinity.podGroupAntiAffinity` |
| 同 PodGroup 内分片互斥 | Pod spread | slice 注解（单层/多层） | 多 SubGroup + 各层 constraint | 单 PodGroup gather | `matchLabelKeys` + policy 内 anti | 实例 4 |
| 同 PodGroup 跨角色拓扑 | Pod affinity 组合 | 多 PodSet 同域注解 | 父/子 SubGroup 层级共域 | `groups` + MustGather | 无 | `subGroupTopologyAffinity` |
| 拓扑数据源 | Node label | Node label 层级 | Topology CRD 树 | `ClusterNetworkTopology` + Node label | HyperNode CRD | HyperNode CRD |
| 组间 hard 反亲和 API | 无（Pod 级 spread） | 无 | 无一等字段 | 无（gather 语义） | 无 | `required` + `separationTierName` |

---

## 友商洞察（详细）

### Kubernetes（kube-scheduler）

**定位：** 在 **标准 PodGroup Gang** 上增加 **Workload 级拓扑共域**；跨副本打散仍主要依赖 **Pod 级** `topologySpreadConstraints` / `podAffinity`，**没有** PodGroup 级「拓扑组互斥」或「同 PodGroup 内 SubGroup 互斥」API。

| 资料 | 链接 |
|------|------|
| Topology-Aware Workload Scheduling（TAS 概念） | https://kubernetes.io/docs/concepts/workloads/workload-api/topology-aware-scheduling/ |
| PodGroup API（`scheduling.k8s.io/v1alpha2`） | https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-group-v1alpha2/ |
| Pod Topology Spread | https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/ |
| Pod Affinity / Anti-Affinity | https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#affinity-and-anti-affinity |
| Gang 调度算法（placement-based） | https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling/pod-group-scheduling/ |

#### 用法 1：PodGroup Gang + 单条拓扑共域（TAS）

**适用：** 分布式训练等「整组必须落在同一 rack/zone label 域」。

**要点（v1.36 alpha）：**

- `schedulingPolicy.gang.minCount`：整组 **一次性模拟** 放置，满足 `minCount` 才 commit。
- `schedulingConstraints.topology`：**每个 PodGroup 仅允许一条** topology；`key` 为 **Node label**（非 CRD 树）。
- 语义是 **共域（colocation）**，不是「子组互斥」或「多 PodGroup 互斥」。
- TAS **不支持** 为拓扑触发抢占；无可行域则整组 Unschedulable。

```yaml
apiVersion: scheduling.k8s.io/v1alpha2
kind: PodGroup
metadata:
  name: example-podgroup
spec:
  schedulingPolicy:
    gang:
      minCount: 4
  schedulingConstraints:
    topology:
      - key: topology.example.com/rack   # 全部 Pod 共享同一 rack label 值
```

**与本设计：** 近似 Volcano `PodGroupSpec.networkTopology`（Job 级 envelope），但 Volcano 用 HyperNode `tierName` 而非任意 node label；且 Volcano 可叠加 **组间** `topologyAffinity` / `subGroupTopologyAffinity`。

#### 用法 2：Pod Topology Spread（副本打散）

**适用：** Deployment / 无 Gang 的副本 **尽量均匀** 分布在 zone/rack。

```yaml
spec:
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: DoNotSchedule
      labelSelector:
        matchLabels:
          app: llama-70b
```

**局限：**

- **Pod 级**、逐 Pod 调度；与 PodGroup Gang **混用困难**（先绑定的 Pod 会锁定 spread 域）。
- 无 `topologyGroup` 概念；多 **独立 PodGroup instance** 的「各占不同超节点」需靠 label + spread **间接** 表达，运维成本高。

#### 用法 3：Pod Affinity / Anti-Affinity

**适用：** 细粒度「跟某 Pod 同节点/同 hostname」。

**局限（Gang 场景）：** KAI 拓扑设计文档明确指出：带 PodAffinity 的 PodGroup 在调度第一个 Pod 后会把 label 域 **锁死**，后续 Pod 无法回退尝试其它节点，易导致 **整组无解**。Volcano / KAI 均倾向 **PodGroup 级拓扑插件 + 整组模拟**，而非 Pod 级 affinity 链式锁定。

#### 与本设计差距（摘要）

| 本设计诉求 | Kubernetes 常见写法 | 差距 |
|------------|---------------------|------|
| 多 inference instance 各占不同超节点 | spread + 统一 app label | Pod 级；无 PodGroup `topologyGroup` |
| Prefill-Decode 分片分机柜 + 共超节点 | 多条 Pod 规则或无法表达 | 无 SubGroup / 无组间 Term |
| 同 PodGroup 内 prefill-0 vs prefill-1 互斥 | 需自建 label + spread | 无 `subGroupAntiAffinity` |

---

### Kueue

**定位：** **队列 + 准入（admission）** 阶段的拓扑感知；用 **Node label 层级** 描述数据中心结构，用户通过 **Job PodTemplate 注解** 声明 PodSet 共域/偏好，**不是** Volcano 式 HyperNode CRD，也 **没有** PodGroup 级跨 Workload 反亲和 API。

| 资料 | 链接 |
|------|------|
| Topology Aware Scheduling（概念与注解） | https://kueue.sigs.k8s.io/docs/concepts/topology_aware_scheduling/ |
| `Topology` / `ResourceFlavor` API | 同上（Admin-facing APIs） |
| TAS 与 Cluster Autoscaler | 同上（Provisioning AdmissionCheck） |
| v0.17 多层 slice（alpha） | 同上（`TASMultiLayerTopology`） |

#### 管理员：定义拓扑层级

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: Topology
metadata:
  name: default
spec:
  levels:
    - nodeLabel: cloud.provider.com/topology-block
    - nodeLabel: cloud.provider.com/topology-rack
    - nodeLabel: kubernetes.io/hostname
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: tas-flavor
spec:
  topologyName: default
  nodeLabels:
    cloud.provider.com/node-group: tas-group
```

调度前 Kueue 按层级计算各域 **空闲容量**（扣除已准入 TAS Workload 与其它 Pod 占用）。

#### 用户：PodSet 拓扑注解（Job `template.metadata.annotations`）

| 注解 | 语义 |
|------|------|
| `kueue.x-k8s.io/podset-required-topology` | **Hard**：该 PodSet 全部 Pod 必须在注解值所指 **同一拓扑域**（如 rack）；放不下则不准入 |
| `kueue.x-k8s.io/podset-preferred-topology` | **Soft**：优先共域；不行则向上一层扩散，最终可跨域准入 |
| `kueue.x-k8s.io/podset-unconstrained-topology` | 参与 TAS 容量计算，但不强制共域（减碎片） |
| `kueue.x-k8s.io/podset-group-name` | 多个 PodSet **共享** 同一 flavor 与拓扑域（类似「绑在一起准入」） |
| `kueue.x-k8s.io/podset-slice-required-topology-constraints` | 多层 slice（最多 3 层，需 feature gate） |

**示例（preferred @ block）：**

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  generateName: tas-sample-preferred
  labels:
    kueue.x-k8s.io/queue-name: tas-user-queue
spec:
  parallelism: 40
  template:
    metadata:
      annotations:
        kueue.x-k8s.io/podset-preferred-topology: cloud.provider.com/topology-block
    spec:
      containers:
        - name: worker
          image: registry.k8s.io/e2e-test-images/agnhost:2.53
          resources:
            requests:
              cpu: "1"
              memory: 200Mi
```

**多层 slice 示例（64 Pod：block 内 32、rack 内 16）：**

```yaml
metadata:
  annotations:
    kueue.x-k8s.io/podset-slice-required-topology-constraints: |
      [
        {"topology": "cloud.provider.com/topology-block", "size": 32},
        {"topology": "cloud.provider.com/topology-rack", "size": 16}
      ]
```

#### 与本设计关系

| 维度 | Kueue | Volcano 本设计 |
|------|-------|----------------|
| 调度阶段 | **准入前** 选域，再交给 kube-scheduler 绑 Node | Volcano Session 内 HyperNode gradient + allocate |
| 多 PodGroup 互斥 | 靠多个 Workload / 运维约定，**无** `podGroupAntiAffinity` | `topologyAffinity` + `topologyGroup` |
| 同 PodGroup 内 prefill/decode | `podset-group-name` 绑多个 PodSet **共域**；**无** policy 内 pairwise 反亲和 | `subGroupTopologyAffinity` + `matchLabelKeys` |
| 拓扑模型 | Node label 链 | HyperNode 树 + `separationTierName` |

KAI 拓扑插件 **显式参考 Kueue Topology CRD**（见下节），Volcano 与 Kueue **路径不同**（HyperNode vs label），但 **required/preferred 分层思想** 与 Volcano `networkTopology.mode` / 组间 `required`·`preferred` 可对齐理解。

---

### KAI Scheduler（NVIDIA）

**定位：** AI 训练/推理场景下的 **Gang + 独立 topology 插件**；**分层 PodGroup（SubGroups）** 表达组件差异；拓扑通过 **Job 注解 → PodGroup.TopologyConstraint** 注入。与 Volcano 本设计 **架构最接近**，但 **缺少** 跨 PodGroup hard 反亲和、同 PodGroup 内显式 `subGroupAntiAffinity` Term。

| 资料 | 链接 |
|------|------|
| Topology Aware Scheduling 设计 | https://github.com/kai-scheduler/KAI-scheduler/blob/main/docs/developer/designs/topology-awareness/README.md |
| Hierarchical PodGroup / SubGroups | https://github.com/kai-scheduler/KAI-scheduler/blob/main/docs/developer/designs/hierarchical-podgroup/README.md |
| PodGroup CRD 类型（v2alpha2） | https://github.com/kai-scheduler/KAI-scheduler/blob/main/pkg/apis/scheduling/v2alpha2/podgroup_types.go |
| 跨 Workload 手写 PodGroup 讨论 | https://github.com/kai-scheduler/kai-scheduler/issues/1420 |
| Run:ai 用户文档（TAS 概念） | https://run-ai-docs.nvidia.com/saas/platform-management/aiinitiatives/resources/topology-aware-scheduling |
| Kueue Topology（KAI 对齐参考） | https://kueue.sigs.k8s.io/docs/concepts/topology_aware_scheduling/ |

#### 用法 1：Job 注解 → PodGroup 拓扑约束

`pod-grouper` 从 **顶层 Owner** 读取注解并写入 PodGroup：

| 注解 | 含义 |
|------|------|
| `kai.scheduler/topology` | 使用的 Topology CRD 名称 |
| `kai.scheduler/topology-required-placement` | **Hard**：整 Job 不得跨越该层级（如 `zone`） |
| `kai.scheduler/topology-preferred-placement` | **Soft**：优先该层级（如 `rack`） |

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: topology-aware-job
  annotations:
    kai.scheduler/topology: network
    kai.scheduler/topology-required-placement: zone
    kai.scheduler/topology-preferred-placement: rack
```

对应 PodGroup 字段（设计草案）：

```yaml
spec:
  topologyConstraint:
    topology: network
    requiredTopologyLevel: zone
    preferredTopologyLevel: rack
```

**调度实现（两阶段）：**

1. **Stage 1**：维护 `TopologyInfo` 树，按域汇总资源；`NodeOrder` / `Predicate` 按距离排序（解决 PodAffinity 在 Gang 下锁死问题）。
2. **Stage 2**：`FeasibleNodes(PodGroup) [][]Node` **枚举**可行拓扑域并 simulation（精度高、大集群成本高）。

#### 用法 2：分层 SubGroup（Prefill / Decode 等）

**资料：** [Hierarchical PodGroup README](https://github.com/kai-scheduler/KAI-scheduler/blob/main/docs/developer/designs/hierarchical-podgroup/README.md)

Pod 通过 label `kai.scheduler/subgroup-name` 归属 **叶子 SubGroup**；父 SubGroup 可设 `minSubGroup` 与 **自己的** `topologyConstraint`。

**示例（User Story 3：decode / prefill 各在 block 内，rack 上 Gang）：**

```yaml
spec:
  minSubGroup: 2
  subGroups:
    - name: decode
      minSubGroup: 2
      topologyConstraint:
        topology: cluster-topology
        requiredTopologyLevel: block
    - name: decode-workers
      parent: decode
      minMember: 4
      topologyConstraint:
        requiredTopologyLevel: rack
    - name: prefill
      minSubGroup: 2
      topologyConstraint:
        requiredTopologyLevel: block
    - name: prefill-workers
      parent: prefill
      minMember: 4
      topologyConstraint:
        requiredTopologyLevel: rack
```

**表达力对比：**

| 诉求 | KAI 典型写法 | Volcano 本设计 |
|------|--------------|----------------|
| prefill 与 decode **可** 不同 block | 父 SubGroup 各 `requiredTopologyLevel: block` | 默认 **不** 配跨角色 anti；共超节点用 Job `networkTopology` 或 `subGroupAffinity` |
| 4 个 prefill **分片** 各在不同 rack | 需 **多个叶子 SubGroup** 或副本 SubGroup 集（见 Example 3/4） | **一条** `subGroupPolicy` + `matchLabelKeys` + policy 内 `[prefill]` anti（实例 4） |
| 多 instance 各占不同超节点 | **多个 PodGroup**，各自 topology；无 `topologyGroup` | `topologyAffinity.podGroupAntiAffinity` @ `supernode` |
| prefill vs decode **强制分机柜** | 无 `antiSubGroupSelector`；靠层级与 placement 间接实现 | 实例 6：`subGroupAntiAffinity` 跨 policy |

#### 与本设计差距（摘要）

- **有：** 拓扑树、`required`/`preferred` level、分层 SubGroup、独立 topology 插件、Gang 整组模拟。
- **无：** `topologyGroup`；`podGroupAntiAffinity`；`subGroupAntiAffinity` 的 **policy 内两两互斥** Term；`separationTierName` 与 HyperNode 统一（KAI 用 Topology CRD level 字符串）。

---

### Koordinator

**定位：** 在 **scheduler-plugins Coscheduling** 上扩展 **网络拓扑感知**；通过 **`ClusterNetworkTopology` CR** + **PodGroup/Pod 注解 JSON** 配置 **PreferGather / MustGather**；支持 **多 PodGroup 编组**（`groups` 注解）做联合 gather，仍 **不是** 声明式「A 与 B 在 tier T 上必须异域」。

| 资料 | 链接 |
|------|------|
| Network Topology Aware Scheduling（用户手册） | https://koordinator.sh/docs/user-manuals/network-topology-aware-scheduling |
| Coscheduling 插件（scheduler-plugins） | https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/site/content/en/docs/plugins/coscheduling.md |
| `ClusterNetworkTopology` API（v1alpha1） | https://koordinator.sh/docs/user-manuals/network-topology-aware-scheduling#configure-network-topology |

**启用条件（文档要求）：**

- `koord-scheduler` 启动参数：`--enable-network-topology-manager=true`
- Coscheduling 插件配置：`awareNetworkTopology: true`（可选 `enablePreemption: true`）

#### 管理员：拓扑 CR + Node label

```yaml
apiVersion: scheduling.koordinator.sh/v1alpha1
kind: ClusterNetworkTopology
metadata:
  name: default
spec:
  networkTopologySpec:
    - labelKey:
        - network.topology.nvidia.com/spine
      topologyLayer: SpineLayer
    - labelKey:
        - network.topology.nvidia.com/block
      parentTopologyLayer: SpineLayer
      topologyLayer: BlockLayer
    - parentTopologyLayer: BlockLayer
      topologyLayer: NodeTopologyLayer
```

Node 示例：`network.topology.nvidia.com/block`、`.../spine` 等 label（可用 NVIDIA topograph 等工具打标）。

#### 用户：PodGroup `network-topology-spec`（gather 策略）

**PreferGather（尽量聚合，资源紧时仍可调度）：**

```yaml
apiVersion: scheduling.sigs.k8s.io/v1alpha1
kind: PodGroup
metadata:
  name: topology-demo-job
  annotations:
    gang.scheduling.koordinator.sh/network-topology-spec: |
      {
        "gatherStrategy": [
          { "layer": "BlockLayer", "strategy": "PreferGather" },
          { "layer": "SpineLayer", "strategy": "PreferGather" }
        ]
      }
spec:
  minMember: 4
```

Pod 需：`schedulerName: koord-scheduler`、label `pod-group.scheduling.sigs.k8s.io: <pg名>`、注解 `gang.scheduling.koordinator.sh/network-topology-index: "0"`…（建立组内通信序号）。

**MustGather（硬共域，不满足则 Pending / 拓扑感知抢占）：**

```yaml
annotations:
  gang.scheduling.koordinator.sh/network-topology-spec: |
    {
      "gatherStrategy": [
        { "layer": "SpineLayer", "strategy": "MustGather" }
      ]
    }
```

文档场景：4 Pod 训练 Job 必须落在 **同一 Spine**；资源不足时 **按拓扑约束抢占** 低优先级 Pod，并用 `nominatedNodeName` 预留节点。

| 策略 | 语义 | 近似 Volcano |
|------|------|--------------|
| `PreferGather` | 优先聚合到同一 layer 域 | `networkTopology.mode: soft` |
| `MustGather` | 必须整组落在同一 layer 域 | `networkTopology.mode: hard` 或 Job 级 envelope |
| `podCountMultiple` | Block 层按 Pod 数倍聚集（TP 训练） | 组内 `subGroupSize` + Gang |

#### 用法：多 PodGroup 联合拓扑（`groups`）

适用于 **master + worker** 等多 PodGroup 必须 **一起** MustGather 到同一 Spine：

```yaml
apiVersion: scheduling.sigs.k8s.io/v1alpha1
kind: PodGroup
metadata:
  annotations:
    gang.scheduling.koordinator.sh/groups: |
      ["default/llm-master", "default/llm-worker"]
    gang.scheduling.koordinator.sh/network-topology-spec: |
      {
        "gatherStrategy": [
          { "layer": "SpineLayer", "strategy": "MustGather" }
        ]
      }
```

**与本设计：** `groups` 解决的是 **多个 PodGroup 共域**（类似 Volcano 对多个对象同时施加亲和）；Volcano 用 `topologyAffinity.podGroupAffinity` / 共享 `topologyGroup` + 反亲和表达 **共域 / 异域** 更对称。Koordinator **无**「同一 PodGroup 内 prefill-0 与 prefill-1 分机柜」一类 **pairwise** API。

---

### scheduler-plugins（Kubernetes SIG）

**定位：** 提供 **Coscheduling**（PodGroup `minMember` + Permit），**不包含** 拓扑亲和/反亲和 CRD；拓扑能力在 **Koordinator 发行版** 中通过 Coscheduling 扩展实现。

| 资料 | 链接 |
|------|------|
| Coscheduling 插件文档 | https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/site/content/en/docs/plugins/coscheduling.md |
| 仓库首页 | https://github.com/kubernetes-sigs/scheduler-plugins |

**典型用法（仅 Gang）：**

```yaml
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: PodGroup
metadata:
  name: pg1
spec:
  minMember: 3
  scheduleTimeoutSeconds: 10
```

拓扑需另接 Koordinator 注解或 kube-scheduler TAS / Pod spread。

---

### Volcano（现状与本补丁）

| 资料 | 链接 |
|------|------|
| Network Topology Aware Scheduling（组内） | [Network Topology Aware Scheduling](./Network%20Topology%20Aware%20Scheduling.md) |
| Preempt 与拓扑 | [Preempt Action Support Topology](./preempt-action-support-topology.md) |
| 本设计（组间） | 本文档 |

**已有（`network-topology-aware`）：** HyperNode 树、`PodGroupSpec.networkTopology`、`subGroupPolicy` + `matchLabelKeys`、`subGroupPolicy[].networkTopology`、`allocateForJob` / SubJob 两级 Gang。

**本设计新增（`group-topology-affinity`）：** `topologyAffinity`（跨 PodGroup）、`subGroupTopologyAffinity`（同 PodGroup 跨 SubGroup）；Framework 拓扑 gradient **交集**；allocate **资源预筛** 与拓扑解耦。

---

## 场景对照：友商写法 vs 本设计

| 业务场景 | Kubernetes | Kueue | KAI | Koordinator | Volcano 本设计 |
|----------|------------|-------|-----|-------------|----------------|
| 训练 Job 整组同 rack | PodGroup `topology.key=rack` | `podset-required-topology: rack` | `topology-required-placement: rack` | MustGather @ BlockLayer | `networkTopology` @ cabinet/rack tier |
| 40 副本尽量同 block | Pod spread | `podset-preferred-topology` | `preferredTopologyLevel` | PreferGather | `networkTopology.mode: soft` |
| 多 inference instance 分超节点 | spread + label | 多 Job / 运维隔离 | 多 PodGroup | 多 PodGroup + 不同 gather | `topologyGroup` + `podGroupAntiAffinity` |
| PD：分片分机柜 + 共超节点 | 难 | `podset-group-name` 仅共域 | 多 SubGroup + 父级 block | 单 PG gather + 多 PG groups | **实例 4** |
| PD：prefill/decode 强制分柜 | Pod anti-affinity | 难 | 层级 constraint 组合 | 多 layer gather | **实例 6** |
| 分片尽量分柜、可降级 | spread `ScheduleAnyway` | `podset-preferred-topology` | preferred level | PreferGather | **实例 7** `preferred` + `weight` |

---

## Volcano 差异化（相对友商）

1. **组间与组内 API 分离：** `networkTopology`（组内 Gang）vs `topologyAffinity` / `subGroupTopologyAffinity`（组间），避免 Koordinator 式「全写进注解 JSON」或 K8s 式「只有共域一条 topology」。
2. **HyperNode `separationTierName`：** 组间 hard/soft 与组内 `highestTierName` 共用集群 tier 命名；对齐运维 HyperNode CR，而非仅 node label。
3. **同 PodGroup 内 policy 级 Term：** `subGroupSelector` / `antiSubGroupSelector` 支持 **policy 内两两互斥**（实例 4），无需 KAI 式为每个分片复制 SubGroup 树。
4. **跨 PodGroup 显式反亲和：** `topologyGroup` + `podGroupAntiAffinity.required`，补 KAI/Koordinator/Kueue 缺口。
5. **插件分工：** `network-topology-aware` + `group-topology-affinity`；hard 拓扑 gradient **多插件交集**（借鉴 KAI 多约束思想，接口与 Volcano Framework 一致）。

---

## 字段映射表

### 表 1：组内拓扑共域（Colocation / Gang 域内）

| 语义 | Volcano（已有） | Volcano（本设计，不变） | K8s PodGroup TAS | KAI PodGroup |
|------|-----------------|-------------------------|------------------|--------------|
| 约束模式 | `networkTopology.mode` hard/soft | 同左 | 隐含 hard（共域） | required + preferred |
| 不跨越的 tier 上限 | `highestTierAllowed` / `highestTierName` | 同左 | `topology[].key`（node label） | `requiredTopologyLevel`（最大层级） |
| 拓扑数据源 | HyperNode CRD | 同左 | Node labels | Topology CRD |
| 组内 Gang | `minMember` + `subGroupSize` / `minSubGroups` | 同左 | `gang.minCount` | `minMember` / SubGroup `minMember` |

**映射说明：**

- K8s `topology[].key` ≈ 在某一 **label 域** 内共域；Volcano `highestTierAllowed` ≈ 在 HyperNode 树 **某 tier 祖先** 内共域。
- KAI `requiredTopologyLevel: rack` ≈ Volcano `highestTierAllowed` 指向 tierName=`rack` 的 HyperNode 层（需集群 tier 命名一致）。

### 表 2：跨 PodGroup / 多 Instance（组间拓扑亲和 / 反亲和）

| 语义 | Volcano（本设计） | K8s 近似 | KAI 近似 |
|------|-------------------|----------|----------|
| 逻辑分组 | `topologyGroup` 或 `podGroupSelector` | `topologySpreadConstraints.labelSelector` + 工作负载 label | 多个 PodGroup + 相同 queue/label |
| 分离边界 | `separationTier` / `separationTierName` | `topologySpreadConstraints.topologyKey` | 各 PodGroup 的 `requiredTopologyLevel`（共域式，非互斥） |
| Hard 互斥 | `podGroupAntiAffinity.required[]` | `whenUnsatisfiable: DoNotSchedule` + skew | **无直接等价** |
| Soft 偏好 | `podGroupAntiAffinity.preferred[]` + weight | `ScheduleAnyway` + skew | `preferredTopologyLevel` |

**迁移示例（概念）：**

K8s 多副本跨 zone 打散（Deployment）：

```yaml
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        app: llama-70b
```

Volcano 多 PodGroup instance（Gang + 超节点互斥）：

```yaml
metadata:
  labels:
    volcano.sh/topology-group: llama-70b
spec:
  topologyAffinity:
    podGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - topologyGroup: llama-70b
          separationTierName: supernode
```

### 表 3：跨 SubGroup（**仅同 PodGroup 内**，Prefill-Decode 等）

> API 容器：`subGroupTopologyAffinity`。**不**跨 PodGroup；多 instance 互斥见表 2 `topologyAffinity`。

| 语义 | Volcano（本设计） | KAI Hierarchical PodGroup | K8s |
|------|-------------------|---------------------------|-----|
| 子组识别 | `subGroupPolicy[].name` + `matchSubGroupPolicyNames` | `subGroups[].name` + `kai.scheduler/subgroup-name` label | 无 |
| 组间拓扑 API 容器 | `subGroupTopologyAffinity` | 无（各 SubGroup 独立 constraint） | 无 |
| 子组间共域（亲和） | `subGroupAffinity` 或 `PodGroupSpec.networkTopology`（见实例 4） | 父 SubGroup `topologyConstraint.requiredTopologyLevel`（如 block） | Pod affinity |
| 子组间互斥（反亲和） | `subGroupTopologyAffinity.subGroupAntiAffinity` + `subGroupSelector` / `antiSubGroupSelector` | **无显式字段**；Story 3 靠不同子树 placement | `podAntiAffinity` |
| 子组内 Gang | `subGroupPolicy.networkTopology` + `subGroupSize` | SubGroup `minMember` + `topologyConstraint` | 单条 PodGroup topology |

**Prefill-Decode 场景映射：**

| 诉求 | Volcano | 说明 |
|------|---------|------|
| 4+2 分片、2 条 policy、共超节点 | **实例 4** | 共超节点写法见实例 4 方式一/二；分片反亲和 |
| 多 instance + 实例 4 | **实例 5** | `topologyAffinity` @ supernode |
| 仅需分机柜、无共超节点 | 省略 Job 级别 `networkTopology` 与 `subGroupAffinity` | 仅 subGroupPolicy 内反亲和 |
| 分片尽量分机柜、允许降级 | **实例 7** | `subGroupAntiAffinity.preferred` + `weight` |
| prefill 与 decode 无要求 | 省略跨角色 antiAffinity | 实例 4 仍可用 Job 级别 `networkTopology` 共 supernode |
| 仅 2 条 policy 整块角色分机柜+共超节点 | 实例 6 | |

> **结论**：`subGroupTopologyAffinity` 表达 **SubJob（policy）之间** 关系；推荐 **一条 policy + matchLabelKeys**（实例 4），无需按分片拆多条 policy。

### 表 4：插件与调度路径

| 语义 | Volcano | KAI | kube-scheduler |
|------|---------|-----|----------------|
| 域内 Gang + gradient | `network-topology-aware` | topology 插件 Stage 2 simulation | PodGroup placement algorithm |
| 域间组间拓扑亲和 | `group-topology-affinity`（新） | topology 插件 Stage 1 filter/order | `PodTopologySpread` plugin |
| 占用索引 | `TopologyOccupancyIndex` | `TopologyInfo` 树 | `PodTopologySpread` PreFilter 状态 |
| HyperNode 候选 | 拓扑：多插件 gradient 交集 + 重分层；容量：**allocate 资源预筛**（非 Gradient 回调） | `FeasibleNodes(PodGroup)` 规划 | N/A |

### 表 5：tier / level 命名对齐（运维配置）

**前提：** `separationTierName`、`networkTopology.highestTierName` 的值均来自本集群 HyperNode **`spec.tierName`**（字符串完全一致），不是 Volcano 内置枚举。见 [#hypernode-层级与-separationtiername](#hypernode-层级与-separationtiername)。

建议流程：部署 HyperNode → 维护集群 tierName 对照表 → PodGroup 只引用表中字符串 → Webhook 校验 `separationTierName ∈ HyperNodeTierNameMap`。

| 物理含义（示例） | HyperNode `spec.tierName`（集群实际值） | PodGroup 写法 | KAI `requiredTopologyLevel` |
|------------------|----------------------------------------|---------------|-----------------------------|
| 超节点 / 大块 | `supernode` 或 `block` 等 | `separationTierName: supernode` | `block` / `zone` |
| 机柜 / rack | `cabinet` / `rack` / `volcano.sh/tor-1` 等 | `separationTierName: cabinet` | `rack` |
| 节点 | tier=0 或 Node 成员 | 一般用更高层做组间分离 | `node` |

> 复制文档 YAML 前执行：`kubectl get hypernodes -o custom-columns=NAME:.metadata.name,TIER:.spec.tier,TIERNAME:.spec.tierName`，确认 `TIERNAME` 列与 PodGroup 中填写一致。

---

## 标准对齐建议

1. **术语**：`separationTierName` 对齐本集群 HyperNode `spec.tierName`（非 K8s 内置 key）；`separationTier` 对齐 K8s “topology domain” 中「按层划分」的概念；`topologyGroup` 对齐 topology spread 的 “同一 spread 组”。
2. **硬/软**：组间拓扑用 **`required` / `preferred`** 对齐 K8s PodAffinity；`networkTopology.mode` 对齐 K8s `whenUnsatisfiable` 与域内 Gang 的 hard/soft。
3. **IgnoredDuringExecution**：与 PodAffinity / KAI 一致，已调度 Pod 不因约束变化驱逐（与现有 Volcano 拓扑调度一致）。
4. **后续可选**：提供转换工具或文档，将 KAI 注解 `kai.scheduler/topology-required-placement` 映射为 Volcano `TopologySeparationSpec`（只读文档即可，非必须代码）。

---

## 竞品结论（摘要）

| 问题 | 结论 |
|------|------|
| 是否有完全相同的 API？ | **无**。K8s：PodGroup 单条 topology 共域 + Pod spread；Kueue：准入阶段 PodSet 注解；KAI：SubGroup `topologyConstraint`；Koordinator：gather/MustGather 注解。均 **无** Volcano 式 `subGroupAntiAffinity` policy 内互斥 + `topologyGroup` 跨 PodGroup hard 反亲和。 |
| 最接近方案？ | **KAI Scheduler**（拓扑树 + 分层 SubGroup + 独立 topology 插件 + Gang 模拟）。其次 **Koordinator**（Gang + 多级 gather，偏共域与抢占）。 |
| 迁移时优先对照谁？ | 组内 Gang → K8s TAS / Kueue required / Koordinator MustGather / KAI required level；多 instance → Kueue 多 Workload 或本设计 `topologyAffinity`；PD 分片 → KAI 多 SubGroup 或本设计 **实例 4**。 |
| Volcano 差异化？ | 见 [Volcano 差异化（相对友商）](#volcano-差异化相对友商)。 |
| 插件拆分是否合理？ | **是**；与 KAI 多 topology 插件思路一致；Framework **Gradient 交集 + 统一分层**，与 `HyperNodeOrderFn` 累加对称。 |

## 外部参考资料索引

按产品分类，便于评审时跳转（与上文 [友商洞察（详细）](#友商洞察详细) 一致）。

**Kubernetes**

- [Topology-Aware Workload Scheduling](https://kubernetes.io/docs/concepts/workloads/workload-api/topology-aware-scheduling/)
- [PodGroup API (v1alpha2)](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-group-v1alpha2/)
- [Pod Topology Spread Constraints](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/)
- [Assign Pods to Nodes — Affinity](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#affinity-and-anti-affinity)

**Kueue**

- [Topology Aware Scheduling](https://kueue.sigs.k8s.io/docs/concepts/topology_aware_scheduling/)

**KAI Scheduler**

- [Topology Awareness Design](https://github.com/kai-scheduler/KAI-scheduler/blob/main/docs/developer/designs/topology-awareness/README.md)
- [Hierarchical PodGroup Design](https://github.com/kai-scheduler/KAI-scheduler/blob/main/docs/developer/designs/hierarchical-podgroup/README.md)
- [Issue #1420 — Cross-workload PodGroup](https://github.com/kai-scheduler/kai-scheduler/issues/1420)
- [Run:ai — Topology Aware Scheduling](https://run-ai-docs.nvidia.com/saas/platform-management/aiinitiatives/resources/topology-aware-scheduling)

**Koordinator / scheduler-plugins**

- [Koordinator — Network Topology Aware Scheduling](https://koordinator.sh/docs/user-manuals/network-topology-aware-scheduling)
- [scheduler-plugins — Coscheduling](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/site/content/en/docs/plugins/coscheduling.md)

**Volcano**

- [Network Topology Aware Scheduling](./Network%20Topology%20Aware%20Scheduling.md)
- [Preempt Action Support Topology](./preempt-action-support-topology.md)

---

# Validation Rules (Webhook)

**通用**

1. `separationTier` 与 `separationTierName` 互斥；至少配置其一。若使用 `separationTierName`，其值必须存在于 Session `HyperNodeTierNameMap`（即至少一个 HyperNode 的 `spec.tierName` 与之 **完全相等**）。**`topologyAffinity` / `subGroupTopologyAffinity` 的 term 内禁止 `mode` 字段**（hard/soft 由 `required` / `preferred` 列表决定，见 [#required--preferred-与-mode不重复](#required--preferred-与-mode不重复)）。

**`topologyAffinity`（跨 PodGroup）**

2. 配置 `podGroupAntiAffinity` / `podGroupAffinity` term 时，`topologyGroup` 与 `podGroupSelector` 至少其一。
3. `podGroupSelector` 不得仅匹配本 PodGroup 自身来模拟 SubGroup 关系（应使用 `subGroupTopologyAffinity`）。

**`subGroupTopologyAffinity`（同 PodGroup、跨 SubGroup）**

4. 若 `subGroupTopologyAffinity` 非空，则 `subGroupPolicy` 非空且 `len(subGroupPolicy) >= 2`。
5. 所有 `matchSubGroupPolicyNames` 必须是 **本 PodGroup** `subGroupPolicy[].name`（**禁止**写 SubJobID 分片后缀如 `prefill-0`）。
6. `subGroupTopologyAffinity` term 中 **禁止** 出现 `topologyGroup`、`podGroupSelector`、`namespaceSelector`（跨 PodGroup 字段仅属于 `topologyAffinity`）。
7. `subGroupAntiAffinity`：**跨 policy** 时 `subGroupSelector` 与 `antiSubGroupSelector` 的 policy name 集合 **不相交**；**policy 内两两互斥** 时允许两侧填写 **相同** policy name（如均为 `[prefill]`，实例 4）。
8. `subGroupAffinity.required` 中每条 `matchSubGroupPolicyNames` 至少包含 **2 个不同** policy name（如 `[prefill, decode]`，覆盖其下全部 SubJob）。
9. 使用 policy 内互斥时，该 policy 应配置 `matchLabelKeys` 且运行时 SubJob 数量 ≥ 2（否则 Webhook 警告）。
10. hard `subGroupAffinity` 的 tier ≥ hard `subGroupAntiAffinity` 的 tier（数值比较，或 tierName 映射后比较）。

**组合**

11. `topologyAffinity` 与 `subGroupTopologyAffinity` 可同时存在；Webhook 分别校验，调度时 **AND**。
12. `subGroupTopologyAffinity` 与 `subGroupPolicy[].networkTopology` 同时存在时，在文档/Condition 中说明组间 + 组内语义；Webhook 检测明显矛盾的 tier 组合（可选告警）。
13. 若 `PodGroupSpec.networkTopology`（`mode: hard`）与 `subGroupAffinity` 的 **`required`** term 在 **同一 separation tier**（如均为 `supernode`）表达「共域」，Webhook **警告** 冗余（见 [实例 4](#实例-4分布式-prefill-decode-推理推荐)，方式一与方式二勿重复配置）。

---

# Status (Optional)

```go
const PodGroupTopologyUnsatisfiable PodGroupConditionType = "TopologyUnsatisfiable"
```

| Reason | 场景 |
|--------|------|
| `PodGroupAntiAffinityUnsatisfiable` | 无可用 supernode 域 |
| `SubGroupAntiAffinityUnsatisfiable` | Prefill-Decode 分机柜失败 |
| `SubGroupAffinityUnsatisfiable` | 无法与 peer 共超节点 |

---

# Implementation Phases

| Phase | 内容 |
|-------|------|
| P1 | API、Framework **拓扑** gradient 交集/重分层、allocate **资源预筛**、group-topology-affinity、webhook、e2e |
| P2 | preempt/backfill、enqueue 预检、SubJob annotation |
| P3 | Node 级 Predicate 兜底、动态 occupancy、gradient 聚合错误策略可配置 |

---

# References

完整分类索引见 [#外部参考资料索引](#外部参考资料索引)。以下为常用入口：

- Kubernetes: [Topology-Aware Workload Scheduling](https://kubernetes.io/docs/concepts/workloads/workload-api/topology-aware-scheduling/) · [PodGroup Scheduling](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling/pod-group-scheduling/)
- Kueue: [Topology Aware Scheduling](https://kueue.sigs.k8s.io/docs/concepts/topology_aware_scheduling/)
- KAI: [Topology Awareness](https://github.com/kai-scheduler/KAI-scheduler/blob/main/docs/developer/designs/topology-awareness/README.md) · [Hierarchical PodGroup](https://github.com/kai-scheduler/KAI-scheduler/blob/main/docs/developer/designs/hierarchical-podgroup/README.md)
- Koordinator: [Network Topology Aware Scheduling](https://koordinator.sh/docs/user-manuals/network-topology-aware-scheduling)
- Volcano: [Network Topology Aware Scheduling](./Network%20Topology%20Aware%20Scheduling.md)
