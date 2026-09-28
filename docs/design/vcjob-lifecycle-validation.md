# VCJob 生命周期隔离：实施与验证记录

日期：2026-09-24。关联设计：[VCJob 同名重建生命周期隔离](vcjob-lifecycle-isolation.md)。

## 实施范围

保留 VCJob jobCache、全局互斥锁、按 namespace/name 分配 worker 和现有状态机。不修改 CRD、不引入分片锁、不调整 scheduler 管理 Deployment/裸 Pod 的 JobInfo。

- 新增 owner UID informer 索引及只读 `SnapshotReader`；Rebuild 在持锁后读 store、构造并提交投影。同 UID 修复保留 cache 中已确认写入的 Job；不合并旧 Pod 快照。
- `GetLifecycle` 只读轻量身份，`GetForUID` 在 Clone 前检查 UID 和初始化状态。Job 深拷贝，Pod 指针保持 informer 只读约束。
- Pod 回调从当前 store 刷新单个位置；原始失败、退出码和驱逐事件继续独立处理。Task 条件在恢复后可再检查，不因缺少 cache 而永久遗漏。
- AlreadyExists 收集本轮缺失名称，最多重建一次；不将 API GET 结果直接写 cache，不用不完整快照提交状态。等待采用独立退避，不清除真正执行错误的重试计数。
- 身份歧义按 key 限制 live Job GET 频率；最多缓存 4096 个已确认退休 UID，淘汰只影响性能、不改变正确性。
- Pod Patch 使用 UID test，Delete 使用 UID precondition。RestartPod 保留事件 Pod UID；原实例已经消失时，跳过旧目标操作但继续排入普通副本修复。
- 延迟动作检查 Job UID、JobVersion、Pending 条件和动作指针；迟到的 Running 事件不能取消新失败计时器。正常替代 Pod 的 Running 取消规则保留。
- cleanup、Job 指标更新与删除在同一生命周期约束下完成。共享名称的 Service、NetworkPolicy、ConfigMap、Secret 和历史 PodGroup 删除验证 owner，携带资源 UID 前置条件。
- DependsOn 不再把旧 Job UID 的同名 Pod 算作当前 task 已就绪。用户提供的 PVC 复用契约不变。

新增检查的兼容性边界：同名插件资源若不属于当前 Job，不再直接复用、覆盖或删除，而是等待名称释放。资源名称、Service/NetworkPolicy selector 体系未重构；本修复不承诺跨控制器的原子事务或策略事件 exactly-once。

## 自动化回归

已通过：

```sh
go test ./pkg/controllers/... volcano.sh/apis/pkg/apis/helpers -count=1
go test -race ./pkg/controllers/cache ./pkg/controllers/apis ./pkg/controllers/job/... volcano.sh/apis/pkg/apis/helpers -count=1
go test -c ./test/e2e/jobp
go test -c ./test/e2e/jobseq
git diff --check
```

确定性测试覆盖 store 领先回调、旧 Update/Delete 晚到、恢复中 Job UID 切换、第二次修复、状态不回退、partition 一致性、原始策略保留、延迟动作有效性/取消、API UID 条件、旧指标写入/删除、关联资源 ownership，以及真实错误重试额度。

`TestAlreadyExistsBatchRebuildsOnceFor5000Pods` 验证一次 sync 的 5000 次 AlreadyExists 只 List/Rebuild 一次，且不发布不完整 Job.Status；另验证 GET 权限错误不会被观察等待清掉重试计数。它不创建 5000 个真实运行容器。

原有直接调用 cache 的 action/plugin 测试使用 standalone cache；新增生命周期测试使用带 owner 索引的真实 client-go informer store。没有为生产路径放开缺失 UID 的检查。

## 隔离集群 e2e

环境：kind，Kubernetes v1.36.1，Linux ARM64；1 个 control-plane、1 个 worker；Docker VM 4 CPU、约 8 GB 内存。使用独立 kubeconfig，与其他集群隔离。Controller 由当前工作区构建，scheduler/admission 复用本机基线镜像。

已通过 6 个 jobp 用例：

- 8 Pod VCJob 连续 3 轮同名删除/重建，旧 Pod 处于 terminating；新旧 UID、副本恢复、cleanup 后稳定性均检查。
- 每轮再删除当前 Pod，验证 PodEvicted → RestartJob、JobVersion=1、RetryCount=1 和 Service owner UID；此前旧生命周期事件不能增加新 Job 的 Version/RetryCount。
- 第二轮同名重建期间额外重启 controller，验证 informer 重建后继续收敛。
- 最后一次重试保留 Failed Pod、完成后保留 Succeeded Pod。
- 扩容、缩容、缩到零再扩容。

