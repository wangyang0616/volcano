#!/usr/bin/env python3
"""Single-node Ascend NPU DDP smoke/performance workload.

The model is a dependency-free ResNet-50. Synthetic input intentionally removes
storage and data-loader variability so that the result mainly reflects NPU
compute plus HCCL gradient synchronization.
"""

import argparse
import json
import os
import time
from pathlib import Path

import torch
import torch.distributed as dist
import torch.nn as nn
import torch_npu  # noqa: F401 - registers the NPU backend with PyTorch
from torch.nn.parallel import DistributedDataParallel as DDP


def conv3x3(in_channels, out_channels, stride=1):
    return nn.Conv2d(
        in_channels,
        out_channels,
        kernel_size=3,
        stride=stride,
        padding=1,
        bias=False,
    )


def conv1x1(in_channels, out_channels, stride=1):
    return nn.Conv2d(
        in_channels, out_channels, kernel_size=1, stride=stride, bias=False
    )


class Bottleneck(nn.Module):
    expansion = 4

    def __init__(self, in_channels, channels, stride=1, downsample=None):
        super().__init__()
        self.conv1 = conv1x1(in_channels, channels)
        self.bn1 = nn.BatchNorm2d(channels)
        self.conv2 = conv3x3(channels, channels, stride)
        self.bn2 = nn.BatchNorm2d(channels)
        self.conv3 = conv1x1(channels, channels * self.expansion)
        self.bn3 = nn.BatchNorm2d(channels * self.expansion)
        self.relu = nn.ReLU(inplace=True)
        self.downsample = downsample

    def forward(self, x):
        identity = x
        out = self.relu(self.bn1(self.conv1(x)))
        out = self.relu(self.bn2(self.conv2(out)))
        out = self.bn3(self.conv3(out))
        if self.downsample is not None:
            identity = self.downsample(x)
        return self.relu(out + identity)


class ResNet(nn.Module):
    def __init__(self, layers=(3, 4, 6, 3), num_classes=1000):
        super().__init__()
        self.in_channels = 64
        self.conv1 = nn.Conv2d(
            3, 64, kernel_size=7, stride=2, padding=3, bias=False
        )
        self.bn1 = nn.BatchNorm2d(64)
        self.relu = nn.ReLU(inplace=True)
        self.maxpool = nn.MaxPool2d(kernel_size=3, stride=2, padding=1)
        self.layer1 = self._make_layer(64, layers[0])
        self.layer2 = self._make_layer(128, layers[1], stride=2)
        self.layer3 = self._make_layer(256, layers[2], stride=2)
        self.layer4 = self._make_layer(512, layers[3], stride=2)
        self.avgpool = nn.AdaptiveAvgPool2d((1, 1))
        self.fc = nn.Linear(512 * Bottleneck.expansion, num_classes)

        for module in self.modules():
            if isinstance(module, nn.Conv2d):
                nn.init.kaiming_normal_(module.weight, mode="fan_out", nonlinearity="relu")
            elif isinstance(module, (nn.BatchNorm2d, nn.GroupNorm)):
                nn.init.constant_(module.weight, 1)
                nn.init.constant_(module.bias, 0)

    def _make_layer(self, channels, blocks, stride=1):
        out_channels = channels * Bottleneck.expansion
        downsample = None
        if stride != 1 or self.in_channels != out_channels:
            downsample = nn.Sequential(
                conv1x1(self.in_channels, out_channels, stride),
                nn.BatchNorm2d(out_channels),
            )
        layers = [Bottleneck(self.in_channels, channels, stride, downsample)]
        self.in_channels = out_channels
        layers.extend(Bottleneck(self.in_channels, channels) for _ in range(1, blocks))
        return nn.Sequential(*layers)

    def forward(self, x):
        x = self.maxpool(self.relu(self.bn1(self.conv1(x))))
        x = self.layer1(x)
        x = self.layer2(x)
        x = self.layer3(x)
        x = self.layer4(x)
        x = self.avgpool(x)
        return self.fc(torch.flatten(x, 1))


def percentile(values, fraction):
    ordered = sorted(values)
    return ordered[min(int((len(ordered) - 1) * fraction), len(ordered) - 1)]


def train_step(model, optimizer, criterion, images, labels, use_bf16):
    optimizer.zero_grad(set_to_none=True)
    with torch.autocast(device_type="npu", dtype=torch.bfloat16, enabled=use_bf16):
        loss = criterion(model(images), labels)
    loss.backward()
    optimizer.step()
    return loss


