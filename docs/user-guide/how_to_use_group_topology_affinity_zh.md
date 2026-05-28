# 组间拓扑亲和（Group Topology Affinity）用户使用指南

> 英文版：[How to Use Group Topology Affinity](./how_to_use_group_topology_affinity.md)

## 1 背景

[网络拓扑感知调度](./how_to_use_network_topology_aware_scheduling.md)（`network-topology-aware` 插件）解决的是 **组内** 放置：让 Pod 或 SubJob 落在同一 HyperNode 性能域内（例如在单机柜内 Gang，或整 Job 不跨某一层级）。

生产环境还常见 **组间** 拓扑诉求：

- **跨 PodGroup：** 多个推理 **instance**（每个 instance 对应一个 PodGroup）不应落在同一超节点，避免单点故障拖垮全部在线副本。
- **同一 PodGroup 内：** Prefill–Decode 或多分片推理需要 **分片分机柜**、**整机共超节点**，仅靠 Pod 级 `podAffinity` 难以清晰表达。

**组间拓扑亲和** 在 PodGroup 上增加字段，并由调度插件 **`group-topology-affinity`** 生效；与 **`network-topology-aware`** 配合使用：组内用 `networkTopology`，组间用 `topologyAffinity` 与 `subGroupTopologyAffinity`。

API 与设计细节见 [组间拓扑亲和设计提案](../design/group-topology-affinity.md)（中文）。

## 2 功能说明

### 2.1 三层拓扑规则

| 层级 | 字段 | 作用域 | 典型目标 |
|------|------|--------|----------|
| 组内 | PodGroup / `subGroupPolicy` 上的 `networkTopology` | 同一 policy 内 Pod / SubJob | 机柜内 Gang；整 Job 不跨某 tier |
| 组间（同 PodGroup） | `subGroupTopologyAffinity` | 不同 `subGroupPolicy` 拆出的 SubJob | 分片分机柜；prefill/decode 共超节点等 |
| 组间（跨 PodGroup） | `topologyAffinity.podGroupAntiAffinity` | 其它 PodGroup | 多 instance 各占不同超节点 |

硬性约束使用 `requiredDuringSchedulingIgnoredDuringExecution`；软性偏好使用 `preferredDuringSchedulingIgnoredDuringExecution` 及 `weight`。

### 2.2 跨 PodGroup 反亲和与 `podGroupSelector`

通过 Kubernetes 标准 **`podGroupSelector`**（`metav1.LabelSelector`）匹配 **PodGroup `metadata.labels`** 选定 peer。Volcano **不提供** 专用 `topologyGroup` 字符串字段。

**label 须由用户在创建 PodGroup 时自行写入**；需要在某 tier 上彼此避让的 PodGroup，应对该 label **键使用相同取值**，并在 term 的 `podGroupSelector.matchLabels`（或 `matchExpressions`）中引用。

**label 赋值建议：**

| 项 | 说明 |
|----|------|
| 谁写入 | 平台或业务方在创建/更新 PodGroup 时设置；调度器 **不会** 自动生成。 |
| 取值含义 | 表示 **故障域 / 容量池**（例如 `llama-70b-prod` = 须在超节点层彼此打散的生产模型多副本）。 |
| 环境隔离 | 不同环境、租户使用不同取值（如 `…-staging` 与 `…-prod`）。 |
| 与 selector 关系 | 仅把用于界定 peer 集合的 label 写入 `podGroupSelector`；仅用于运维筛选的 label（如 `app`）不必写入 selector。 |

### 2.3 同 PodGroup 内 SubJob 规则

`subGroupTopologyAffinity` 写在 **PodGroup spec 顶层**（**不** 挂在各 `subGroupPolicy` 条目上）：

- **`subGroupAffinity`：** `matchSubGroupPolicyNames` 所列 policy 在 `topologyDomain` 指定层上 **共域**（如 prefill 与 decode 共超节点）。
- **`subGroupAntiAffinity`：** 须同时配置 **`subGroupSelector`** 与 **`antiSubGroupSelector`**。policy **内** 分片互斥时两侧写 **相同** policy 名；**跨角色** 分机柜时两侧写 **不同** policy 名（可选）。

`matchSubGroupPolicyNames` 只写 `subGroupPolicy[].name`（如 `prefill`），**不要** 写 `prefill-0` 等分片后缀。

### 2.4 分离层级：名称或整数

