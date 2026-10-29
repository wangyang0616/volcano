Pod status 变化采集与更新者关联
============================

依赖：Python 3.8+、kubectl，以及能访问目标集群的 kubeconfig；不需要安装 pip 包。
单独复制 watch_pod_status.py 到有 kubectl 的机器即可运行。
脚本只执行 LIST/WATCH/GET 和可选的日志读取，不修改 Pod、调度器或审计配置。
审计日志完全可选；默认根据连续 Pod 版本的 managedFields 给出组件候选和推测依据。

1. 针对当前 Volcano/Kthena 问题启动采集

   python3 watch_pod_status.py watch \
     -n YOUR_NAMESPACE \
     --pod-group YOUR_PODGROUP \
     --scheduler-namespace volcano-system \
     --scheduler-deployment volcano-scheduler \
     --output ./capture-01 \
     --duration 1800

   替换命名空间、PodGroup 和 scheduler Deployment 名称。
   --pod-group 按 Pod 注解 scheduling.k8s.io/group-name 过滤，并同步记录该 PodGroup。
   不确定 PodGroup 名称时，可改用 --pod POD_NAME，或 -l 'app=YOUR_APP'。
   --pod 可重复指定；不加任何过滤条件则记录整个命名空间的 Pod。
   同时使用 --pod、-l 和 --pod-group 时取交集。
   可用 --kubeconfig PATH / --context NAME 指定集群；默认使用 kubectl 当前配置。
   --duration 单位为秒，省略则一直运行，Ctrl-C 停止并生成最终关联文件。
   输出目录必须为空。每轮实验使用新的目录，不要混合不同集群的采集结果。

   只记录单个 Pod（最少权限、最少输出）：

   python3 watch_pod_status.py watch -n YOUR_NAMESPACE \
     --pod YOUR_POD --no-events --output ./capture-02

   stdout 实时输出 JSON；每个 POD 记录都包含完整 status。
   可加 --pretty 在终端展开显示，文件始终为一行一条记录的 JSONL。
   可将 stdout 重定向到文件减少终端输出；核心记录始终另存于输出目录。

2. 每条记录包含什么

   eventType / observedAt                 watch 类型与本机接收时间
   namespace / pod / uid                 对象身份；同名 Pod 重建不会混为同一个对象
   resourceVersion / previousResourceVersion
                                         本次与上次记录的版本
   status / previousStatus               完整新旧 status
   statusDiff                            精确变化路径及新旧值，支持新增、移除、null
   statusChanged                         是否真的改变 status
   podScheduled / previousPodScheduled   PodScheduled condition 的完整新旧内容
   messageChanged                        PodScheduled.message 是否变化
   nodeName / nominatedNodeName           实际绑定节点、提名节点
   schedulerName / ownerReferences        调度配置和所属控制器，仅为上下文
   managedFieldsHints                    新旧 status 字段管理条目，不作为本次更新者证明
   componentHints                        无审计的字段管理者/组件推测及依据
   writer                                审计关联结果，与 componentHints 分开保存

   例如：同一 Pod 的 message 从含 HyperNode 变成不含 HyperNode，或者其中的
   Pipeline 节点 IP 改变，将记录 messageChanged=true，以及 message 的完整新旧值。
   只有 annotation/spec 变化时仍然保留该 Pod 版本，statusChanged=false。
   INITIAL 是启动时快照，不代表一次刚发生的更新；新 Pod 的 ADDED 没有前一份 status，
   所以也不会计算 statusDiff。删除通过 DELETED 单独标识。
   observedAt 是接收时间；审计 stageTimestamp 是 API 请求完成阶段的时间，两者不同。

