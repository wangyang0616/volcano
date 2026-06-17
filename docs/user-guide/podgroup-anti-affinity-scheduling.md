# PodGroup 反亲和调度

## 背景

随着大模型参数规模指数级增长，单个 Pod 已无法承载一个完整的推理实例。业界普遍采用张量并行（TP）、流水线并行（PP）、PD 分离（Prefill/Decode 拆分）等方案，使得**一个推理实例由多个 Pod 协同完成一次推理**。在生产环境中，同一个模型服务往往会部署**多个推理实例**以承载流量，这就带来了两个相互配合的调度诉求：

- **实例内（聚拢）**：同一个推理实例内部的多个 Pod（如 TP/PP 分片、Prefill 与 Decode 角色）通信密集，应尽量落在同一网络拓扑域内，降低通信时延。这一诉求由 Volcano 的**网络拓扑感知调度**（`networkTopology` 软/硬约束）解决。
- **实例间（打散）**：同一个模型服务的多个推理实例之间，应尽量分布在**不同的网络拓扑域**。原因有二：一是避免多个高带宽实例挤在同一组上联链路上互相争抢带宽、引发拥塞；二是避免多个实例集中在同一故障域，一旦该超节点故障会同时损失多个实例、击穿服务可用性。

**实例间打散**正是本文介绍的 **PodGroup 反亲和调度** 所要解决的问题。在 Volcano 中，一个推理实例对应一个 **PodGroup**，PodGroup 反亲和允许声明"本实例应尽量/必须与匹配到的其它实例分布在不同的网络拓扑层级"，从而在实例内聚拢的同时实现实例间打散。

除分布式推理外，该能力同样适用于 AI 训练、大数据、HPC 等需要在作业间做故障域隔离或带宽打散的批量计算场景。

> **说明**
>
> PodGroup 反亲和是**跨 PodGroup（作业/实例间）** 的反亲和约束，作用对象是整个作业组在网络拓扑上的落点；它与 Kubernetes 原生 Pod 间反亲和（`podAntiAffinity`，作用对象是单个 Pod 与节点）解决的是不同层面的问题，二者可同时使用、互不冲突。

## 功能

PodGroup 反亲和调度提供以下能力：

- **基于网络拓扑层级的实例打散**：以 HyperNode 的层级（Tier）为粒度，控制当前实例与匹配到的其它实例分布在不同的拓扑域。
- **硬反亲和（Required）**：强制约束。若在指定拓扑层级上无法与冲突实例错开，则当前实例不予调度（保持 Pending），直到满足条件。适用于故障域强隔离。
- **软反亲和（Preferred）**：优选约束。调度器对候选 HyperNode 打分，优先选择不与冲突实例重叠的拓扑域；无法完全错开时仍允许调度，不会阻塞实例。适用于带宽打散、性能优化等"尽力而为"场景。
- **灵活的匹配范围**：通过标签选择器 `podGroupSelector` 选择需要规避的目标实例（例如"同一个模型服务的其它实例"），并可通过 `namespaceSelector` 限定命名空间范围。
- **多约束组合**：支持在同一实例上同时声明多条 Required 和 Preferred 规则，分别作用于不同的拓扑层级，并与实例内的网络拓扑聚拢约束协同工作。

## 前提条件