def parse_args():
    parser = argparse.ArgumentParser()
    parser.add_argument("--duration", type=int, default=1800, help="measured seconds")
    parser.add_argument("--batch-size", type=int, default=64, help="per-NPU batch")
    parser.add_argument("--image-size", type=int, default=224)
    parser.add_argument("--warmup-steps", type=int, default=20)
    parser.add_argument("--log-every", type=int, default=20)
    parser.add_argument("--precision", choices=("bf16", "fp32"), default="bf16")
    parser.add_argument("--output", default="/output/metrics.json")
    return parser.parse_args()


def main():
    args = parse_args()
    local_rank = int(os.environ["LOCAL_RANK"])
    rank = int(os.environ["RANK"])
    world_size = int(os.environ["WORLD_SIZE"])
    device = torch.device(f"npu:{local_rank}")
    torch.npu.set_device(device)
    dist.init_process_group(backend="hccl", init_method="env://")

    torch.manual_seed(2026 + rank)
    model = ResNet().to(device)
    model = DDP(
        model,
        device_ids=[local_rank],
        output_device=local_rank,
        broadcast_buffers=False,
        gradient_as_bucket_view=True,
    )
    optimizer = torch.optim.SGD(model.parameters(), lr=0.01, momentum=0.9)
    criterion = nn.CrossEntropyLoss().to(device)
    images = torch.randn(
        args.batch_size, 3, args.image_size, args.image_size, device=device
    )
    labels = torch.randint(0, 1000, (args.batch_size,), device=device)
    use_bf16 = args.precision == "bf16"

    if rank == 0:
        print(
            json.dumps(
                {
                    "event": "start",
                    "torch": torch.__version__,
                    "world_size": world_size,
                    "model": "resnet50",
                    "precision": args.precision,
                    "batch_per_npu": args.batch_size,
                    "global_batch": args.batch_size * world_size,
                    "duration_seconds": args.duration,
                },
                sort_keys=True,
            ),
            flush=True,
        )

    model.train()
    for _ in range(args.warmup_steps):
        train_step(model, optimizer, criterion, images, labels, use_bf16)
    torch.npu.synchronize()
    dist.barrier()

    started = time.perf_counter()
    step_times = []
    step = 0
    stop = torch.zeros(1, dtype=torch.int32, device=device)
    loss = None
    while True:
        step_started = time.perf_counter()
        loss = train_step(model, optimizer, criterion, images, labels, use_bf16)
        torch.npu.synchronize()
        step += 1

        if rank == 0:
            step_times.append(time.perf_counter() - step_started)
            elapsed = time.perf_counter() - started
            if step % args.log_every == 0:
                recent = step_times[-args.log_every :]
                throughput = (
                    args.batch_size * world_size * len(recent) / sum(recent)
                )
                print(
                    json.dumps(
                        {
                            "event": "progress",
                            "step": step,
                            "elapsed_seconds": round(elapsed, 1),
                            "loss": round(float(loss.detach().cpu()), 5),
                            "global_images_per_second": round(throughput, 2),
                        },
                        sort_keys=True,
                    ),
                    flush=True,
                )
            if elapsed >= args.duration:
                stop.fill_(1)

        dist.broadcast(stop, src=0)
        if stop.item() == 1:
            break

    elapsed = time.perf_counter() - started
    if rank == 0:
        metrics = {
            "event": "result",
            "model": "resnet50",
            "world_size": world_size,
            "precision": args.precision,
            "batch_per_npu": args.batch_size,
            "global_batch": args.batch_size * world_size,
            "steps": step,
            "elapsed_seconds": round(elapsed, 3),
            "global_images_per_second": round(
                step * args.batch_size * world_size / elapsed, 3
            ),
            "step_time_p50_ms": round(percentile(step_times, 0.50) * 1000, 3),
            "step_time_p95_ms": round(percentile(step_times, 0.95) * 1000, 3),
            "final_loss": round(float(loss.detach().cpu()), 6),
        }
        print(json.dumps(metrics, sort_keys=True), flush=True)
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(json.dumps(metrics, indent=2) + "\n", encoding="utf-8")

    dist.barrier()
    dist.destroy_process_group()


if __name__ == "__main__":
    main()