另通过 4 个定向兼容用例，共计 10 个 e2e：

- PodEvicted → 带 timeout 的 RestartPod：原目标已删除仍能补建，其他 Pod UID 保持不变。
- RestartTask：目标 task 全部替换，其他 task 的 Pod UID 保持不变。
- RestartPartition：仅目标 partition 替换，其他 partition/task 的 Pod UID 保持不变。
- 既有 PodFailed → RestartPod 用例通过。该用例配置了 5 分钟 PodEvicted 策略，但此次只验证 PodFailed 重启流程，未等待 5 分钟超时。

三组 e2e 运行结果分别为 `6/6`、`3/3`、`1/1`；jobseq 套件还显示 1 个原有 Pending 用例，它不属于此次选中的验证用例。测试均为开发过程中的增量回归，最终定向策略用例在含完整目标 UID 与补建逻辑的 controller 镜像上执行。

环境说明：kind 的镜像加载器不识别节点的 containerd v4 配置，测试镜像通过节点自身的 containerd 按 ARM64 平台导入；未修改生产代码规避。工作负载使用已有本地镜像，其中 `busybox:latest` 是本地 Alpine/BusyBox 工具集标签，并非重新拉取的官方 BusyBox 镜像。

## 5000 Pod 微基准

可复现文件：`pkg/controllers/cache/lifecycle_benchmark_test.go`。

```sh
GOMAXPROCS=1 go test ./pkg/controllers/cache -run '^$' \
  -bench 'BenchmarkLifecycle/pods=5000/' -benchmem -benchtime=300ms -count=2
```

Apple M3、darwin/arm64、Go 1.26.3；目标 Job 5000 Pod，namespace 5000/50000 Pod；单 task，partition 场景按 8 Pod 一组。两次运行的平均操作耗时：

| 操作 | 结果 |
| --- | --- |
| owner 索引 Rebuild，无 partition | 1.21–1.31 ms/op，约 560 KB/op |
| owner 索引 Rebuild，开启 partition | 1.99–2.07 ms/op，约 775 KB/op |
| Clone 前拒绝旧 UID | 约数十 ns/op，0 B/op；仅身份检查，不代表整个 worker |
| owner 索引维护 + 单 Pod 实际投影更新 | 约 1 μs/op；完整原始值见本地日志 |

这些值包含基准中的接口与统计开销，不是精确锁持有 P99，也不等价于容器 1 CPU limit。对象模板较小，未模拟生产大小的 Spec/Status；本机也存在构建和测试集群活动，不能作为严格隔离的性能回归阈值。

## 尚需生产规模验收

- 5000 个真实 Pod 的删除/GC/重新调度及恢复耗时。
- 多个大 Job 与小 Job 并发，生产 CPU request/limit 下的锁等待/持有 P50/P95/P99、队列排空、内存/GC 和 CPU throttling。
- 按部署环境已有 SLO 确定性能门槛；当前数据不能证明通用生产尾延迟上限。

本地运行日志位于 `/tmp/volcano-lifecycle-validation.IMdvFb/`，单元/race/微基准日志为 `/tmp/volcano-lifecycle-{controllers,race,benchmark}.log`；这些临时文件不是仓库交付的一部分，长期保存应由 CI 或评审附件承接。

验证结束后已删除仅为本次回归创建的 `codex-vcjob-lifecycle` 临时集群；测试记录、controller 日志及本地镜像保留。

## 2026-09-28：全量检视后的补充修复

继续保留 jobCache 和一把全局锁，修复以下六个边界；不扩大到 scheduler cache 或 CRD 改造：

1. **Job 对象所有权**：Add/Update 入 cache、GetStatus 出 cache 均深拷贝。插件可以修改本次调谐持有的 Job，但必须经过显式状态更新才能进入 cache，避免 `ControlledResources` 与 Clone 并发读写。
2. **SSH 错误分类**：使用 `%w` 保留资源占名错误类型，旧 Secret 未释放走观察等待，不消耗执行失败额度、不触发重试耗尽终止。
3. **恢复期间保留新事件**：读到的旧 Job 恰好被退休，不代表当前请求也已经过期；只丢弃自身 UID 已确认退休的请求，保留新 UID 的原始策略和退出码。
4. **Ray 资源隔离**：head Service 复用检查 Job owner UID；删除检查归属并携带 Service UID 前置条件，旧动作不能删除新 Service。
5. **删除终态**：为内部 JobInfo 增加不可回退的 Deleted 标记。已删除 entry 与尚未收到 Job 的 Pod-only placeholder 区分，退休 UID 记录淘汰后也不能由迟到的 Update/Add/Rebuild 复活。
6. **观察等待与执行失败分离**：AlreadyExists 快照修复返回专用等待结果，不伪装成功；等待与同请求 cache 恢复均不 Forget，保留原始动作和已累计的真实执行失败次数。真正成功或明确淘汰旧请求时才清理计数。

