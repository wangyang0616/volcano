# VCJob 同名重建：修改前后对比测试

使用同一份脚本，分别对**已部署的修改前、修改后 controller** 各运行 100 轮，统计新 Job 副本无法创建的次数。脚本需要 Python 3.9+ 和 `kubectl`，没有额外 Python 依赖。

脚本仅创建、删除自己生成的测试命名空间及其中的资源，不部署、重启或切换 controller。`compare` 完全离线。建议使用独立的本地测试集群。

## 1. 准备两版 controller

本次修复对应的准确源码版本：

- 修改前：`8708aca48a4c6cb167a3b509aee972d912f2ab4e`。
- 修改后：`290c0c98e6f6e9c127cf7c0b5f6c28d9f4aae42c`，PR #6029 的修复提交。

不要用不同版本的 Volcano 发行镜像直接替代这组版本，否则其他改动也会影响结果。脚本分支基于修改后版本，但实际测试对象取决于集群中运行的 controller 镜像。

可在仓库根目录准备两个独立目录并构建镜像。下面以 ARM64 节点为例，AMD64 节点将 `linux/arm64` 改为 `linux/amd64`：

```sh
git worktree add --detach /tmp/vcjob-before 8708aca48a4c6cb167a3b509aee972d912f2ab4e
git worktree add --detach /tmp/vcjob-after 290c0c98e6f6e9c127cf7c0b5f6c28d9f4aae42c

make -C /tmp/vcjob-before vc-controller-manager-image \
  IMAGE_PREFIX=vcjob-test TAG=before DOCKER_PLATFORMS=linux/arm64
make -C /tmp/vcjob-after vc-controller-manager-image \
  IMAGE_PREFIX=vcjob-test TAG=after DOCKER_PLATFORMS=linux/arm64
```

将这两个镜像加载到集群节点，或上传到节点可以访问的镜像仓库。kind 示例：

```sh
kind load docker-image vcjob-test/vc-controller-manager:before \
  vcjob-test/vc-controller-manager:after --name YOUR_KIND_CLUSTER
```

集群需已安装兼容的 Volcano CRD、scheduler、admission 和 controller，`default` Queue 可用。准备能容纳至少 8 个运行中 Pod 的资源，并为新旧 Pod 重叠及诊断 Job 留出余量。每个测试 Pod 请求 10m CPU、16Mi 内存。提前在节点准备 `busybox:1.36.1`，也可用 `--image` 指定提供 `sh` 和 `sleep` 的可访问镜像，建议固定 digest。

以下示例假设 Helm release 为 `volcano`，controller Deployment 和容器名均为 `volcano-controllers`。自定义安装需同步调整部署命令和脚本的 `--controller-namespace`、`--controller-deployment`。

## 2. 修改前后各运行 100 次

在**脚本所在分支的仓库根目录**运行以下命令。仅切换 controller 镜像，保持 scheduler、admission、Queue、节点、controller 参数和配置一致，包括 `--max-requeue-num`。本地加载镜像时 controller 的 `imagePullPolicy` 需允许使用节点缓存。

```sh
REPRO_CONTEXT=YOUR_LOCAL_CONTEXT
REPRO_ORIGINAL_IMAGE=$(kubectl --context "$REPRO_CONTEXT" -n volcano-system \
  get deployment volcano-controllers \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="volcano-controllers")].image}')

kubectl --context "$REPRO_CONTEXT" -n volcano-system set image \
  deployment/volcano-controllers volcano-controllers=vcjob-test/vc-controller-manager:before
kubectl --context "$REPRO_CONTEXT" -n volcano-system rollout status \
  deployment/volcano-controllers --timeout=180s

python3 hack/vcjob-recreation/reproduce.py run \
  --context "$REPRO_CONTEXT" --label before-8708aca48 \
  --iterations 100 --output /tmp/vcjob-results-before
```

修改前出现复现时脚本退出码为 `1`，属于预期的实验结果；退出码 `2` 表示有未判定、未完成或环境异常。查看结果后继续切换版本，不要用 `&&` 将两轮测试串联。

```sh
kubectl --context "$REPRO_CONTEXT" -n volcano-system set image \
  deployment/volcano-controllers volcano-controllers=vcjob-test/vc-controller-manager:after
kubectl --context "$REPRO_CONTEXT" -n volcano-system rollout status \
  deployment/volcano-controllers --timeout=180s

python3 hack/vcjob-recreation/reproduce.py run \
  --context "$REPRO_CONTEXT" --label after-290c0c98e \
  --iterations 100 --output /tmp/vcjob-results-after

python3 hack/vcjob-recreation/reproduce.py compare \
  --before /tmp/vcjob-results-before --after /tmp/vcjob-results-after
```

测试完成后，可在同一个 shell 中恢复原镜像：

```sh
kubectl --context "$REPRO_CONTEXT" -n volcano-system set image \
  deployment/volcano-controllers "volcano-controllers=$REPRO_ORIGINAL_IMAGE"
kubectl --context "$REPRO_CONTEXT" -n volcano-system rollout status \
  deployment/volcano-controllers --timeout=180s
```

首次使用建议先将两轮都改成 `--iterations 3`，使用新的输出目录完成冒烟验证。脚本拒绝覆盖已有输出目录。

## 每轮测试做什么

