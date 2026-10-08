# VCJob 同名重建测试

准备：Python 3.9+、`kubectl`，当前 context 指向已安装 Volcano 的测试集群，`default` Queue 可用。

## 一条命令启动

在仓库根目录执行：

```sh
python3 hack/vcjob-recreation/reproduce.py
```

默认使用当前 kubectl context，运行 **100 轮、每轮 8 副本**。始终复用同一个 Job 名称；每轮创建的新 Job 会成为下一轮要删除的旧 Job。奇数轮在旧 Pod 仍存在时重建，偶数轮在旧 Pod 全部删除并等待 0.5 秒后重建。测试会创建独立命名空间，结束后自动清理。启动时打印集群 context 和结果目录：`_artifacts/vcjob-recreation/<时间>-<编号>/`。

默认使用 `busybox:1.36.1`，每个 Pod 请求 10m CPU、16Mi 内存。集群需有足够余量容纳新旧 Pod 重叠；默认 controller 为 `volcano-system/volcano-controllers`。完整运行通常超过一小时，先试跑 3 轮可执行：

```sh
python3 hack/vcjob-recreation/reproduce.py run --iterations 3
```

常用可选参数：

- `--context NAME`：指定集群。
- `--image IMAGE`：指定提供 `sh` 和 `sleep` 的测试镜像。
- `--controller-namespace NS --controller-deployment NAME`：指定 controller。
- `--mode natural --grace-seconds 10`：奇数轮使用终止宽限期形成重叠。默认 finalizer 模式会在奇数轮将一个旧 Pod 对象保留 20 秒。
- `--output DIR --label NAME`：指定结果目录和报告名称。结果目录需尚不存在。

参数放在 `run` 后，完整列表见 `python3 hack/vcjob-recreation/reproduce.py run --help`。

## 查看结果

结束时输出通过、复现、未判定次数及复现率。结果目录中：

- `summary.json`：汇总及两种时序各自的结果；复现率为 `复现次数 / (通过次数 + 复现次数)`。
- `results.csv`：每轮结果和原因；失败轮次的资源快照、controller 日志保存在 `trial-NNNN/`。
- `metadata.json`：集群、镜像和测试参数。

每轮会删除旧 Job，按对应时序创建同名新 Job，然后检查副本创建和后续调谐恢复。新旧 Pod 按 Job UID 区分。Pod 已创建但 Pending 不算创建失败，环境异常单独记为未判定。

退出码：`0` 全部通过，`1` 有复现，`2` 有未判定、未完成或运行错误。`0/100` 表示本次未复现，不代表发生概率绝对为零。

## 对比两次结果（可选）

分别在两个待测版本就绪后运行，保持测试参数一致：

```sh
python3 hack/vcjob-recreation/reproduce.py run --label before --output /tmp/vcjob-before
python3 hack/vcjob-recreation/reproduce.py run --label after --output /tmp/vcjob-after
python3 hack/vcjob-recreation/reproduce.py compare --before /tmp/vcjob-before --after /tmp/vcjob-after
```

两次测试分别执行，不要用 `&&` 串联。对比会检查参数、实际镜像和环境信息；若修改前也未复现，则无法据此判断修复效果。
脚本时序已更新，先前脚本产生的结果不能与这版直接比较，需使用这版在修改前和修改后各运行一次。

## 中断后清理

Ctrl-C 会保存已有结果并清理。若进程被强杀或清理失败，恢复集群访问后执行：

```sh
python3 hack/vcjob-recreation/reproduce.py cleanup --context YOUR_CONTEXT --metadata RESULT_DIR/metadata.json
```