- 已部署 Volcano，且版本支持 `group-topology-affinity` 插件与 PodGroup `topologyAffinity` 字段。
- 集群已开启 **网络拓扑感知调度（Network Topology Aware Scheduling）**，并已创建描述集群网络拓扑的 **HyperNode** 资源（即 HyperNode 树已就绪）。
- 业务以 **PodGroup** 作为调度单元提交。常见的上层负载形态：
  - **Kthena `ModelServing`**：云原生大模型推理负载，由 Kthena 控制器为每个推理实例（ServingGroup）自动创建一个 PodGroup（需额外部署 [Kthena](https://kthena.volcano.sh/)）。
  - **Deployment 等原生负载**：通过将 Pod 绑定到预先创建的 PodGroup 来纳管。
  - **Volcano Job**：直接生成 PodGroup。
- 需要相互反亲和的实例，其 PodGroup 上已设置可用于选择的 `metadata.labels`。

> **说明**
>
> HyperNode 用于描述集群网络拓扑结构，通过 `spec.tier`（层级编号，数值越大层级越高、范围越广）和 `spec.tierName`（层级名称）组织成树状结构。例如在华为 A3 超节点场景下，`tierName: hypernode` 代表**超节点**、`tierName: hypercluster` 代表**参数面**。PodGroup 反亲和的"拓扑层级"即引用 HyperNode 的 Tier。

## 约束与限制

| 约束项 | 说明 |
| --- | --- |
| 依赖网络拓扑 | 仅在集群 HyperNode 拓扑就绪时生效。拓扑未就绪时，带反亲和的实例会跳过本轮调度并记录相应日志。 |
| 作用粒度 | 反亲和以 **HyperNode Tier** 为最小比较粒度，不能精确到单个节点。如需节点级反亲和，请使用 Pod 原生 `podAntiAffinity`。 |
| `podGroupSelector` 必填 | 每条反亲和规则必须指定 `podGroupSelector`，否则该规则不会匹配任何实例（视为无效）。 |
| 拓扑层级字段互斥 | `topologyTier` 与 `topologyTierName` 互斥，二者只能填其一；`topologyTierName` 必须是集群中已存在的层级名称。 |
| Weight 取值范围 | `weight` 仅对 Preferred 规则生效，取值范围为 **1~100**；超出范围的 Preferred 规则在打分阶段被忽略。 |
| 自我排除 | 反亲和匹配时会自动排除实例自身，实例不会与自己产生反亲和冲突。 |
| 仅匹配已落位实例 | 冲突判断基于"目标实例当前已分配（运行/已绑定）的 HyperNode"。尚未调度落位的目标实例不参与冲突计算。 |

## 功能原理

### 整体流程

PodGroup 反亲和能力由调度插件 **`group-topology-affinity`** 实现，工作在 Volcano 的 HyperNode（网络拓扑）调度链路上。当一个 PodGroup 声明了 `topologyAffinity.podGroupAntiAffinity` 后，调度器会将其纳入 HyperNode 级别的分配流程，并在如下两个阶段施加反亲和约束：

1. **候选 HyperNode 生成（梯度过滤，作用于 Required）**
   调度器从拓扑树根开始遍历，为实例生成候选 HyperNode 列表。对每一条 **Required** 规则，在其指定的拓扑层级上计算"已被匹配实例占用的 HyperNode 集合"；若某个候选 HyperNode 在该层级的祖先恰好落在占用集合内，则将其从候选中**剔除**（reject）。这样保证最终落点在硬约束层级上与冲突实例不重叠。

2. **候选 HyperNode 打分（优选，作用于 Preferred）**
   对通过过滤的候选 HyperNode，调度器调用打分函数。对每一条 **Preferred** 规则，在其指定层级上，如果候选 HyperNode 的祖先与匹配实例占用的 HyperNode 重叠，则对该候选**扣分**，扣分幅度为 `weight / 100`。最终选择得分最高（冲突最少）的 HyperNode 落位。

### 与网络拓扑亲和、层级装箱的配套关系

PodGroup 反亲和并非独立工作，而是与 Volcano 网络拓扑调度的另外两项能力配套，在同一 HyperNode 调度链路上协同决策，分别解决"打散""聚拢""紧凑"三个维度：

- **网络拓扑亲和（实例内聚拢）**：由 PodGroup 的 `networkTopology`（`mode: hard/soft`）提供，约束同一实例的 Pod 尽量/必须落在同一 HyperNode，保证实例内通信效率。
- **PodGroup 反亲和（实例间打散）**：由本文的 `topologyAffinity.podGroupAntiAffinity` 提供，约束不同实例落在不同 HyperNode，保证可用性与带宽隔离。
- **层级装箱（紧凑放置）**：由 `network-topology-aware` 插件提供，在满足上述约束的候选 HyperNode 中，按资源装箱（bin-packing）打分，优先选择能在尽量低（紧凑）的拓扑层级内放下整个实例的 HyperNode，减少跨层级碎片、提升资源利用率。

三者的打分会在候选 HyperNode 上叠加：反亲和负责"先去哪个域错开冲突"，层级装箱负责"在可选域中挑最紧凑的那个"，网络拓扑亲和负责"在选定域内部把实例的 Pod 收拢"。实际使用时通常同时开启 `group-topology-affinity` 与 `network-topology-aware` 两个插件，即可获得"实例间打散 + 实例内聚拢 + 层级紧凑装箱"的整体效果。

### 匹配规则

一个"目标实例"是否被某条规则匹配，需同时满足：

- 目标实例不是当前实例自身；
- 目标实例的命名空间满足 `namespaceSelector`（未设置时默认同命名空间）；
- 目标实例 PodGroup 的 `metadata.labels` 满足 `podGroupSelector`。

匹配到的目标实例，会按其**当前已分配的 HyperNode** 在指定 Tier 上的祖先 HyperNode 计算"占用域"，作为当前实例需要规避的对象。

### 实例落点的记录与维护

实例被分配后，其落点（所有已分配 Pod 的最小公共祖先 HyperNode）会记录在 PodGroup 注解 `volcano.sh/job-allocated-hypernode` 上，供其它实例进行反亲和比较。该落点随实例扩缩容动态更新：扩容时向上扩大为新的公共祖先，缩容时收敛到剩余 Pod 的公共祖先。

## 开启方式

PodGroup 反亲和能力由 `group-topology-affinity` 插件提供。在 **volcano-scheduler** 的配置（ConfigMap `volcano-scheduler-configmap`）中加入该插件，并确保已启用网络拓扑感知插件 `network-topology-aware`。

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: volcano-scheduler-configmap
  namespace: volcano-system
data:
  volcano-scheduler.conf: |
    actions: "enqueue, allocate, backfill"
    tiers:
    - plugins:
      - name: priority
      - name: gang
      - name: conformance
    - plugins:
      - name: overcommit
      - name: drf
      - name: predicates
      - name: proportion
      - name: nodeorder
      - name: binpack
      - name: network-topology-aware
      - name: group-topology-affinity
        arguments:
          weight: 5          # 可选，反亲和优选打分权重，默认 1
```

插件参数说明：

| 参数 | 说明 | 默认值 |
| --- | --- | --- |
| `weight` | `group-topology-affinity` 在 HyperNode 优选打分中的权重，数值越大，反亲和优选对最终落点的影响越大。 | `1` |

> **说明**
>
> - 修改 ConfigMap 后，需等待 volcano-scheduler 重新加载配置（或重启对应 Pod）后生效。
> - `enabledHyperNodeOrder`、`enabledHyperNodeGradient` 等开关默认开启，无需额外配置。

## 使用示例与效果

下文以分布式推理为主线，分别给出 **Kthena ModelServing** 与 **Deployment** 两种上层负载的配置方式。

### 示例拓扑

假设集群网络拓扑为两层：

- `tier 2`（`tierName: hypercluster`）：`root`，代表 A3 场景下的**参数面**，覆盖全部节点；
- `tier 1`（`tierName: hypernode`）：`hypernode-a`、`hypernode-b`、`hypernode-c`，分别代表 A3 场景下的不同**超节点**。

```yaml
apiVersion: topology.volcano.sh/v1alpha1
kind: HyperNode
metadata:
  name: hypernode-a
spec:
  tier: 1
  tierName: hypernode          # A3 场景：超节点
  members:
  - type: Node
    selector:
      labelMatch:
        matchLabels:
          topology-hypernode: hypernode-a
---
apiVersion: topology.volcano.sh/v1alpha1
kind: HyperNode
metadata:
  name: root
spec:
  tier: 2
  tierName: hypercluster       # A3 场景：参数面
  members:
  - type: HyperNode
    selector:
      exactMatch:
        name: hypernode-a
  - type: HyperNode
    selector:
      exactMatch:
        name: hypernode-b
  - type: HyperNode
    selector:
      exactMatch:
        name: hypernode-c
```

> 说明：`hypernode-b`、`hypernode-c` 的定义与 `hypernode-a` 类似，按 `topology-hypernode` 标签区分，此处省略。

### 示例一：Kthena ModelServing —— 多推理实例跨超节点打散

Kthena 的 `ModelServing` 通过 `spec.replicas` 定义 ServingGroup（推理实例）数量，**Kthena 控制器会为每个 ServingGroup 创建一个独立的 PodGroup**，并为其打上 `modelserving.volcano.sh/name: <模型服务名>` 标签。借助该标签，可让同一模型服务的多个实例相互反亲和、分散到不同超节点。

> **须知**
>
> Kthena 当前为 ServingGroup **自动创建**的 PodGroup 暂不支持注入 `topologyAffinity` 字段，因此本节描述的多实例反亲和打散为 **规划中能力，将在后续 Kthena 版本中提供**。在该能力可用之前，如需对推理负载使用 PodGroup 反亲和，请参考[示例二](#示例二deployment--推理服务间跨超节点打散)，采用"手动创建带 `topologyAffinity` 的 PodGroup + 负载绑定"的方式。

以下示例展示该能力可用后的目标用法。`ModelServing` 部署 3 个推理实例，每个实例内部用 `networkTopology.groupPolicy` 聚拢：

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: llama-infer
  namespace: default
spec:
  schedulerName: volcano
  replicas: 3                 # 3 个推理实例（ServingGroup）
  template:
    networkTopology:
      groupPolicy:            # 实例内聚拢：同一实例的 Pod 尽量同一超节点
        mode: soft
    gangPolicy:
      minRoleReplicas:
        worker: 2
    roles:
    - name: worker
      replicas: 2
      # ... 角色容器与资源定义省略 ...
```

Kthena 会为每个实例生成一个 PodGroup（如 `llama-infer-0`、`llama-infer-1`、`llama-infer-2`）。能力可用后，这些 PodGroup 将自动携带如下 `topologyAffinity`，让 3 个实例在 `hypernode`（超节点）层级上相互软反亲和、尽量分散到不同超节点（以 `llama-infer-0` 为例）：

```yaml
apiVersion: scheduling.volcano.sh/v1beta1
kind: PodGroup
metadata:
  name: llama-infer-0
  namespace: default
  labels:
    modelserving.volcano.sh/name: llama-infer       # Kthena 自动注入
spec:
  minMember: 2
  networkTopology:                                  # 实例内聚拢
    mode: soft
  topologyAffinity:                                 # 实例间打散
    podGroupAntiAffinity:
      preferred:
      - weight: 100
        topologyTierName: hypernode                     # 在超节点层级打散
        podGroupSelector:
          matchLabels:
            modelserving.volcano.sh/name: llama-infer   # 规避同一模型服务的其它实例
```

**预期效果**（能力可用后）：3 个推理实例在调度时相互避让，分别落到 `hypernode-a`、`hypernode-b`、`hypernode-c`；每个实例内部的 2 个 Pod 仍聚拢在各自超节点内。任一超节点故障最多只影响 1 个实例，服务整体仍可用。若超节点数不足 3，软反亲和会退化为"尽量分散"，多余实例与已有实例共处一个超节点，但不会阻塞调度。

> **说明**
>
> `topologyAffinity` 作用在每个 ServingGroup 对应的 PodGroup 上，`podGroupSelector` 借助 Kthena 自动注入的 `modelserving.volcano.sh/name` 标签匹配同一模型服务的其它实例，匹配时自动排除实例自身。该字段需由 Kthena 在创建/协调 PodGroup 时写入，不建议用户手动 `patch`（会被 Kthena 控制器协调覆盖），请等待后续 Kthena 版本支持。

### 示例二：Deployment —— 推理服务间跨超节点打散

对于以原生 Deployment 部署的推理服务（每副本单 Pod），可先创建一个带 `topologyAffinity` 的 PodGroup，再通过 Pod 注解 `scheduling.k8s.io/group-name` 将 Deployment 的 Pod 绑定到该 PodGroup。

下例中，推理服务 `infer-svc-a` 要求与另一个高带宽推理服务 `infer-svc-b` 在 `hypernode`（超节点）层级上**强制**错开（Required），以避免两者争抢同一超节点的参数面带宽：

```yaml
# 1) 为 infer-svc-a 创建 PodGroup，声明反亲和
apiVersion: scheduling.volcano.sh/v1beta1
kind: PodGroup
metadata:
  name: infer-svc-a
  namespace: default
  labels:
    app: infer-svc-a
spec:
  minMember: 4
  topologyAffinity:
    podGroupAntiAffinity:
      required:
      - topologyTierName: hypernode    # 在超节点层级强制错开
        podGroupSelector:
          matchLabels:
            app: infer-svc-b           # 必须与 infer-svc-b 错开超节点
---
# 2) Deployment 的 Pod 绑定到该 PodGroup
apiVersion: apps/v1
kind: Deployment
metadata:
  name: infer-svc-a
  namespace: default
spec:
  replicas: 4
  selector:
    matchLabels:
      app: infer-svc-a
  template:
    metadata:
      labels:
        app: infer-svc-a
      annotations:
        scheduling.k8s.io/group-name: infer-svc-a    # 绑定到上面的 PodGroup
    spec:
      schedulerName: volcano                          # 使用 Volcano 调度
      containers:
      - name: server
        image: my-registry/infer-server:latest
        resources:
          limits:
            nvidia.com/gpu: "1"
```

**效果**：`infer-svc-a` 的所有 Pod 作为一个 PodGroup 整体，落点会避开 `infer-svc-b` 当前所在的超节点；由于是 Required，若无满足条件的超节点，`infer-svc-a` 会保持 Pending，直到出现可错开的超节点。若希望"尽量错开但不阻塞"，将 `required` 改为带 `weight` 的 `preferred` 即可。

### 验证与观测

调度完成后，通过 PodGroup 注解查看实例实际落点：

```bash
kubectl get podgroup llama-infer-0 -n default \
  -o jsonpath='{.metadata.annotations.volcano\.sh/job-allocated-hypernode}'
```

将调度器日志级别提升到 `-v=3` 后，可观测到反亲和的关键过程日志，例如：

```
podGroup anti-affinity: matching occupancy, job=default/llama-infer-2, tier=1, occupiedHyperNodes=hypernode-a,hypernode-b, matchingPodGroups=default/llama-infer-0(hyperNode=hypernode-a); default/llama-infer-1(hyperNode=hypernode-b)
podGroup anti-affinity: preferred final scores, job=default/llama-infer-2, pluginWeight=5, scores=hypernode-a:0.00,hypernode-b:0.00,hypernode-c:500.00
```

可见第 3 个实例避开了已被占用的 `hypernode-a`、`hypernode-b`，最终落到 `hypernode-c`。

## FAQ

**Q1：硬反亲和（Required）与软反亲和（Preferred）有什么区别？该如何选择？**

Required 是强制约束，无法满足时实例保持 Pending；适用于故障域强隔离、合规隔离等不可妥协的场景。Preferred 是优选约束，通过打分尽量满足，无法满足时仍允许调度；适用于带宽打散、性能优化等"尽力而为"的场景。推理服务通常希望"尽量打散但不因拓扑不足而拒绝拉起实例"，多数情况下建议使用 Preferred。

**Q2：实例内聚拢和实例间打散会冲突吗？**

不会。二者作用在不同维度：实例内聚拢由 PodGroup 的 `networkTopology` 控制（同一实例的 Pod 落在同一 HyperNode）；实例间打散由 `topologyAffinity.podGroupAntiAffinity` 控制（不同实例落在不同 HyperNode）。调度器先按反亲和筛选/打分确定实例应去往哪个超节点，再在该超节点内部按聚拢约束放置实例的各个 Pod。

**Q3：设置了软反亲和，但实例没有按 HyperNode 打散，落点和不设置时一样？**

请确认 volcano-scheduler 配置中已启用 `group-topology-affinity` 插件，且集群 HyperNode 拓扑已就绪。只有进入 HyperNode 调度链路的实例才会执行反亲和优选打分；同时确认 `podGroupSelector` 能正确匹配到目标实例、且目标实例已实际落位（具有 `volcano.sh/job-allocated-hypernode` 注解）。

**Q4：Kthena 多实例如何相互打散？**

Kthena 为每个 ServingGroup（实例）创建独立 PodGroup，并自动打上 `modelserving.volcano.sh/name: <模型服务名>` 标签。在这些 PodGroup 上配置反亲和、令 `podGroupSelector` 匹配该标签，即可让同一模型服务的多个实例相互避让，N 个实例两两打散到不同拓扑域（匹配时自动排除实例自身）。需要注意：**Kthena 自动创建的 PodGroup 目前尚不支持注入 `topologyAffinity`，该能力将在后续 Kthena 版本中提供**；在此之前，推理负载如需反亲和，可改用 Deployment 等原生负载 + 手动 PodGroup 的方式（参见 Q5 与示例二）。

**Q5：Deployment 没有自动生成带反亲和的 PodGroup，怎么办？**

Volcano 不会从 Deployment 自动注入 `topologyAffinity`。推荐做法是手动创建带 `topologyAffinity` 的 PodGroup，并在 Deployment 的 Pod 模板上通过注解 `scheduling.k8s.io/group-name` 绑定到该 PodGroup，同时设置 `schedulerName: volcano`。

**Q6：`topologyTier` 和 `topologyTierName` 有什么区别？**

二者都用于指定比较的拓扑层级，互斥只能填其一。`topologyTier` 直接填层级编号（对应 HyperNode 的 `spec.tier`）；`topologyTierName` 填层级名称（对应 `spec.tierName`，如 `hypernode`、`hypercluster`），可读性更好，推荐使用。

**Q7：`weight` 不填或填 0 会怎样？**

`weight` 仅对 Preferred 规则有效，有效范围为 1~100。若取值不在该范围（含 0 或不填），该 Preferred 规则在打分阶段会被忽略，相当于该条软反亲和不生效。

**Q8：实例扩容后，新 Pod 落到了其它 HyperNode，反亲和落点会更新吗？**

会。实例的 `volcano.sh/job-allocated-hypernode` 会在 Pod 增减时动态维护：扩容时向上扩大为新的公共祖先 HyperNode，缩容时收敛到剩余 Pod 的公共祖先。其它实例的反亲和比较始终基于该最新落点。

**Q9：没有匹配到任何目标实例时，反亲和会影响调度吗？**

不会。若选择器未匹配到任何已落位的目标实例，则不存在需要规避的占用域，Required 不会剔除任何候选，Preferred 不会产生扣分，实例按常规拓扑感知逻辑正常调度。第一个被调度的实例即属于此情况。