新增正式单测覆盖上述六项，包括退休记录超过 4096 条后淘汰、恢复后实际执行 ExitCode 策略、执行失败→等待→再次失败→成功的计数变化，以及插件修改与 cache Clone 并发。SSH 单测将执行重试上限设为 0，确认资源占名不触发 TerminateJob。

本轮全量 controller/helpers 单测及 cache/apis/job/helpers race 检测均通过；新增回归单测另以 `-race -count=10` 重复通过。日志为 `/tmp/volcano-lifecycle-followup-{all,unit,race,repeat}.log`。

新增两个 e2e 分别用 finalizer 固定旧 SSH Secret、Ray head Service：创建同名新 Job 后确认其保持 Pending、Version/RetryCount 为 0、不提前创建新 Pod，也不接管旧资源；释放 finalizer 后确认恢复及新资源 owner/UID。占名观察窗口为 15 秒，重试额度耗尽边界由上述确定性单测验证。Ray 用例只验证 Service 生命周期，不运行 Ray 应用。

本轮在重新创建的专用 kind 集群 `codex-vcjob-followup` 中验证，Kubernetes v1.36.1、Linux ARM64、1 个 control-plane 和 1 个 worker。Controller 镜像 `volcanosh/vc-controller-manager:codex-lifecycle-followup` 由本轮全部生产代码构建，其他组件使用原有基线镜像。全部 **12 个选定 e2e 通过**：

| 运行组 | 结果 | 覆盖 |
| --- | --- | --- |
| 生命周期隔离 | 6/6，385.5 秒 | SSH/Ray 旧资源占名；8 Pod 连续三轮同名重建及中途 controller 重启；RestartPod/Task/Partition |
| 原有功能兼容 | 5/5，88.5 秒 | Failed/Succeeded Pod 保留；扩容、缩容、缩零再扩容 |
| 原有失败策略 | 1/1，29.6 秒 | PodFailed → RestartPod |

最后一组套件另外显示 1 个原有 Pending 用例，不属于选中的验证目标；配置中的 PodEvicted 5 分钟超时未在该组等待验证。三组测试均使用同一个包含本轮完整修复的 controller 镜像。

集群、构建及 e2e 日志保存在 `/tmp/volcano-lifecycle-followup.S786qk/`，其中 `e2e.log`、`compat-e2e.log`、`restartpod-e2e.log` 分别对应上表，`controller.log` 为结束时的 controller 日志。测试镜像继续使用已有本地标签，镜像导入方式及 BusyBox 标签说明与前次相同。

验证完成后已删除 `codex-vcjob-followup` 临时集群及其两个节点容器；本地日志与镜像保留。

5000 Pod 微基准再次通过：无 partition 的 Rebuild 为 1.52–2.05 ms/op、约 562 KB/op；开启 partition 为 2.07–2.26 ms/op、约 777 KB/op。本轮与镜像构建及集群启动并发，不能据此作严格前后性能比较，也不代表锁持有 P99 或 5000 个真实 Pod 的生产验收。原始日志为 `/tmp/volcano-lifecycle-followup-benchmark.log`。

## 2026-09-28：第二轮兼容性修复

修复再次检视中确定性复现的四项问题，仍保留 jobCache 与一把全局锁：

1. **最新 Pod 投影抢先结束 Job**：原始 PodFailed/TaskCompleted 请求尚未执行时，普通 sync 不能先按最新计数把 Running Job 变为 Failed/Completed。增加当前 Pod UID 的终态观察确认，在动作成功后才确认；失败重试、延迟策略、后续元数据更新不能提前放行。延迟动作合并取消时同步确认被合并的观察并补一次调谐。
2. **Pending 初始化标记未持久化**：显式保存变化的 ControlledResources，包括后续初始化失败前已成功的步骤。保留 Job 深拷贝；svc 仅在全部子资源就绪后标记完成，部分创建后的清理仍按 owner/UID 执行，不依赖完成标记。
3. **删除后的旧请求放大 live GET**：Job lister NotFound 时优先利用请求自身 UID 的退休证明。5000 个已知旧 UID 请求产生 **0 次** live Job GET；首次未知但已删除的同一 UID，5000 个请求共 **1 次** GET。
4. **VolcanoJobSupport 关闭仍永久重排**：没有 Job informer 时结束无法恢复的请求，不再周期性重排原生负载的 PodGroup 通知。

