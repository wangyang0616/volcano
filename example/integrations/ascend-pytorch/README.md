# 昇腾 A3/A5 单机 8 卡训练验证

本示例运行一个占用 8 张 NPU 的 Volcano Pod，由 `torchrun` 启动 8 个训练进程，
每个进程使用一张 NPU，DDP 通过 HCCL 同步 ResNet-50 梯度。合成数据可以排除
数据集、存储和 DataLoader 对结果的干扰。

提供两份 Volcano Job 清单：

| 硬件 | 清单 | 镜像产品标签 | Kubernetes NPU 资源 |
| --- | --- | --- | --- |
| Ascend A3 | `volcano-job.yaml` | `a3` | `huawei.com/Ascend910` |
| Ascend A5/950 | `volcano-job-950.yaml` | `950` | `huawei.com/npu` |

## 1. 最小预检

确认 Volcano、默认队列和 Ascend Device Plugin 正常：

```bash
kubectl get crd jobs.batch.volcano.sh
kubectl get pods -n kube-system -o wide | grep -Ei 'ascend|device'
kubectl get queue default
```

查询目标节点实际架构和扩展资源名：

```bash
kubectl get node NPU_NODE -o json | jq '{
  node: .metadata.name,
  arch: .status.nodeInfo.architecture,
  npu: (.status.allocatable |
    with_entries(select(.key | test("Ascend|npu"; "i"))))
}'
```

资源名必须与所选清单一致，并且可分配数量至少为 8。

## 2. 把镜像送到集群节点

Mac 的 Docker/Colima 镜像对远端 Kubernetes 节点不可见。优先推送到内网镜像仓库；
如果只有一个离线节点且运行时是 containerd，也可以直接导出、传输并导入。以下以
A5/950 镜像为例：

```bash
# 在本机执行，生成文件较大。
docker save \
  quay.io/ascend/triton:3.2.2-cann9.1.0-torch_npu2.7.1.post8-950-openeuler24.03-py3.11 \
  | zstd -T0 -3 -o ascend-950-pytorch.tar.zst
scp ascend-950-pytorch.tar.zst A5_NODE:/tmp/

# 在 A5 节点执行。
zstd -dc /tmp/ascend-950-pytorch.tar.zst \
  | sudo ctr -n k8s.io images import -
sudo ctr -n k8s.io images list | grep 'quay.io/ascend/triton'
```

`volcano-job.yaml` 默认从仓库拉取 A3 镜像；`volcano-job-950.yaml` 默认使用已导入
节点运行时的 A5/950 镜像。如果集群有多个 A5 节点而镜像只在其中一个节点上，需要在
清单中配置 `nodeSelector`，或者把镜像导入所有候选节点。

## 3. 提交 30 分钟训练任务

A5/950 环境在本目录执行：

```bash
kubectl create namespace npu-test \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n npu-test create configmap a5-resnet50-script \
  --from-file=train_resnet50_synthetic.py \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n npu-test apply -f volcano-job-950.yaml
kubectl -n npu-test get pods -l app=a5-resnet50-8npu -w
```

Pod 创建后查看日志：

```bash
POD=$(kubectl -n npu-test get pods -l app=a5-resnet50-8npu \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n npu-test logs -f "$POD"
```

最后一行 JSON 会给出全局 images/s 和 P50/P95 step time。8 个 rank 连续运行 30 分钟、
结果为有限值且没有 HCCL 超时或设备健康事件，即完成本轮验证。

如果 NPU 显存不足，把 `--batch-size 64` 调到 `32`。建议首次先把
`--duration 1800` 改成 `120`，通过两分钟冒烟测试后再跑 30 分钟。

这是可重复的计算与通信负载，不是 MLPerf 结果：它不衡量数据加载、模型收敛或精度。
单机任务也不能证明 HyperNode 的跨节点拓扑调度收益。