1. 创建 8 副本的旧 Job，等待所有旧 Pod Running，排除初始镜像和调度问题。
2. 默认给一个旧 Pod 添加测试专用 finalizer，删除旧 Job，以后台垃圾回收保留重叠窗口。
3. 确认旧 Job 已删除、旧 Pod 正在删除，间隔 0–200ms 后创建**同 namespace/name、不同 UID** 的新 Job。
4. 确认新旧实例确实重叠，保持旧 Pod 名称 20 秒，再移除测试 finalizer。
5. **确认全部旧 Pod 真正消失后**，观察新 Job 能否在 60 秒内创建全部副本。
6. 持续观察 15 秒，再删除新 Job 的一个 Pod，通过显式 `PodEvicted -> RestartJob` 策略验证后续调谐仍能补齐副本。这可以发现“Pod 已创建，但旧 cleanup 随后删除了缓存”的情况。
7. 收集结果并清理本轮资源，再进入下一轮。

每轮内部删除、重建的 Job 名称相同；不同轮使用 `recreate-0001`、`recreate-0002` 等名字，避免上一轮坏缓存直接污染下一轮的初始创建。每轮测试的是一次受控的同名实例更替，不代表高并发持续创删的全部时序。

默认仅占名和稳定性观察就需要约 **58 分钟/版本**，加上创建、恢复和清理会更久。一次只运行一份脚本。不要在同一轮实验中切换镜像、重启 controller 或修改配置。

### 自然删除模式

如果希望更接近业务删除过程，前后两轮都增加：

```sh
--mode natural --grace-seconds 10
```

此模式不添加 finalizer，容器忽略 TERM，依靠终止宽限期形成重叠。若未观察到新旧 Pod 重叠，该轮记为未判定。finalizer 模式扩大竞争窗口，更适合先验证故障路径；两种模式的结果不能混在一起比较，也不能将受控实验比例直接作为生产环境发生概率。

其他参数可通过 `run --help` 查看。随机种子默认固定，每轮删除重建间隔按相同序列生成。

## 结果与判断口径

| 结果 | 含义 |
| --- | --- |
| `passed` | 新 UID 的副本已补齐，稳定性检查和后续 Pod 删除恢复也通过 |
| `reproduced` | 旧 Pod 已释放名称，新实例仍缺副本；同配置的新诊断 Job 能创建副本，controller 期间没有重启或切换 |
| `inconclusive` | API/权限错误、初始 Pod 无法运行、没有形成重叠、旧资源未释放、诊断 Job 也无法创建、观察期限后才恢复或实验被中断等 |

副本数只统计 owner 为新 Job UID 且未处于删除状态的 Pod。**Pod 已创建但 Pending 不算“副本无法创建”。** 超时判定反映限定观察窗口内的故障特征，具体根因仍需结合保存的日志确认。

输出包括：

- `results.csv`、`results.jsonl`：逐轮结果、阶段、耗时、新旧 UID 和未判定原因；每轮即时写入。
- `summary.json`：计划/实际轮数、通过/复现/未判定次数、有效样本数、观察复现率及区间。
- `metadata.json`：脚本哈希、参数、Kubernetes 版本、节点容量、controller 镜像实际 imageID、启动参数、Pod UID 和重启次数。
- `trial-NNNN/result.json`：每轮详情；失败或未判定轮次另保存 Job、Pod、PodGroup、Event 及本轮 controller 日志（每容器最多 3000 行）。

`复现率 = reproduced / (passed + reproduced)`，未判定和未运行的轮次单独列出。比如实际跑了 100 轮，其中 10 次复现、85 次通过、5 次未判定，则观察复现率为 `10/95`，同时明确有 5 次未判定。

`compare` 会检查脚本和实验参数一致、controller 实际镜像不同。检测到节点/Kubernetes 版本、controller 参数/资源或工作负载实际镜像不同，会拒绝给出修复有效的结论。集群负载、外部配置和未记录的因素仍需人为保持一致。

- 修改前有复现、修改后 100 个有效样本均通过：支持当前场景下修复有效。
- 修改前也为 `0/100`：本次实验没有触发基线缺陷，无法据此证明修复效果。
- 修改后仍有复现：根据失败阶段和证据继续排查。
- `0/100` 不证明缺陷绝不发生。假设各轮独立且条件一致，零失败时发生率的单侧 95% 上界约为 **2.95%**。连续运行共享 controller，独立性只是近似假设；还应覆盖其他时序和负载。

## 中断与清理

正常结束、Ctrl-C、SIGTERM 会移除测试自己的 finalizer 并删除测试命名空间，证据保留在本地。不会清空其他 finalizer，也不会强制删除 Pod。

SIGKILL、机器断电或 API 不可用时，资源可能残留。恢复访问后，使用对应运行记录清理；命名空间 UID 和运行标签不匹配时脚本会拒绝操作：

```sh
python3 hack/vcjob-recreation/reproduce.py cleanup \
  --context "$REPRO_CONTEXT" --metadata /tmp/vcjob-results-before/metadata.json
```

所需权限：读取节点、Queue、controller Deployment/Pod/日志；创建、读取、删除测试命名空间及 VCJob；读取、删除、patch 测试 Pod；读取测试 PodGroup 和 Event。脚本不读取 Secret 或复制 kubeconfig。

## 离线检查

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s hack/vcjob-recreation -p 'test_*.py' -v
```

这些测试使用模拟客户端，检查实验流程、异常分类、UID 保护、finalizer 清理和统计，不能替代真实集群上的修改前后实验。