正式回归覆盖：store 领先 callback 的 PodFailed → RestartJob；无策略的全部成功/失败；MinSuccess 与终态 timeout；真实动作失败后的再次处理；合并 timeout 后不残留终态阻塞；初始 List 已有终态 Pod、Pod 先于 Job cache；标记的 Clone 隔离、同 UID Rebuild、任务注解移动、Pod/Job UID 替换及删除；插件部分成功/失败、状态写入失败、无变化时不重复写状态；功能开关和上述 5000 请求边界。

包含全部正式测试的全量 controller/helpers 单测、cache/apis/job/helpers race 检测通过。新增核心用例另以 `-race -count=10` 重复通过；末尾补充的初始化状态写入失败和初始 List 用例各单独重复 10 次通过。原始日志为 `/tmp/volcano-lifecycle-fix-round2-{unit,race,repeat,status-error,initial-list}.log`。

### 新增开销测量

Apple M3、darwin/arm64、Go 1.26.3，GOMAXPROCS=1，5000 Pod，小型 Job 模板。并发编译和 race 测试结束后复测两次，专用 kind 集群仍在运行，不能作为严格隔离的回归阈值。

| 测量项 | 两次运行范围 |
| --- | --- |
| 无 partition 的 Rebuild，namespace 5000/50000 Pod | 1.38–2.54 ms/op，约 562 KB/op |
| 有 partition 的 Rebuild | 2.07–2.33 ms/op，约 777 KB/op |
| 5000 个 Running Pod 的新增终态检查 | 44.8–46.0 μs/op，0 B/op |
| 5000 个已确认终态 Pod 的新增检查 | 278.9–284.7 μs/op，0 B/op |
| 小型 JobInfo Clone，无终态标记 | 0.259–0.272 ms/op，219520 B/op |
| 相同 JobInfo Clone，含 5000 个终态标记 | 0.482–0.485 ms/op，438000 B/op |

终态检查在 cache 锁外，标记确认复用原锁；Clone 会增加终态标记复制，最坏的全终态样本每次约多 0.21–0.23 ms 和 218480 字节临时分配。正常 Running Pod 不分配标记。不能把单次检查成本理解为 5000 请求总成本：完整队列排空仍包含逐请求 Clone、状态机、API 和 GC，应纳入生产压测。

微基准源码为 `pkg/controllers/cache/lifecycle_benchmark_test.go`、`pkg/controllers/job/state/observation_test.go`；复测日志为 `/tmp/volcano-lifecycle-fix-round2-benchmark-final.log`。早先与编译/race 并发的数据保留在 `benchmark.log`，不用于前后比较。5000 个真实运行 Pod 的生产规模验收仍未完成。

### 本轮隔离集群

专用 kind 集群 `codex-vcjob-policy-fix`，独立 kubeconfig，Kubernetes v1.36.1、Linux ARM64，1 个 control-plane、1 个 worker。最终 controller 镜像为 `volcanosh/vc-controller-manager:codex-lifecycle-policy-final`，包含本轮全部生产逻辑；scheduler/admission 仍使用本地基线镜像。镜像导入和 BusyBox 本地标签说明与前轮相同。

新增三个 e2e 在 PodGroup 未获准、没有 Pod 的 Pending 阶段，分别终止 svc/ssh/ray 作业，验证初始化标记已写入、插件资源被清理、Job 本体未删除，以排除 GC 掩盖问题。svc 还检查 ConfigMap 和 NetworkPolicy。

最终共 **21 个选定 e2e 全部通过**，三组均使用上述最终 controller 镜像：

| 运行组 | 结果 | 覆盖 |
| --- | --- | --- |
| 生命周期隔离 | 6/6，337.8 秒 | 三个 Pending 插件清理；SSH/Ray 旧资源占名；8 Pod 三轮同名重建及中途 controller 重启 |
| 兼容性 | 12/12，222.9 秒 | RestartPod/Task/Partition；Failed/Succeeded Pod 保留；扩容/缩容/缩零恢复；MinSuccess；Completed/Aborted/Terminated Job 清理 |
| 既有失败策略 | 3/3，75.5 秒 | PodFailed → RestartJob、TerminateJob、RestartPod |

jobseq 套件另外显示 1 个原有 Pending 用例，不属于选中的测试目标；RestartPod 用例中配置的 PodEvicted 5 分钟超时未在本组等待验证。日志目录为 `/tmp/volcano-lifecycle-policy.np10XG/`，三组分别对应 `lifecycle-e2e.log`、`compat-e2e.log`、`policy-e2e.log`；`controller.log` 为结束时保存的控制器日志。

验证结束后已删除 `codex-vcjob-policy-fix` 临时集群及两个节点容器；临时测试对象随集群删除，不保留恢复用途。本地测试日志和构建镜像保留，代码未提交、未推送。