每个 term 含 **`topologyDomain`**；其中 `topologyTierName`（对应 `HyperNode.spec.tierName`）与 `topologyTier`（对应 `HyperNode.spec.tier`）**二选一**，不可同时配置，与 `networkTopology` 的 `highestTierName` / `highestTierAllowed` 规则一致。

**注意：** 此处 **不是** Kubernetes `PodTopologySpread` 的 `topologyKey`（Node 标签键），而是 HyperNode **层级名/序号**。详见设计文档 [决策-7](../design/group-topology-affinity.md#ad-7组间层级命名与-kubernetes-topologykey)。

示例映射（以集群 HyperNode CR 为准）：

| tierName | tier 整数（示例） | 典型用途 |
|----------|------------------|----------|
| `cabinet` | `1` | 分片 / SubJob Gang |
| `supernode` | `2` | 整机 instance 或跨 PodGroup 互斥 |

## 3 前置条件

1. 已安装 **Volcano**，调度器支持 HyperNode 分配（与 [网络拓扑感知调度](./how_to_use_network_topology_aware_scheduling.md) 相同基线）。
2. 集群已配置 **HyperNode** 树（[手工创建](./how_to_use_network_topology_aware_scheduling.md#322-build-manually) 或 [自动发现](./how_to_use_hypernode_auto_discovery.md)）。
3. 负载通过 **PodGroup**（或由控制器创建的 PodGroup）提交；多分片场景需配置 `subGroupPolicy` / `matchLabelKeys`。

## 4 使用指南

### 4.1 启用调度插件

编辑调度器 ConfigMap：

```shell
kubectl edit cm -n volcano-system volcano-scheduler-configmap
```

在 **`network-topology-aware`** 同级启用 **`group-topology-affinity`**：

```yaml
data:
  volcano-scheduler.conf: |
    actions: "enqueue, allocate, backfill"
    tiers:
    - plugins:
      - name: priority
      - name: gang
      - name: predicates
    - plugins:
      - name: group-topology-affinity
        arguments:
          weight: 10
      - name: network-topology-aware
        arguments:
          weight: 10
          hypernode.binpack.cpu: 5
          hypernode.binpack.memory: 1
```

`arguments.weight` 用于缩放本插件 **HyperNodeOrderFn** 得分（键名与 `network-topology-aware` 一致），与 PodGroup term 上 **`preferred[].weight`**（单条 soft 规则强度）不是同一含义。

修改 ConfigMap 后重启或 reload 调度器。

### 4.2 PodGroup API 概览

字段位于 **`PodGroup.spec`**（`scheduling.volcano.sh/v1beta1`）：

```yaml
apiVersion: scheduling.volcano.sh/v1beta1
kind: PodGroup
metadata:
  name: my-workload
  labels:
    # 跨 PodGroup peer 匹配用 label（见 2.2 节）
    topology.volcano.sh/spread-group: my-spread-group
spec:
  minMember: 1
  queue: default
  # 可选：整 PodGroup 组内拓扑（network-topology-aware）
  networkTopology:
    mode: hard
    highestTierName: supernode
  # 可选：跨 PodGroup 反亲和
  topologyAffinity:
    podGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - podGroupSelector:
            matchLabels:
              topology.volcano.sh/spread-group: my-spread-group
          topologyTierName: supernode
  # 可选：同 PodGroup 内跨 SubJob 组间规则
  subGroupTopologyAffinity:
    subGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution: []
    subGroupAffinity:
      requiredDuringSchedulingIgnoredDuringExecution: []
  subGroupPolicy: []
```

组间 topology **term** 内 **不要** 写 `mode: hard` / `mode: soft`；hard/soft 仅由 `required` / `preferred` 列表表达。

### 4.3 场景：多 inference instance（跨 PodGroup）

**目标：** 同一模型同时运行 3 个 PodGroup（instance），每个 instance 占 **不同超节点**，单超节点故障只影响一个 instance。

**步骤：**

1. 选定 spread 组 label 的键与值（须互斥的 instance 使用 **相同取值**）。
2. 在每个 PodGroup 的 **`metadata.labels`** 上写入该 label。
3. 在每个 PodGroup 上配置相同的 `podGroupSelector` 与 `topologyTierName`（或 `topologyTier`）。

**示例（单个 instance）：**

```yaml
apiVersion: scheduling.volcano.sh/v1beta1
kind: PodGroup
metadata:
  name: llama-70b-instance-0
  labels:
    # 【用户设置】Volcano 不会自动添加
    topology.volcano.sh/spread-group: llama-70b-prod
spec:
  minMember: 8
  queue: default
  topologyAffinity:
    podGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - podGroupSelector:
            matchLabels:
              topology.volcano.sh/spread-group: llama-70b-prod
          topologyTierName: supernode
          # topologyTier: 2   # tierName 与 tier 整数二选一
```

再创建 `llama-70b-instance-1`、`llama-70b-instance-2` 等，使用 **相同** label 与 **相同** 反亲和 term。

**预期结果：** 各 PodGroup 落在不同 `Domain_supernode`；若仅剩 2 个可用超节点域，第 3 个 PodGroup 将 Pending 直至有域可分配。

**验证：**

```shell
kubectl get podgroup -o wide
kubectl describe podgroup llama-70b-instance-0
```

若长期 Pending，检查超节点域余量及调度器日志。

### 4.4 场景：单 instance Prefill–Decode（同 PodGroup）

**目标：** 1 个推理 instance：4 个 prefill 分片 + 2 个 decode 分片：

- 分片内 Pod 在 **同一机柜** Gang（组内）。
- prefill 各分片 **不同机柜**，decode 各分片 **不同机柜**（默认 **不强制** prefill 与 decode 分机柜）。
- **整机在同一超节点**。

使用 `prefill` / `decode` 两条 `subGroupPolicy`，配合 `matchLabelKeys` 拆 SubJob；**不要** 为每个分片单独建 policy 名（`matchSubGroupPolicyNames` 中禁止写 `prefill-0`）。

**推荐方式一：** 超节点 envelope 写在 **`spec.networkTopology`**，分片分机柜写在 **`subGroupTopologyAffinity`**。

```yaml
apiVersion: scheduling.volcano.sh/v1beta1
kind: PodGroup
metadata:
  name: pd-instance-0
spec:
  minMember: 44
  queue: default
  networkTopology:
    mode: hard
    highestTierName: supernode
  subGroupPolicy:
    - name: prefill
      labelSelector:
        matchLabels:
          volcano.sh/role: prefill
      matchLabelKeys:
        - volcano.sh/shard-id
      subGroupSize: 8
      minSubGroups: 4
      networkTopology:
        mode: hard
        highestTierName: cabinet
    - name: decode
      labelSelector:
        matchLabels:
          volcano.sh/role: decode
      matchLabelKeys:
        - volcano.sh/shard-id
      subGroupSize: 6
      minSubGroups: 2
      networkTopology:
        mode: hard
        highestTierName: cabinet
  subGroupTopologyAffinity:
    subGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - subGroupSelector:
            matchSubGroupPolicyNames: [prefill]
          antiSubGroupSelector:
            matchSubGroupPolicyNames: [prefill]
          topologyTierName: cabinet
        - subGroupSelector:
            matchSubGroupPolicyNames: [decode]
          antiSubGroupSelector:
            matchSubGroupPolicyNames: [decode]
          topologyTierName: cabinet
```

**方式二（可选）：** 不写顶层 `networkTopology`，用一条亲和 term 表达 prefill + decode 共超节点：

```yaml
  subGroupTopologyAffinity:
    subGroupAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - matchSubGroupPolicyNames: [prefill, decode]
          topologyTierName: supernode
    subGroupAntiAffinity:
      # 分机柜 term 同方式一
```

「共超节点」在方式一与方式二中 **二选一**，勿重复配置。

**Pod 模板：** Pod 须带 `subGroupPolicy.labelSelector` 与 `matchLabelKeys` 所需 label（如 `volcano.sh/role`、`volcano.sh/shard-id`）。

### 4.5 场景：生产组合（多 instance + Prefill–Decode）

合并 [4.3 节](#43-场景多-inference-instance跨-podgroup) 与 [4.4 节](#44-场景单-instance-prefilldecode同-podgroup)：

- 各 instance：`topologyAffinity.podGroupAntiAffinity` @ `supernode` + 相同 spread-group label。
- 各 instance 内部：按 4.4 节配置 `subGroupPolicy` 与 `subGroupTopologyAffinity`。

每个 instance 占一超节点；超节点内按配置分机柜部署分片。

### 4.6 可选：软性分片分机柜（机柜紧张）

机柜不足时，可将 `subGroupAntiAffinity` 从 `required` 改为 `preferred`，并为 term 设置 `weight`（越大表示越倾向避开同柜）：

```yaml
  subGroupTopologyAffinity:
    subGroupAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          term:
            subGroupSelector:
              matchSubGroupPolicyNames: [prefill]
            antiSubGroupSelector:
              matchSubGroupPolicyNames: [prefill]
            topologyTierName: cabinet
```

若整机仍须共超节点，保持 `networkTopology` @ `supernode` 为 hard。

**不要** 在 term 上写 `mode: soft`；软性仅通过 `preferred` 列表表达。

### 4.7 可选：prefill 与 decode 强制分机柜

默认不要求 prefill 与 decode 使用不相交机柜。若需 **跨角色** 分机柜，增加 policy 名 **不相交** 的 term：

```yaml
    subGroupAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        # ... 保留 prefill↔prefill、decode↔decode term ...
        - subGroupSelector:
            matchSubGroupPolicyNames: [prefill]
          antiSubGroupSelector:
            matchSubGroupPolicyNames: [decode]
          topologyTierName: cabinet
```

角色隔离更强，但 prefill–decode 跨柜通信可能增加。

## 5 配置参考

### 5.1 `topologyAffinity.podGroupAntiAffinity`

| 字段 | 必填 | 说明 |
|------|------|------|
| `podGroupSelector` | 是 | 匹配 **其它** PodGroup 的 `metadata.labels`。 |
| `namespaceSelector` | 否 | 限制 peer PodGroup 所在命名空间。 |
| `topologyTierName` / `topologyTier` | 二选一 | 在何层级上要求域互异。 |

Phase 1 **仅支持反亲和**（无跨 PodGroup `podGroupAffinity`）。同 PodGroup 内共域请用 `networkTopology` 或 `subGroupAffinity`。

### 5.2 `subGroupTopologyAffinity`

| 子字段 | 用途 |
|--------|------|
| `subGroupAffinity` | `matchSubGroupPolicyNames`：所列 policy 在指定 tier **共域**。 |
| `subGroupAntiAffinity` | `subGroupSelector` + `antiSubGroupSelector`：subject 与 peer SubJob 在指定 tier **异域**。 |

### 5.3 常见误配

| 误配 | 正确做法 |
|------|----------|
| 用 `podGroupAntiAffinity` 表达同 PodGroup 内 prefill vs decode | 使用 `subGroupTopologyAffinity` |
| `matchSubGroupPolicyNames` 写 `prefill-0` | 写 policy 名 `prefill`，分片用 `matchLabelKeys` |
| 在组间 term 上写 `mode: soft` | 使用 `preferredDuringSchedulingIgnoredDuringExecution` |
| 同一 term 同时写 `topologyTierName` 与 `topologyTier` | 二选一 |
| 跨 PodGroup term 未填 `podGroupSelector` | `podGroupSelector` 必填 |
| 仅靠 Pod `podAntiAffinity` 做 PodGroup 级打散 | 配置 PodGroup label + `podGroupAntiAffinity` |

## 6 故障排查

| 现象 | 排查项 |
|------|--------|
| PodGroup 长期 Pending（拓扑相关） | 对应 tier 是否有足够 HyperNode 域；`minMember` / Gang；HyperNode 容量预筛。 |
| 多 instance 仍落同一超节点 | 各 instance label 是否一致；`podGroupSelector` 是否匹配；是否启用 `group-topology-affinity`。 |
| 分片仍在同一机柜 | 是否配置 `subGroupAntiAffinity`；policy 名是否正确；是否误用 soft 替代 hard。 |
| Webhook 拒绝 | `subGroupTopologyAffinity` 存在时 `subGroupPolicy` 是否 ≥ 2 条；tier 是否在 HyperNode 中存在；字段是否写错章节。 |
| 规则似乎未生效 | 组内+组间并存时是否同时启用 `network-topology-aware` 与 `group-topology-affinity`；hard 规则 `required` 列表是否非空。 |

可提高调度器日志级别，搜索 `group-topology-affinity`、`HyperNodeGradient`。

## 7 相关链接

- [组间拓扑亲和设计提案（中文）](../design/group-topology-affinity.md)
- [网络拓扑感知调度用户指南](./how_to_use_network_topology_aware_scheduling.md)（英文）
- [调度器配置](./how_to_configure_scheduler.md)
- 社区 Issue：[volcano-sh/volcano#5347](https://github.com/volcano-sh/volcano/issues/5347)