3. 如何确认“谁触发了这次更新”

   没有审计日志时，不传 --audit-log 即可，脚本不会要求或访问审计日志。
   默认从 API 的原始 Pod JSON 读取 managedFields（内容与 YAML 对象相同），比较连续
   版本中的 status 值和字段管理记录，输出 componentHints。它不额外 GET 最新 Pod，
   避免把后续版本的 managedFields 错配到当前 watch 事件。

   如需手动查看相同信息，kubectl 默认隐藏 managedFields，需显式显示：

   kubectl get pod YOUR_POD -n YOUR_NAMESPACE -o yaml --show-managed-fields

   重点查看 metadata.managedFields 中的 manager、operation、subresource、time、
   fieldsV1。例如 fieldsV1.f:status.f:conditions.k:{"type":"PodScheduled"}.f:message
   表示该 manager 管理 PodScheduled.message。operation=Update 也可能对应普通 PATCH，
   不能用它区别 PUT/PATCH；manager 可由客户端 fieldManager 指定，否则可由 User-Agent
   推导，所以它不是经过认证的身份，也不能区分同名 manager 的不同副本。

   componentHints 的含义：
   - inferredManagers：本次 status 变化的候选字段管理者，如 volcano-scheduler。
   - likely：唯一候选具有本轮 managedFields 时间变化或新取得变更字段归属等线索。
   - possible：唯一候选管理发生变化的字段，但条目没有可观察的同步变化；线索更弱。
   - ambiguous：存在多个候选，保留全部候选及证据。
   - unknown：没有可用的匹配线索；不会仅凭 schedulerName 或 ownerReferences 猜测。
   - no_status_change：仅 metadata/spec 等变化，本次不推测 status 更新组件。
   - not_a_comparable_update：初始快照、新建/删除或重新 LIST，没有可比较的单次 MODIFIED。
   - candidates[].changedOwnedPaths：本轮真正发生值变化、且该 manager 当前管理的字段。
   - candidates[].time / previousTime / timeChanged：此条目的新旧时间及是否变化。
   - candidates[].newlyOwnedChangedPaths：本轮新取得归属、同时值发生变化的字段。
   - previousOwners：失去变更字段归属的原 manager，仅保留历史线索，不据此指认更新者。

   例如：message 内容变了，当前归属 volcano-scheduler，并且该条目 time 也变化，
   会输出 state=likely、inferredManagers=["volcano-scheduler"]。如果同一秒连续更新
   message，time 可能不变，此时仍保留字段归属线索，输出 possible。
   条目因别的 manager 接管字段而丢失归属时，时间不一定更新，所以不能仅比较谁的
   time 最大。条件列表按 type（例如 PodScheduled/Ready）匹配，避免列表排序、增加
   Ready 条件、或某个 manager 只管理列表成员资格时误认 message 的更新者。
   changedOwnedPaths 使用字段选择器表示列表项，属于可读路径而不是 JSON Pointer。

   对于本次排查：inferredManagers 一直相同而 message 在交替，支持“同一字段管理者
   反复改写”的判断；若 manager/字段归属也交替，则支持“多个字段管理者轮流写”的
   判断。二者都是证据线索，无法直接区分 scheduler 的 allocate/preempt 等内部路径。
   managedFields 缺失、被重置、共用 manager 名称或观察到版本缺口时，判断能力会下降。

   Kubernetes Pod watch 不包含经过认证的请求者身份。
   managedFields.manager 表示字段管理关系，可以由客户端指定，不能证明某一次更新
   的身份；Event 的 reportingController/source 也不能证明谁写了 Pod status。
   因此，没有可匹配审计证据时 writer.state=unknown；组件推测请看 componentHints，
   其 notProofOfWriter 始终为 true，即使 state=likely 也不表示审计确认。

   精确关联需要 API Server 的审计事件，最好使用 RequestResponse 级别，同时保留
   requestObject、responseObject、user、userAgent、objectRef 和 ResponseComplete 阶段。
   Metadata 级别能看到 API 调用者，但没有响应对象版本，不能直接把某个请求唯一对应
   到某一次 watch 更新。

   已有本机审计日志时实时关联：

   python3 watch_pod_status.py watch -n YOUR_NAMESPACE --pod YOUR_POD \
     --audit-log /path/to/apiserver-audit.jsonl --output ./capture-03

   --audit-log 指脚本所在机器能够读取的文件，不是远程控制面上的路径。
   可以重复传入多份文件，用于多个 API Server 的日志。
   普通 JSONL 从文件开头读取并持续追踪；识别文件替换/截断；.gz 归档仅读取一次。
   不自动从控制面下载日志。托管集群可先导出原始 Kubernetes audit JSONL 再关联。
   云厂商额外包装的 JSON 需要先解出原始 audit.k8s.io Event；不支持普通文本日志。

   复现结束后拿到审计日志，也能补充关联：

   python3 watch_pod_status.py correlate --capture ./capture-01 \
     --audit-log /path/to/apiserver-1-audit.jsonl \
     --audit-log /path/to/apiserver-2-audit.jsonl.gz

   请在 watch 结束后运行 correlate，并只导入同一集群的日志。
   correlate 保留原始 pod-updates.jsonl，生成/更新 pod-updates.enriched.jsonl。
   审计延迟到达时，实时输出 ATTRIBUTION 记录，以 eventID / UID / resourceVersion
   补充已记录的 POD 事件。最终统一查看 enriched 文件即可。

   关联规则：
   - 只接受成功的 Pod 主资源/status create、update、patch、delete 请求，排除 dry-run。
   - 使用 responseObject.metadata.uid + resourceVersion 对应 watch 的对象版本。
     不使用 requestObject 的旧版本冒充本次写入版本，也不靠 Pod 名字/近似时间硬匹配。
   - confirmed_version_advanced：唯一一条匹配请求具有明确的请求版本，且响应版本
     与请求版本不同；confirmedAuditID 指明确认的请求，candidates 中包含该请求的
     user.username、userAgent、sourceIPs、verb、subresource、时间和 auditID。
   - response_version_match_unproven：有匹配的响应版本，但没有请求版本证明版本推进。
     例如不携带 resourceVersion 的 PATCH 可能是 no-op，不能唯一确认其导致了变更。
   - ambiguous：存在多个无法唯一判定的候选请求，保留全部证据。
   - unknown：没有足够的匹配证据。请求版本等于响应版本的 no-op 不作为更新者候选。
   - not_an_observed_write：INITIAL/RESYNC 快照，不把历史请求当作刚观察到的写入。
   - 使用 impersonation 时同时记录 user 与 impersonatedUser，便于区分原身份和代理身份。

   审计确认的是发起 API 请求的身份，不能直接定位 Volcano 内部是 allocate/preempt/
   backfill 哪个分支触发，也不直接证明 admission webhook 对返回结果做了哪些修改。
   这些需要根据 UID、时间、message 对照 scheduler 日志或增加针对性调度日志。

   如需给集群管理员提供审计配置示例：

   python3 watch_pod_status.py audit-policy -n YOUR_NAMESPACE > audit-policy-example.yaml

   此命令只生成示例，不启用或修改 API Server 配置。将示例中的 rule 合并到现有
   policy 中先于宽泛匹配规则的位置；不要直接覆盖已有审计 policy。
   policy 必须匹配 pods/status；只写 pods 不会覆盖 status 子资源。
   若全局 omitStages 已排除了 ResponseComplete，也需要保留这个阶段。
   审计后端必须保留请求/响应内容；日志轮转、截断、缺失均可能导致无法关联。
   RequestResponse 会增加日志量并可能包含 Pod spec/env；只针对复现范围采集并妥善保存。

