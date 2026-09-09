# ARCSync 调度器使用指南

本调度器插件旨在解决 GitHub ARC (Actions Runner Controller) 在 NPU 资源不足时，Runner Pod 提前启动导致 GitHub Insight 统计排队时间不准确的问题。

## 1. 核心逻辑
- **全局预检**：在 Runner Pod 调度前，`ARCSync` 插件会扫描集群内所有节点。
- **资源核算**：对每个节点，通过以下优先级计算 NPU 剩余量：
    1. 检查 Pod 标签 `ascend-ci.com/npu-count`。
    2. 若无标签，则检查容器的 `Resources.Requests`。
- **调度决策**：只要集群中**至少有一个节点**能满足后续 Workflow Pod 的需求，Runner Pod 即可正常调度（允许调度到任何节点，不局限于有卡的节点）；否则，Runner Pod 保持 `Pending`。

## 2. 标签 (Label) 使用说明

### Runner Pod (声明需求)
在 Runner Pod 模板中添加以下标签，以便调度器进行预检：

| 标签 Key | 示例值 | 说明 |
| :--- | :--- | :--- |
| `ascend-ci.com/required-npu-count` | `"1"` | **必填**。触发拦截逻辑并声明所需 NPU 数量。 |
| `ascend-ci.com/npu-resource-domain` | `"huawei.com"` | **必填**。资源域名。 |
| `ascend-ci.com/npu-resource-model` | `"ascend-310"` | **必填**。NPU 型号。 |

### Workflow Pod (状态标识)
为了使调度器核算更精准，建议 Workflow Pod 包含以下标签：

| 标签 Key | 示例值 | 说明 |
| :--- | :--- | :--- |
| `ascend-ci.com/npu-count` | `"1"` | 声明当前 Pod 实际占用的 NPU 数量。 |
| `ascend-ci.com/npu-resource-domain` | `"huawei.com"` | **必填**。资源域名。 |
| `ascend-ci.com/npu-resource-model` | `"ascend-310"` | **必填**。NPU 型号。 |

## 3. 调度器配置
在 `KubeSchedulerConfiguration` 中启用插件：

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: ascend-scheduler
    plugins:
      preFilter:
        enabled:
          - name: "ARCSync"
```

## 4. 预期效果
- 当 NPU 资源耗尽时，Runner Pod 会停留在 `Pending` 状态。
- GitHub Actions 会显示 "Waiting for a runner..."。
- **GitHub Insight 将正确统计这段时间为 Queue Time**。

## 5. Liqo 虚拟节点集成

当集群中存在 Liqo 虚拟节点（带 `liqo.io/remote-cluster-id` 标签的 Node）且 Pod 所在 namespace 配置了 `NamespaceOffloading` CR 时，ARCSync 会执行本地与虚拟节点之间的 NPU 资源比对：

1. **本地总剩余** = 所有本地非 cordoned 节点的空闲 NPU 之和
2. **虚拟节点剩余** = `Allocatable[NPU]` - 该虚拟节点上 runner pod 的 `required-npu-count` 标签累加值（远程 workflow pod 不可见，用 runner pod 声明量估算）
3. 取两者中剩余更多的一方进行调度，另一方被 Filter 拒绝
4. 平局时本地优先

### NamespaceOffloading 配置

在需要使用虚拟节点的 namespace 中创建 `NamespaceOffloading` CR（`offloading.liqo.io/v1beta1`），通过 `spec.clusterSelector` 指定可调度的虚拟节点：

```yaml
apiVersion: offloading.liqo.io/v1beta1
kind: NamespaceOffloading
metadata:
  name: default
  namespace: your-namespace
spec:
  clusterSelector:
    matchLabels:
      liqo.io/remote-cluster-id: "target-cluster"
```

## 6. Volcano Queue 集成

当 Pod 所在 namespace 通过 annotation `scheduling.volcano.sh/queue-name` 关联了 Volcano Queue 时，本地资源总量取 Queue 限额与实际总卡数的最小值：

- `本地资源总量 = min(Queue.spec.capability[NPU], Σ 本地节点 Allocatable[NPU])`
- `本地剩余 = 本地资源总量 - 本地已占用`

```yaml
apiVersion: scheduling.volcano.sh/v1beta1
kind: Queue
metadata:
  name: my-queue
spec:
  capability:
    huawei.com/ascend-310: "8"
```

Namespace annotation：

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: your-namespace
  annotations:
    scheduling.volcano.sh/queue-name: "my-queue"
```

## 7. 调度顺序：FIFO + 回填 (Backfill)

同一调度池（相同 namespace、资源型号、目标集群/nodeSelector）内的 runner pod 按创建时间排序，最老的 pod 为**队首 (pool head)**，享有优先调度权。为避免队首因碎片化阻塞时小任务空等（head-of-line blocking），较新的 pod 允许**回填**——前提是可证明不会推迟队首：

1. **总量保护**：回填 pod 调度后，剩余空闲 NPU 总量必须仍 ≥ 队首需求量；否则回填 pod 保持 `Pending`，空闲卡为队首积累。
2. **排水节点保护**：当队首因碎片化无法在单节点放下时，调度器固定选择一个"排水节点"（可容纳队首需求且当前空闲最多的节点），不再向其调度回填 pod，等待其上任务结束、空闲累积到队首需求。其余节点的碎片对回填 pod 开放。
3. 排水节点选择具有粘性：仅在队首变化或该节点不再可用时重新选择，避免多节点同时排水导致谁都排不空。

仅保护队首（EASY backfill 语义）：第 3 个任务可能短暂插到第 2 个之前，但每个任务成为队首后即获得完全保护，大任务最多等待一轮排水。若队首需求超过所有节点的可分配量（永远无法满足），则不阻塞队列，需人工干预。

打分策略为 **best-fit**：小任务优先装入剩余卡少的节点，保留大块连续空闲给大任务，从源头减少碎片化。
