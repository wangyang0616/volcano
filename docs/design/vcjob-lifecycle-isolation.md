# VCJob 同名重建的生命周期隔离设计

状态：核心实现已落地；本地单元、race、微基准和隔离集群验证结果见 [实施与验证记录](vcjob-lifecycle-validation.md)。生产资源配额下的 5000 个真实 Pod 压测仍是上线前验收项，不以微基准替代。

适用范围：VCJob job controller；覆盖单个 VCJob 包含 5000 个 Pod、频繁删除并创建同名 VCJob 的场景。

关联问题：[volcano-sh/volcano#5973](https://github.com/volcano-sh/volcano/issues/5973)。

## 1. 设计决策

保留现有 jobCache、按 namespace/name 分配 worker 的机制，以及 jobCache 的一把全局互斥锁。在此基础上完善对象身份校验、cache 恢复、事件语义和 API 操作的隔离。

核心规则是：namespace/name 定位一个位置，Job UID 标识一次 VCJob 生命周期，Pod UID 标识一个具体 Pod。旧生命周期可以触发当前 Job 重新检查副本，但不能修改当前生命周期的对象，也不能把旧策略应用到新 Job。

RebuildLifecycle 在同一个全局锁临界区内读取 informer、构造新 Pod 集合并提交。通过 owner UID 索引、恢复请求合并和 Clone 前 UID 检查控制成本。正常 Pod 事件采用单 Pod 更新，不进行全量 Rebuild。

本次不删除 jobCache，不引入 Job 分片锁，不改造 scheduler 的 JobInfo，不重构整个状态机。后续架构调整应按整体可维护性和长期演进价值独立评估。

## 2. 问题与修复目标

### 2.1 触发场景

1. 创建 `namespace/name` 为 K、UID 为 A 的 VCJob，其 Pod 使用可复用的固定名称。
2. 删除 Job A；部分 Pod 仍在 terminating，部分 informer 回调和 worker 请求尚未处理。
3. 创建同名 Job B，UID 为 B。
4. A 的 Pod 事件、cleanup、延迟动作与 B 的 cache 初始化、调谐交错。
5. 如果仅按名字处理，旧对象可能污染新 cache；旧 cleanup 可能删除新条目；cache miss 可能消耗唯一的恢复请求，造成缺失副本长期不再创建。

5000 Pod 会放大事件积压、重复恢复和大 JobInfo 克隆的成本，但不是生命周期混淆的根因。

### 2.2 必须满足的目标

- 旧 Job/Pod 回调、cleanup 和延迟动作不能删除、覆盖或操作新实例。
- informer 仍可推进、apiserver 可用、Pod 名称冲突最终解除时，当前 Job 最终恢复应有副本。
- cache 恢复不吞掉当前 Job 的 PodEvicted、PodFailed、退出码等策略事件。
- 同 UID 的 Pod 修复不回退已经确认写入的 Job.Status。
- 5000 Pod 不导致逐 Pod 全量 Rebuild，也不导致逐个旧请求克隆整个 JobInfo。
- 保持现有 Job/Task/Pod/Partition 策略、JobVersion 和重试规则的适用范围。

这里保证的是身份隔离和最终收敛，不承诺跨 Job informer、Pod informer 和 apiserver 的原子快照，也不新增策略事件 exactly-once 保证。

## 3. 数据来源与不变量

### 3.1 各类数据的职责

| 数据 | 职责 | 限制 |
| --- | --- | --- |
| Job informer | 提供当前已观察到的 Job 生命周期 | 可能落后于 apiserver 的成功写入；不同 informer 没有统一进度 |
| Pod informer | 提供当前已观察到的 Pod 集合 | store 可能领先于 handler；不是事件消费进度 |
| jobCache.Job | 供状态机使用的 Job 对象 | 可以包含 UpdateStatus 成功后、informer 尚未观察到的更新 |
| jobCache.Pods/Partitions | 当前 Job 的 Pod 投影 | 可重建；不能作为策略事件是否已经处理的记录 |
| jobCache.HandledTerminalPods | 已完成处理的终态 Pod 观察，按 Pod UID 记录 | 仅进程内状态；同 UID 恢复保留、跨 Job/Pod UID 不继承，不替代原始策略请求 |
| 原始 Pod 事件/Request | 保留事件种类、退出码、JobVersion 和对象身份 | 处理时可能已经过时；不能直接作为当前状态覆盖 cache |
| apiserver | 执行最终对象操作和解决必要的身份歧义 | 调用必须在 cache 锁外；修改必须携带实例约束 |

因此，不能简单地把整个 JobInfo 定义成“随时无条件被 informer 覆盖的副本”。其中 Pod 集合和已确认写入的 Job 状态需要分别处理。

### 3.2 必须保持的不变量

1. 一个可调谐 JobInfo 只属于一个 Job UID；其 Pod 的 controller owner UID 必须与之相同。
2. 同名 Pod 的 UID 不同时，旧事件不能仅凭名字覆盖或删除新实例。
3. Pod cache 写入只来自受控的 informer 观察路径和 Rebuild；API GET 结果不能直接 Add/Update cache。
4. 同 UID 恢复 Pod 时保留已确认的 Job 对象；跨 UID 切换时创建独立 JobInfo。
5. Rebuild 不代表消费了任何策略事件；cache 写入 no-op 不代表策略事件 no-op。
6. 未完成初始化或需要恢复的条目不能被 worker 当作健康的完整快照使用。
7. cache 的 Add/Update/Rebuild 保存独立 Job 副本，Get/GetForUID/GetStatus 也返回独立的可变 Job/Status 副本。不能把 API 返回对象入 cache 后又直接交给插件修改。Pod 指针仍遵守 informer 只读约束，需要修改时先 DeepCopy。
8. 同一个 UID 可以多次按需修复；只合并同一轮重复工作，不设“生命周期只能恢复一次”的限制。

## 4. 单锁模型与 Pod 索引

### 4.1 全局锁范围

继续使用现有 jobCache Mutex，覆盖 Job/Pod cache 更新、快照克隆、cleanup 检查、初始化状态和 Rebuild 提交。

Rebuild 锁内允许的主要工作是：读取本地 lister/indexer、校验身份、构造 task/partition map、交换 JobInfo。禁止 apiserver 调用、等待 goroutine、执行插件或状态机、操作延迟动作锁，以及回调任何会重新进入 jobCache 的函数。

queue.Add/AddAfter、事件记录和普通日志/耗时指标在解锁后执行。需要与生命周期删除一起完成的指标清理使用明确、受限的内部方法，不能调用回 jobCache；其他代码不得反向持有指标或其他业务锁等待 cache 锁。

“一把全局锁”指 cache 的业务同步模型；lister/indexer 内部仍有锁。锁顺序为 jobCache → informer store 读取。注册的 index function 必须只读取对象字段，不能访问 jobCache，从而不形成反向依赖。

### 4.2 owner UID 索引

在 Pod informer 启动前注册专用索引，建议命名 `volcanoJobOwner`，索引值由 namespace、controller owner name、controller owner UID 组成。

- 只索引 controller owner 为 Volcano Job 的 Pod，校验 owner group/kind。
- 使用 ownerReference 建立索引，不依赖用户可修改的 JobName label。
- namespace/name/UID 共同作为查询参数，读取后再次校验身份及必需的任务元数据。
- 非 VCJob Pod 返回空索引值。
- 索引注册失败时初始化失败并报告原因，避免悄悄回退到锁内全 namespace 扫描。

设 N 为该生命周期的 Pod 数量、T 为 task 数量、P 为 partition 数量，Rebuild 的主要时间与临时映射空间为 O(N + T + P)。索引增加受管理 Pod 的索引内存和事件维护开销，需要纳入基准。5000 Pod 的 Job 自身仍需遍历一次。

## 5. cache 接口契约

以下名称是实现建议，具体 Go 签名可调整，但语义必须保持。

| 接口 | 契约 |
| --- | --- |
| `GetLifecycle(key)` | O(1) 返回 UID、JobVersion、初始化状态等轻量值；不 Clone Pod map |
| `GetForUID(key, uid)` | 同一临界区内先检查 UID/初始化状态，匹配后才 Clone；返回明确的 Missing、Mismatch、NeedsRecovery |
| `ObserveJob(event)` | 处理 Job 观察，验证当前生命周期；同 UID 使用现有更新防回退路径 |
| `ObservePod(event)` | 分离原始事件身份和当前 Pod 投影更新，返回更新结果、生命周期判断和恢复提示 |
| `RebuildLifecycle(key, expectedUID, mode, hints)` | 锁内重新读取 informer，初始化/切换生命周期或刷新同 UID Pod 集合 |
| `DeleteJob(job)` | 精确匹配 UID；返回 Applied、Stale 或 Missing；协调该生命周期的 cleanup 和指标 |

实现中 `ObservePod` 的结果区分 Applied、Unchanged、NeedsRecovery；是否为旧生命周期请求由 worker 的 UID 确认路径单独判断，不把当前投影的更新结果当成原始事件的归属证明。例如，Delete 对 cache 是 Unchanged，仍可能对应合法的 PodEvicted。

读取 informer 使用构造时注入的窄接口，只暴露 Job Get、Pod Get 和按 owner 查 Pod。不要暴露任意业务 callback，避免维护者无意在锁内调用网络或重入 cache。

测试构造对象应使用真实形态的 Job UID、Pod UID。生产 API 对象缺失 UID 应被报告为非法输入，不能为了旧测试普遍放宽跨生命周期校验。

## 6. Job 初始化和 RebuildLifecycle

### 6.1 恢复模式

| 模式 | 触发条件 | 行为 |
| --- | --- | --- |
| EnsureLifecycle | cache 缺失、尚未完成 Pod 初始化，或与当前观察到的 Job UID 不同 | 从当前 Job 和其 Pod 快照建立可调谐条目 |
| RefreshPods | 同 UID 下有具体 Pod 缺失/不一致证据，例如 AlreadyExists | 保留 Job 对象，重建 Pod/Partition 集合 |

Job Add 建立条目时可以先标记 `PodsInitialized=false`，使 Pod handler 能正常增量更新。首次 worker 使用前必须完成 EnsureLifecycle，避免“Job UID 已匹配，但此前错过的 Pod 永远没有补入”。

这个标记只表示当前条目是否完成初始化，不限制后续 RefreshPods。跨 UID 替换重置初始化状态；不在 CRD 中增加字段。

### 6.2 事务步骤

1. 获取 jobCache 全局锁。
2. 在锁内读取当前 Job informer 对象，而不是接收调用者提前读取的 Job/Pod 切片。
3. 检查 expectedUID。请求已经过时则返回 LifecycleChanged，由调用方重新定位当前 Job；不根据陈旧调用者覆盖条目。
4. 检查模式和恢复提示。Ensure 已完成且 UID 相同则立即返回；Refresh 的缺失已经被正常 handler 修复时跳过。
5. 通过 owner 索引读取 Pod 快照，校验 controller owner 身份。
6. 选择 Job 对象：初始化/跨 UID 切换使用当前 informer Job；同 UID Refresh 保留 cache 中已确认的 Job。
7. 构建新的 Pods 和 Partitions，包含当前 UID 的 terminating Pod；不合并旧 cache 中的 Pod。
8. 再次检查 Job informer 的 UID。如果已变化或观察状态不确定，放弃本次提交并请求重试。同 UID 的新 Spec 由正常 Job 更新路径处理。
9. 原子提交条目，标记初始化完成，释放锁。
10. 锁外记录耗时与结果，安排当前生命周期的调谐。

两次 Job Get 不能制造跨 informer 原子事务。它只减少明显的过时提交；在最后一次检查后发生的变化，仍由后续 Job handler 和 worker UID 校验收敛。

Job informer 暂时 NotFound 时不安装空 Job 覆盖现有条目，也不据此宣告某个尚未确认的请求永久失效。删除仍由精确 UID 的删除路径完成；需要消除歧义时在锁外检查 apiserver。

非法任务元数据不能生成半成品可调谐 JobInfo。报告具体对象和原因，保留恢复入口；若它占用了期望 Pod 名称，则按不可管理的名称冲突处理，不能冒充完整恢复或自动删除该对象。

### 6.3 Job 对象不回退

`RefreshPods` 不使用较旧的 Job lister 对象覆盖 cache.Job，也不把不同 ResourceVersion 对象的 Spec、Status 随意拼接。

Job handler 和成功 API 写回继续走受 UID 保护的 Job 更新路径。新增逻辑不通过比较不同 UID 的 ResourceVersion 判断生命周期先后，也不为 Pod 事件引入 ResourceVersion 数值排序规则。

## 7. Pod 观察与策略事件

### 7.1 当前 Pod 投影的更新

每个 Pod handler 保留原始 old/new/tombstone 对象用于事件判断。修改 cache 时，在全局锁内通过 Pod lister Get 读取该名字当前观察到的对象。

| 当前 informer 状态 | cache 投影操作 |
| --- | --- |
| 存在且 owner 属于当前 Job UID | 以该对象更新位置，不用落后的回调对象覆盖它 |
| 存在同名不同 Pod UID | 由当前 informer 身份确认替换；同步移除旧 task/partition 索引 |
| NotFound | 移除该位置上已确认不在 informer 的实例；不复活旧 Add/Update 对象 |
| 属于其他 Job UID | 不加入当前生命周期；必要时请求 Job 级恢复 |
| lister 错误或 Job 生命周期未确认 | 不做破坏性修改；返回恢复提示 |

显式 `DeletePod(expectedPod)` 如果保留，只能条件删除匹配 Job UID 和 Pod UID 的实例。按当前 informer 刷新位置是另一种操作，不能让旧 Delete 事件直接删除一个不同 UID 的当前对象。

Job/Pod informer 未对齐时，不直接使用事件对象创建一个“完整”的 JobInfo。原始请求携带 UID 保留，worker 先恢复 lifecycle 再处理。

### 7.2 策略事件不能由 HasPod 决定

删除当前 Job 的 Pod，即使 Rebuild 已经提前把它从 cache 移除，仍应按原始删除事件评估 PodEvicted。删除事件是否适用，需要检查 Job UID、JobVersion、OutOfSync 标记及既有策略规则，而不是以 cache 是否成功删掉对象为依据。

- `PodFailed`：保留阶段变化及原始 ExitCode。
- `PodEvicted`：保留实际删除/tombstone 事件；移除“HasPod=false 就退化为 OutOfSync”的耦合。
- `PodPending/PodRunning`：保留原始阶段变化及延迟策略处理。
- `TaskCompleted/TaskFailed`：在属于同一 Job UID 的快照上计算条件，不能只因为某次 Pod cache 写入是 no-op 就跳过判断。
- 带 OutOfSync 标记的控制器主动删除，继续遵守现有的策略抑制语义。
- 请求的 JobVersion 过时，继续遵守现有策略降级规则。

同 UID 下，一个 Pod 实例已经消失，不自动意味着它产生的 Job/Task 级失败事件失效。例如失败 Pod 被替换后，其合法失败事件仍可能要求 RestartJob。对单 Pod 的操作则必须另外校验目标 Pod UID。

#### 终态统计不能抢先吞掉策略

保留请求还不够：store 可能已经是 Failed，但 listener 正在处理此前的 Running 元数据回调。若普通 sync 直接按最新 Failed 计数结束 Job，后到的 PodFailed → RestartJob 就会落入 finishedState 而失效。

因此，为当前 JobInfo 增加轻量的 `HandledTerminalPods` 集合，并在 Request 中区分终态转换/首次终态 Add 与后续元数据更新：

- Pod 投影继续读取最新 store，不回退到回调快照。Observe/Rebuild 本身不确认策略已消费。
- worker 处理对应终态请求前，仅确认本次私有快照；状态动作成功后，才在原有 cache 锁下确认对应 Job UID、Pod UID。失败重试和等待 timeout 不放行。
- Running 状态继续同步资源与计数，但自动 Completed/Failed（包括 MinSuccess）要等相关终态观察处理完成。检查在 SyncJob 修改私有 Pod map 之前完成，且在 cache 锁外执行。
- 延迟动作成功合并取消同作用域的其他动作时，一并确认这些被合并的终态观察并安排普通调谐；先释放 delayActionMap 锁，再访问 cache，避免嵌套锁。
- 控制器主动淘汰、terminating、旧 JobVersion 的 Pod 不阻塞当前版本的完成；首次 List 中已有的终态 Pod 以普通同步确认，不伪造新失败事件或 Pending timeout。
- 标记以 Pod UID 为身份，同 UID 元数据更新、同 UID Rebuild 保留；删除、同名 Pod 替换及 Job UID 切换清理。task 注解变化时仍按 Pod UID 确认原始请求，不把任务 bucket 当身份。

该集合按当前投影中的终态 Pod 数量有界，不新增 CRD、独立锁或每 Pod 定时器。正常运行 Pod 不分配标记；读取快照需要复制已有标记。它避免最新投影抢先消费终态，不构造跨 informer 事务，也不提供策略 exactly-once。

### 7.3 判定陈旧事件的边界

必须区分：确认属于已退出生命周期、cache 落后，以及 Job informer 也尚未观察到事件所属 Job。

只比较 Request UID 与 cache UID 不足以认定事件陈旧。优先核对当前 Job informer；若当前 informer 与事件相符，先修复 cache，再保留原始策略事件。

若不同 informer 进度导致归属仍不明确，保留原始请求，在 worker 的恢复路径中延迟处理；必要时在锁外 GET Job，确认是否已经被删除或替换。API 当前对象与请求匹配而 informer 落后时，等待观察推进，不能把请求转换成别的 Job 的策略。

确认旧 Job UID 已退休后，才能丢弃其 Action、ExitCode、Task/Pod 等策略字段，转换为当前 Job 的规范 OutOfSync 请求。不能仅凭 UID 不同假定谁新谁旧。

为避免 5000 个已知旧事件重复验证，可以复用有界的“已确认退休 UID”结果：只记录明确 Job Delete/UID 切换或锁外确认得到的结果，保护在现有 cache 锁下；容量/过期只影响优化命中，未命中回到普通身份确认。它不保存策略事件，也不能成为恢复正确性的前提。缓存条目自身通过 `Deleted` 标记保存不可逆的删除状态，普通 Add/Update/Rebuild 不能重新激活同 UID 的已删除条目；只有新 UID 才建立新条目。该标记不是 API/CRD 字段。普通事件不调用 apiserver。

特别地，Get 得到的 Job 观察可能在随后的退休检查前已经失效。“观察对象 A 已退休”不等于“当前请求 B 已过期”。只有请求自身的 UID 被确认退休，才能丢弃其策略；否则保留原始请求等待观察推进。

Job lister 返回 NotFound 时也优先检查请求 UID 的退休证明：已确认退休直接结束，无需逐 Pod live GET；未知 UID 仍走必要的身份确认。VolcanoJobSupport 关闭时不进入观察等待，因为不存在最终会补齐观察的 Job informer。

同一个 key 的身份歧义检查由对应 worker 合并处理，不能为每个 Pod 启动独立 API 检查。确认结果只证明特定旧 UID 已退出，不能把一次 GET 的“当前 Job”永久缓存为未来请求的依据。持续变化期间不确定的请求保留有上限频率的重试；确认生命周期消失后结束其请求，避免永久保留无法再执行的策略事件。

## 8. worker、AlreadyExists 与恢复重试

### 8.1 worker 入口

worker 使用 GetForUID：在同一把锁内完成 UID 判断和条件 Clone。避免先 GetLifecycle、解锁、再无条件 Get 的检查与使用间隙。

- UID 匹配且初始化完成：按现有状态机执行原始请求。
- cache 缺失/未初始化：EnsureLifecycle 后重新检查请求归属。
- UID 不匹配：确认当前生命周期；只对确认旧请求转换成普通调谐。
- 归属或观察进度不确定：保留原始请求，延迟重试。

不把“UID 未匹配”计为 Job 执行失败，不触发 maxRequeueNum 耗尽后的 TerminateJob。

### 8.2 AlreadyExists 的处理

创建 Pod 遇到 AlreadyExists 后优先读取 informer 中同名 Pod；观察不足时才在锁外 GET。GET 只帮助判断冲突和安排恢复，不直接写 Pod cache，也不把另一生命周期的 Pod 算进当前 Job 状态。

| 冲突对象 | 行为 |
| --- | --- |
| 属于当前 Job，informer 已观察到 | 收集缺失位置，在本轮 sync 末尾合并为一次 RefreshPods |
| 属于当前 Job，informer 尚未观察到 | 等待观察并安排延迟调谐 |
| 属于旧 Job且正在删除 | 不收编、不主动删除；等待名称释放，保留延迟调谐 |
| 属于旧 Job但未删除，或属于其他控制器/无 owner | 报告名称冲突，保留有限频率的检查；不越权接管或强制删除 |
| GET 时已 NotFound | 安排当前 Job 再次调谐 |
| GET 返回权限、网络等错误 | 保留真实错误；不伪装成 stale success |

一次 sync 可以收集多个缺失 Pod 名，但只触发一次全量 RefreshPods。正常 handler 已补齐缺失时跳过重建；informer 本身尚未观察到缺失 Pod 时等待，不反复重建相同的不完整快照。

真实 Pod 状态统计以当前 cache/informer 的同 UID 对象为准。需要恢复时允许暂缓提交这一轮根据不完整快照算出的 Job 状态，避免把缺失 cache 等同于实际副本消失。

### 8.3 规范恢复请求

可合并的普通恢复请求仅包含 namespace、JobName、Job UID、OutOfSync。清空 PodName、PodUID、TaskName、PartitionID、ExitCode、Action 等使每个 Pod 产生不同队列项的字段。原始合法策略事件保持原样。

沿用 workqueue 的 dirty/processing 语义：处理期间再次到达的相同请求可以在 Done 后再次处理。去重减少重复工作，不承诺只调谐一次。

观察落后或旧 Pod 占名采用 AddAfter，使用有上限、带抖动的等待退避；建议初始约 1 秒、最大约 30 秒，具体值由端到端验证确定。该等待状态按当前 Job UID 管理，正常事件可以提前唤醒；观察到进展后重置等待。

此类等待与真实执行错误分开，不累加 Job 失败次数，也不清除已有的执行失败次数。AlreadyExists 导致的快照不完整通过内部等待错误返回，由 worker 延迟处理原始请求，不把本轮当成执行成功；已有策略/命令及其失败历史均保留。新构造的纯恢复请求仍使用规范 OutOfSync 字段。Job 被确认删除、UID 改变或恢复完成时清理等待状态；只有实际执行成功或确认请求已退出当前生命周期时才 Forget 对应执行请求。控制器关闭仍按原有 stop 信号结束，不启动永久独立恢复 goroutine。

### 8.4 syncTask 收敛

保留错误任务恢复入口，但移除“API GET 后直接 UpdatePod”和“GET NotFound 后按名字删除 cache”的行为。改为安排对应 Pod 的 informer 观察修复或当前 Job 调谐。

NotFound、新 Pod UID、旧 Job UID 分别按身份规则处理。重试结束时正确 Forget；需要继续等待观察的任务不能因为耗尽普通错误重试而静默丢失唯一的 Job 级恢复入口。

## 9. API 操作与延迟策略

### 9.1 Pod 修改的最终防线

cache 锁不跨越 apiserver 调用。所有 Pod Delete 使用目标 Pod UID precondition；标记 OutOfSync 的 JSON Patch 第一项 test `/metadata/uid`，随后才写 annotation。

- NotFound：该目标实例已不存在，按幂等完成处理。
- 确认当前名字对应其他 UID：结束针对旧实例的操作，不修改新实例。
- UID 仍匹配但修改失败：保留真实错误并重试。
- 无法确认：不能把所有 Conflict、Invalid 或 patch test 错误都吞掉。必要时锁外 GET 分类；后续重试仍携带原 UID 条件。

Patch 与 Delete 分别带 UID 约束，不能认为 Patch 成功就保证随后按名字 Delete 安全。

Pod 级 action 的目标必须保留触发时的 Pod UID。不能先收到 P1 的 RestartPod 请求，再从当前 JobInfo 按名字取出 P2，并携带 P2 UID 删除；这种情况下 API precondition 也无法保护 P2。

若原 Pod 目标已经消失或被替换，该动作不操作替代实例、不提交一次实际重启的状态；仍排入规范 OutOfSync 请求，避免 PodEvicted → RestartPod 的唯一唤醒被消耗，导致缺少副本无法补建。

### 9.2 延迟动作

保留现有延迟动作执行模型，记录 Job UID、触发 Pod UID、JobVersion 和原始策略上下文。

- 到期执行前通过轻量检查确认 Job 生命周期，不为已知旧 Job 克隆完整 JobInfo。
- Pod 级动作绑定原 Pod UID；Job/Task/Partition 动作按原策略范围和版本约束执行。
- Pending 等等待状态的有效性按该策略本身检查；不能把“Pod 已不在 cache”一概解释成所有失败/驱逐策略失效。
- 新动作替换旧动作后，旧 goroutine 的清理必须校验动作对象身份，不能删掉新动作。
- 延迟动作锁与 cache 锁不嵌套；先取得需要的信息，释放一把锁，再进入另一段操作。
- 不把旧生命周期的延迟动作迁移到新 Job 上执行。

原有 timeout 取消规则需要逐项回归，特别是同 Job 下替换 Pod 后 Running 事件对既有延迟失败策略的处理。不能统一添加 Pod UID 判断而无意改变所有 Job/Task 级策略的取消行为。

## 10. cleanup、指标和关联资源

cleanup item 保留旧 JobInfo 的对象身份和 Job UID。在删除 map 条目前同时检查 key 对应的当前指针、UID，以及该条目 Job 已删除且 Pod 集合为空。任何一个不匹配都只结束旧 cleanup item。

Job 已删除但其旧条目仍在等待 Pod 清空时，Pod Delete 仍可对该条目做精确 UID 清理，不要求 Job lister 中还存在这个 Job。后续旧 Pod Add/Update 不得把已删除条目重新变为可调谐 Job。若同名新 Job 已占据 map 位置，则旧事件不再修改该位置，旧 cleanup item 按指针/UID 不匹配退出。

旧 Job Delete 返回 Stale 时，不清理当前同名 Job 的指标。匹配删除的身份确认和指标清理需要作为同一个受保护提交完成；仅返回 Applied 后在锁外无条件按名字删除指标，仍有新 Job 插入的竞态。状态指标更新也应带 Job UID 验收，避免旧 worker 在切换后回写指标。

同名关联资源也需要明确边界：当前设计重点覆盖 Job/Pod cache 与 Pod 操作，不能据此宣称所有插件资源已经隔离。对本修复调用链中的 Service、ConfigMap、Secret、NetworkPolicy、PVC 和 PodGroup 操作逐项审计：

- 名称含 Job UID 的资源继续保持隔离。
- 名称可复用且由 Job 独占的资源，验证 owner UID；删除还需目标资源自身的 UID precondition，更新需保留 API 并发约束。
- 用户共享资源或显式复用的资源保持原有契约，不自动改成按 Job 删除或重命名。
- 旧 worker 的插件调用不能仅靠一次入口 Job UID 检查就宣称安全。

已审计调用链包括 svc 和 Ray head Service；Ray 同样使用 owner 检查与资源 UID 条件删除。SSH 插件必须以 `%w` 保留 `JobResourceConflictError`，保证共享名称等待不会被外层当作真实执行失败并触发 TerminateJob。

Job 深拷贝后，插件修改的 `ControlledResources` 必须显式持久化。`initiateJob` 比较初始化前后的标记，只在有变化时 UpdateStatus 并更新 cache；即使 PodGroup 仍 Pending 或后续 PVC/插件初始化失败，也保存已经成功的步骤。状态写入失败仍按真实错误返回，不能被名称冲突等待掩盖。

svc 的完成标记只在 ConfigMap、Service、NetworkPolicy 全部就绪后设置，避免重试跳过未完成的子资源；清理根据资源 owner/UID 进行，不以完成标记为前提，覆盖部分初始化或标记写入失败后的资源。全部清理成功后才删除标记。正常 Pending → Terminate/Abort 的资源清理不能依赖 Job 对象删除后的 GC。

若某插件仍存在跨生命周期误删，需在合入前补充定向修复或明确其不受本方案保护的范围。关联资源命名体系的全面调整不属于本次架构变更。

## 11. 关键时序与正确性说明

### 11.1 Delete handler 已完成，再发生 Rebuild

Delete callback 之前 informer store 已反映删除。Rebuild 持 cache 锁后重新读 store，所以不会安装调用者早先保存的含旧 Pod 快照。

### 11.2 store 已删除，Delete handler 尚未执行

Rebuild 得到不含该 Pod 的投影。随后 Delete 对投影可以 no-op，但合法的当前 Job PodEvicted 仍按原始事件处理，不通过 HasPod 抑制。

### 11.3 store 已是新 Pod，旧 Update 尚未执行

Rebuild 安装新 Pod。旧 handler 更新投影时读取当前 lister，得到新 Pod或后续状态，因此不把旧实例写回。原始事件另行按 Job/Pod 目标和版本规则决定是否适用。

### 11.4 Pod store 在 Rebuild 快照后变化

对应事件的 handler 等待 cache 锁，随后以当前观察修正投影。只要 informer 和 controller 持续推进，最终收敛；恢复等待请求还提供后续检查入口。

### 11.5 cache 已有新 Job.Status，Job informer 尚未更新

同 UID Refresh 只替换 Pod/Partition 集合，保留已确认的 Job。正常 Job 更新路径随后接收 informer 事件并保持现有防回退约束。

### 11.6 worker 通过检查后 Job/Pod 被替换

cache 锁不会阻止这种外部变化。请求目标保留原 UID，API Patch/Delete 的前置条件拒绝跨实例修改。旧请求无法通过按名字重新解析成新目标绕过检查。

这些说明依赖 informer 持续运行和最终观察到变化；无法在 apiserver 永久不可用、Pod finalizer 永不释放或其他控制器永久占用名称时保证副本恢复。

## 12. 5000 Pod 性能评估

此前 Apple M3 本地微基准、`GOMAXPROCS=1` 的参考数据如下。测试文件未保留为正式基准，正式实现需提交可复现版本重新测量。

| 测量对象 | 平均操作耗时 |
| --- | --- |
| 当前 ReplaceLifecycle，5000 Pod | 约 2.22～2.24 ms |
| 模拟快照读取与新 JobInfo 构造，namespace 共 5000 Pod | 约 1.61～1.62 ms |
| namespace 共 50000 Pod、目标 5000 Pod，按 namespace 扫描 | 约 9.37～11.48 ms |
| 相同规模，通过 owner UID 索引 | 约 1.32～1.47 ms |
| 当前 cache.Get 克隆 5000 Pod JobInfo | 约 0.27～0.28 ms，约 219 KB/op |

这些是平均操作耗时，不是精确锁持有时长，也不是尾延迟。单执行线程不等价于容器 1 CPU limit；样本没有覆盖完整 Pod Spec、partition、大量并发事件或 CPU throttling。

数据支持优先验证单锁方案，不构成生产延迟承诺。新增的单 Pod lister 查询、UID 判断、索引维护也必须测量，不能只测 Rebuild。

恢复频率通过“初始化一次、后续按证据修复、每轮合并”控制。已知旧请求在 Clone 前结束，避免 5000 次旧事件各自重复克隆 5000 Pod。临界区仍会短暂阻塞其他 Job，实际影响以并发测试为准。

## 13. 可观测性

在现有 cache miss、lifecycle mismatch、stale cleanup 指标基础上，增加或复用：

- Rebuild 次数和结果：按固定 reason/mode/result 分类。
- Rebuild Pod 数量、锁等待时长、锁持有时长和整体耗时直方图。
- 恢复等待原因与次数：observer lag、old owner terminating、name conflict。
- Clone 前拒绝的旧请求数、跳过的重复恢复数。
- UID 前置条件阻止的旧实例操作数。

使用有限枚举标签，禁止把 JobName、Job UID、PodName 或 Pod UID作为 Prometheus 标签。耗时采样和记录尽量在解锁后完成，正常 Pod 热路径的通用 mutex profiling 留给基准/诊断。

已确认旧事件使用低级别日志和计数；真正的观察错误、非法元数据和持续名称冲突保留可定位诊断。日志不能逐个旧 Pod 以 error 级别刷屏。

## 14. 验证与验收

### 14.1 确定性单元测试

使用 barrier/channel、可控 informer store 和 fake clock 固定时序，不依赖 sleep 碰概率。至少覆盖：

1. 锁外旧快照的反例，以及锁内快照不复活已完成 Delete 的 Pod。
2. store Add/Delete 领先 handler；同名新 Pod 快照后仍有旧 Update/Delete。
3. Job A/B 的正常 Delete/Add、合并为 Update、Pod 先于 Job handler 的情况。
4. cache UID 落后与事件 UID 陈旧的区分；Job/Pod informer 进度不一致。
5. Rebuild 提前移除 Pod，PodEvicted → RestartJob 仍正常触发。
6. PodFailed、ExitCode、TaskCompleted/TaskFailed、JobVersion 和 OutOfSync 抑制规则。
7. 同 UID Refresh 不回退 API 成功写入的 Job.Status，Partitions 与 Pods 一致。
8. 同 UID 再次缺失允许第二次恢复；重复提示已满足时跳过。
9. 5000 个 AlreadyExists 同轮最多执行一次全量 Refresh；普通 Pod 事件不触发全量恢复。
10. API GET 后同名 Pod 被替换、请求通过检查后实例被替换，旧 Patch/Delete 不作用于新实例。
11. Pod 级延迟动作、Job/Task 级延迟动作及取消规则；旧 timer 不清理新动作。
12. 旧 cleanup、旧 Job Delete、旧指标更新不影响新 lifecycle。
13. 观察等待不耗尽 Job 执行重试额度；真实执行错误仍遵守既有重试策略。
14. namespace 标签变更、非 VCJob Pod、缺失 owner/非法元数据的索引与恢复处理。

原有 controller、cache、state 和 plugin 测试需通过；对 cache/job 并发路径运行 race detector。临时反例测试只能证明缺陷可触发，不能替代修复后的回归测试。

### 14.2 常规 e2e

将现有 8 Pod 同名重建用例扩展为至少 3 轮，覆盖旧 Pod terminating、新旧 UID、旧 cleanup 后的健康状态和新 Pod 再次删除后的补建。

增加当前生命周期的 PodEvicted/PodFailed 策略、RestartPod、Task/Partition 动作和 timeout 场景。观察 JobVersion、RetryCount、目标 UID 和动作结果，不能只断言最终副本数量。

补充 controller 重启/relist 与同名重建交错；验证所启用插件的独占资源没有被旧动作误删。

### 14.3 独立规模验证

5000 Pod 测试单独运行，不要求每个 PR 在普通 kind 环境启动 5000 个真实运行容器。可分别使用 informer/cache 压测、apiserver 集成测试和有足够容量的真实集群 e2e；前两种不能替代实际调度、运行与 GC 验证。

| 维度 | 测试组合 |
| --- | --- |
| Job Pod 数量 | 8、1000、5000 |
| namespace Pod 数量 | 5000、50000 |
| task/partition | 单 task、多 task、开启 partition |
| 并发负载 | 单大 Job 重建；多个大 Job 错峰/同时重建；持续创建的小 Job |
| 事件进度 | 正常；人为 handler 积压；watch 恢复/relist |
| controller 资源 | 生产 request/limit 配置；受限 CPU；内存压力 |

采集 P50/P95/P99/max 的锁等待和持有、其他 Job 调谐延迟、queue 深度及排空时间、CPU throttling、GC/分配量、API 请求数和 5000 副本恢复时间。记录硬件、Go 版本、对象形态、并发数、CPU 配额、负载发生频率及测试时长，保存原始结果和基线。

正确性门槛：无跨 UID 修改、无永久缺失副本、无新增策略回归。性能门槛：普通路径相对同配置基线无显著退化，冲击结束后队列和内存恢复，其他 Job 的尾延迟满足部署环境已有 SLO。不能用当前微基准凭空确定通用生产 P99 上限；正式合入前须在报告中明确采用的数值门槛及实测结果。

## 15. 实施顺序与文件范围

以下是同一方案的交付顺序，不是架构改造的多个阶段。影响正确性的配套变更应一起验收，不能单独合入快照重建却保留旧 HasPod 策略判断。

| 顺序 | 内容 | 主要文件/目录 |
| --- | --- | --- |
| 1 | UID 接口、初始化状态、owner 索引、窄 informer reader、Clone 前检查 | `pkg/controllers/cache/`、`pkg/controllers/apis/`、`job_controller.go` |
| 2 | 锁内 Rebuild、同 UID Job 状态保留、Pod 当前观察、cleanup | `cache.go`、`job_info.go`、`job_controller_handler.go` |
| 3 | 策略事件与投影分离、AlreadyExists 聚合、恢复等待、syncTask | `job_controller_handler.go`、`job_controller_actions.go`、`job_controller_resync.go` |
| 4 | Pod UID API 条件、目标身份、延迟动作与指标清理 | `job_controller_actions.go`、`job_controller_util.go`、`state/factory.go`、`helpers/`、延迟动作代码、`metrics/` |
| 5 | 确定性回归、已有功能测试、e2e 和规模报告 | cache/job 测试、`test/e2e/jobp/`、正式 benchmark |

业务 API/CRD 不变；用户不需要改 VCJob YAML。不新增多层业务锁。Deployment、裸 Pod 等在 scheduler 中构造的 JobInfo 不由本 jobCache 管理，不在本次删除或替换范围内。

由于不涉及持久化格式迁移，controller 二进制可按既有流程回滚；回滚会重新暴露原生命周期问题。上线先使用小范围实例/环境观察恢复次数、尾延迟、策略触发和 API 错误，再扩大范围。

## 16. 参考依据

- [client-go SharedInformer 契约](https://github.com/kubernetes/client-go/blob/v0.36.1/tools/cache/shared_informer.go#L41-L139)：store 更新与 handler 通知的先后关系，以及 UID 不属于 informer key 的事实。
- [client-go workqueue](https://github.com/kubernetes/client-go/blob/v0.36.1/util/workqueue/queue.go)：相同 item 的 dirty/processing 合并与 Done 后重排机制。
- [Kubernetes API concepts](https://kubernetes.io/docs/reference/using-api/api-concepts/)：API 并发控制与条件更新。
- [Kubernetes UID/ResourceVersion Preconditions](https://github.com/kubernetes/apimachinery/blob/v0.36.1/pkg/apis/meta/v1/types.go)：删除等操作的目标实例前置条件。
- 当前实现涉及 `pkg/controllers/cache/cache.go`、`pkg/controllers/apis/job_info.go`、`pkg/controllers/job/job_controller*.go` 和 `test/e2e/jobp/job_lifecycle.go`。本文的接口和约束是目标设计，实施时须以实际 diff 和验证报告核对完成情况。