4. 输出文件

   pod-updates.jsonl           每次观察到的 Pod 版本，完整新旧 status、差异、即时归因
   pod-updates.enriched.jsonl  停止时或 correlate 后，合并当前全部审计证据的完整记录
   attributions.jsonl         运行过程中晚到的审计归因补充
   audit-writes.jsonl          实时读取到的成功审计写请求摘要
   audit-unmatched.jsonl       实时读取到的缺少响应 Pod UID/版本的审计请求
   events.jsonl               同命名空间的 Pod Event，保留完整对象；不是更新者证明
   podgroups.jsonl            --pod-group 指定的 PodGroup 状态及事件
   scheduler-<ns>-<pod>.log    可选 scheduler 日志，各副本分别保存
   scheduler-pods.jsonl       从 Deployment 发现的 scheduler Pod 信息
   raw-pods.jsonl             仅 --raw-pods 时保存完整 Pod，包括 spec/env 等
   diagnostics.jsonl          重连、权限问题、WATCH_GAP、审计文件轮转/解析异常
   run.jsonl / summary.jsonl  采集参数、开始结束时间、Pod 记录数/status 变化数/缺口数
   audit-index.sqlite         磁盘关联索引；保留整个目录便于事后 correlate

   文件按需创建，没有相关记录时可能不存在。correlate 主要更新索引和 enriched 文件，
   不回写原始 POD 记录，也不补造实时 attributions/audit-writes 文件。
   仅按 label/PodGroup 过滤时，events.jsonl 保留命名空间内 Pod Event 以免丢失先于
   Pod 到达的事件，分析时按 involvedObject.uid 关联；指定 --pod 时额外按名字过滤。
   --scheduler-deployment 记录启动时发现的全部当前副本，可用 --scheduler-pod 重复
   指定 Pod，--scheduler-container 指定容器。日志流因重建/重启结束会提示，需要另行
   补取替换 Pod 或 previous 日志。日志详细程度取决于 scheduler 现有配置。

5. 权限、重连和采集边界

   Pod 采集需要目标命名空间 pods 的 list/watch 权限。
   默认 Event 采集还需要 events 的 list/watch，可用 --no-events 关闭。
   --pod-group 需要 scheduling.volcano.sh/podgroups 的 list/watch。
   Deployment 日志发现需要 scheduler 命名空间 deployments 的 get、pods 的 list；
   日志读取需要 pods/log 的 get，部分 kubectl 版本还会读取 Pod 信息。
   某条辅助流因权限不足停止会写 diagnostics；Pod 主流停止会报错退出。

   使用 LIST 的 resourceVersion 启动 WATCH，逐条消费事件，断线从最后已消费版本重连。
   服务器正常结束一次 watch 不会被当作漏采，脚本继续从相同版本订阅。
   如果出现 410/Expired，写 WATCH_GAP 并重新 LIST：RESYNC 是最新快照，无法恢复
   历史窗口外的中间状态；summary.gaps 统计 Pod watch 的这类缺口。
   重建快照中缺失的旧 Pod 记录为 MISSING_AFTER_RESYNC，不伪造删除事件和删除时间。
   需要检查 diagnostics，不能因为脚本自动重连就断言全程绝无遗漏。

   本脚本记录的是 API Server 对象版本变化；没有实际改变对象的 no-op 请求不一定
   产生 watch 事件，需要直接查 audit-writes/原始审计日志。
   记录每次完整 status 的输出量可能较大，可通过 Pod/PodGroup 过滤和 --duration
   控制采集范围。原始日志不自动轮转，注意输出目录剩余空间。

6. 开发验证

   python3 -m unittest discover -s hack/diagnostics -v

   覆盖差异计算、晚到审计关联、同名 Pod 重建、no-op/失败/代理线索处理、版本匹配
   歧义、watch 批量事件/断线续接/410 重建、审计半行读取和文件轮转。
   同时覆盖无审计字段归属推测、同秒更新、manager 切换、条件列表增删/排序、成员
   归属与值归属的区别，以及 metadata 更新/快照不误报 status 更新者。

参考：
https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/
https://kubernetes.io/docs/reference/using-api/api-concepts/#efficient-detection-of-changes
https://kubernetes.io/docs/reference/using-api/server-side-apply/
https://kubernetes.io/docs/reference/kubernetes-api/definitions/managed-fields-entry-v1-meta/
