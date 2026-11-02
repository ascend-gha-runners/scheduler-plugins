# ARC NPU 调度子系统详细设计说明

## 1 设计目标与范围

### 1.1 业务目标

**BG-01 NPU 感知准入。** 在 ARC Container Mode 的 Runner Pod 绑定前，根据后续 Workflow 的 NPU 需求、授权、执行拓扑和资源状况判断可行性，减少 Runner 已启动而 Workflow 因 NPU 资源不可用长期等待的情况。

**BG-02 公平等待与资源效能。** 在同一资源竞争范围内，为 PR、Nightly 等任务提供可解释的等待顺序、分时优先级、资源预留和安全 Backfill；不通过抢占终止运行中的任务。

两个目标并列。准入负责判定“是否有可执行资源”，公平策略负责判定“谁先获得可用资源”。两者不得通过一个聚合容量判断或一个 FIFO 谓词相互替代。

### 1.2 子系统职责与外部边界

| 范围 | 子系统职责 | 外部系统职责 |
|---|---|---|
| NPU 需求 | 解释、校验 Runner 所代表的 Workflow 资源规格；标记需求来源和授权范围 | ARC 提供 Runner 与 Job 的关联；可信平台配置提供可声明的规格 |
| 候选资源 | 依据资源事实、授权和最终执行约束构造可行候选域 | Kubernetes 提供 Node/Pod 资源事实；Liqo 暴露远端资源和 Offloading 约束 |
| 公平调度 | 对可比较任务执行排序、优先级、保护性预留和 Backfill 裁决 | GitHub Actions/ARC Listener 负责 Job 分发，不由本子系统控制 |
| 逻辑预约 | 记录准入承诺、处理竞争和交接；表达资源保证级别 | Workflow Scheduler 执行真正的 Pod 放置和绑定；设备插件管理设备资源 |
| 配额与隔离 | 将资源域和租户授权约束纳入决策，拒绝未经授权的候选 | Volcano 负责其 Queue 管理与实际工作负载调度 |
| 运维质量 | 定义一致性、故障恢复、诊断事件和质量属性契约 | Kubernetes 控制面与平台观测设施提供基础运行能力 |

**边界约束：** Runner 被绑定，不等于 Workflow 已得到 NPU；子系统不能控制 ARC Listener 接单或 GitHub UI 的 Queued 状态；也不能仅凭本地逻辑预约宣称另一个 Scheduler 已锁定设备。此文档不涉及沙箱设计。

### 1.3 架构约束

1. 相同需求在相同有效资源事实、策略版本和预约状态下，应得到相同的可行性结论。
2. 未获授权、需求格式非法或关键资源事实不可信时，不得把资源判定为可用。
3. 跨节点、跨集群、跨型号的容量不得直接求和替代完整可执行性判断。
4. Runtime Node/Pod 观测、逻辑预约和设备实际分配是不同事实，须明确权威来源。
5. 任务公平顺序、跨租户竞争与跨拓扑竞争须有明确作用域。
6. Scheduler 扩展点只承担其语义允许的职责，不把一次调度周期内的通知等价为分布式硬锁。
7. 所有放行、等待、拒绝和失效选择均具有原因码、证据版本及责任组件。

## 2 需求基线

### 2.1 功能需求

| IR | 需求 | 对应场景 | BG |
|---|---|---|---|
| IR-01 | 可信需求表达与 NPU 可执行性准入 | SR-01 需求校验；SR-02 资源核算；SR-03 可行性判断 | BG-01 |
| IR-02 | 逻辑预约及 Runner/Workflow 生命周期交接 | SR-04 预约和回收；SR-05 跨阶段关联与对账 | BG-01 |
| IR-03 | 本地/远端资源域授权、配额和候选选择 | SR-06 Liqo 候选；SR-07 资源域选择；SR-08 Queue 约束；SR-09 Filter/Score 一致性 | BG-01 |
| IR-04 | FIFO 公平、分时优先级、Reserve Node 和安全 Backfill | SR-10 竞争任务调度 | BG-02 |
| IR-05 | 事件、数据新鲜度、故障响应和安全诊断 | SR-11 状态变化与重评估；SR-12 运行支撑 | BG-01 / BG-02 |

### 2.2 DFX 质量属性

| QA | 质量属性 | 必须回答的设计问题 |
|---|---|---|
| QA-01 | 正确性 | 容量、拓扑、授权与放置约束能否得到一致判定？ |
| QA-02 | 公平性 | 大任务不会因为持续插队而饿死；Backfill 是否破坏预留承诺？ |
| QA-03 | 性能与容量 | 随 Node、Pod、排队任务、资源域规模变化，决策时延和吞吐如何受控？ |
| QA-04 | 可靠性 | 并发、重复事件、预约失效和资源事实不一致如何处理？ |
| QA-05 | 可用性 | 控制面或外部依赖不可用时如何安全降级、恢复和重新调度？ |
| QA-06 | 安全性 | 不可信租户能否通过伪造需求、队列或资源域标签获取额外 NPU？ |
| QA-07 | 可服务性 | 能否解释每个 Runner 等待原因，并将其关联到后续 Workflow？ |

质量属性需以场景、架构控制和可观察结果组成完整契约。性能阈值和故障恢复边界由容量和运行质量设计统一确定，不能以缺少测量数据代替设计要求。

## 3 总体架构

### 3.1 架构原则

| 原则 | 设计要求 | QA |
|---|---|---|
| AP-01 复用平台调度机制 | 基于 Kubernetes Scheduling Framework 扩展，不复制通用 Pod 绑定和设备分配功能 | QA-03/04 |
| AP-02 可行性先于评分 | 先用最终执行约束形成候选，再做公平和评分；评分不得扩展候选域 | QA-01 |
| AP-03 事实与预测分离 | 区分资源事实、预测性准入、逻辑预约和真实 Workflow 分配 | QA-01/04 |
| AP-04 授权先于候选 | 资源类型、租户、Queue、Offloading 和需求来源须经可信规则校验 | QA-06 |
| AP-05 单一状态权威 | 预约应有持久、版本化、可对账的权威数据；本地缓存仅是派生视图 | QA-01/04/05 |
| AP-06 公平与利用率兼顾 | FIFO、分时优先级和安全 Backfill 统一仲裁，不抢占运行任务 | QA-02/03 |
| AP-07 明确失效语义 | 依赖异常按授权、安全性和一致性影响选择等待、拒绝或有限降级 | QA-04/05/06 |
| AP-08 可诊断的决策 | 决策具有关联 ID、策略版本、数据版本和原因码 | QA-07 |

### 3.2 逻辑架构

~~~mermaid
flowchart TB
  subgraph EXT["外部系统及资源事实"]
    ARC["ARC Controller / Listener"]
    API[("Kubernetes API")]
    LIQO["Liqo：远端资源与授权"]
    VQ["Volcano：Queue 与 Workflow 调度"]
    DEVICE["Device Plugin：设备资源"]
    WF["Workflow Scheduler：最终绑定"]
  end
  subgraph S["NPU 调度子系统"]
    DEM["需求校验"]
    SNAP["资源事实视图"]
    FEAS["资源域可行性判定"]
    FAIR["公平/优先级/Backfill 仲裁"]
    RES["逻辑预约与交接"]
    DIAG["诊断、事件与一致性监视"]
  end
  ARC -->|"Runner/需求"| API
  DEVICE --> API
  LIQO --> API
  VQ --> API
  API --> DEM
  API --> SNAP
  DEM --> FEAS
  SNAP --> FEAS
  FEAS --> FAIR --> RES
  RES <-->|"版本化预约"| API
  RES -.->|"仅在共享契约下协调"| WF
  WF --> API
  DEM --> DIAG
  FEAS --> DIAG
  FAIR --> DIAG
  RES --> DIAG
~~~

该图划分**逻辑责任**，不要求一个方框对应一个进程、Pod 或 API 对象。资源事实流与决策流分离；功能模块不得绕过授权、预约仲裁或最终 Workflow 资源约束。

### 3.3 组件职责及数据权威

| 组件 | 决策职责 | 权威输入与输出 | 不承担的保证 |
|---|---|---|---|
| 需求校验 | 确认 Runner 的身份、需求、资源型号、数量及授权 | 可信 ARC 规格、Pod UID/Namespace、租户与策略 | 不猜测 Workflow 未声明的 NPU 需求 |
| 资源事实视图 | 提供按节点、资源类型、资源域和版本组织的容量与占用视图 | Kubernetes Node/Pod、Liqo 可见范围、预约账本 | 不把资源 Requests 当作 AI Core 实际利用率 |
| 可行性判定 | 为未来 Workflow 计算能满足硬约束的候选执行计划 | 需求、授权候选域、剩余量、拓扑和可信数据版本 | 不通过跨域余量求和宣称 Workflow 一定可运行 |
| 公平仲裁 | 维护可比较任务排序及预留保护，决定是否轮到当前任务 | 租户/队列策略、任务到达顺序、保留承诺 | 不控制 GitHub Job 接单与跨系统抢占 |
| 预约与交接 | 对逻辑预约进行有版本约束的竞争仲裁、释放及对账 | 持久化预约记录、Pod UID、Workflow 分配事实 | 不在下游未协同时提供设备硬锁 |
| Workflow Scheduler | 最终 Workflow Pod 放置及设备分配 | 真实 Workflow Pod、Node 和设备分配 | 不由 Runner 成功绑定所替代 |
| 诊断与事件适配 | 描述等待、拒绝、重试和交接的原因及证据 | 各决策组件的状态变化 | 不另行形成一套容量权威数据 |

### 3.4 规范化领域对象

| 对象 | 必需字段与关系 | 语义 |
|---|---|---|
| Demand | DemandID、Runner UID、namespace、租户、resourceName、count、WorkflowShape、policyVersion | DemandID 唯一标识一项未来 NPU 需求 |
| WorkflowShape | 单节点或多节点形态、每节点卡数、节点/拓扑约束、集群可达性 | 决定资源是否可被组合，而非只记录总卡数 |
| ResourceDomain | domainID、clusterID、model、授权范围、物理节点集合或可靠映射、数据版本 | 只在此域具备可信视图时参与候选 |
| CapacityView | allocatable、committed、eligibleFree、snapshotVersion、freshness | 可供准入的调度容量，不等同实时设备利用率 |
| CandidatePlan | demandID、domainID、节点/拓扑绑定集合、可行性证据及版本 | 完整的未来 Workflow 放置方案 |
| FairnessOrder | poolID、tenant、jobClass、arrivalOrder、priorityEpoch、reserveCommitment | 明确比较范围与分时策略 |
| LogicalReservation | demandID、domainID、resources、owner、version、state、expiry、handoffID | 逻辑承诺与生命周期权威 |
| AllocationEvidence | Workflow UID、实际 Requests、Node/拓扑、phase、关联 DemandID | 与预约核算的最终占用事实 |
| DecisionEvidence | decisionID、reasonCode、policyVersion、snapshotVersion、guaranteeLevel | 解释准入、等待和拒绝原因 |

### 3.5 关键架构决策

| ADR | 目标决策与理由 | 取舍 |
|---|---|---|
| ADR-01 调度框架复用 | 复用 Scheduling Framework 的调度与绑定边界 | 必须遵守扩展点生命周期与单队列排序规则 |
| ADR-02 可信需求 | 需求由可信模板和准入授权约束，并在调度周期规范化 | 增加授权策略维护；禁止任意 Pod 元数据自行授予资源 |
| ADR-03 最终执行可行性 | 以 WorkflowShape 在一个被授权且连贯的执行范围内存在有效 CandidatePlan 为准入依据 | 比简单求和保守，但排除资源碎片造成的错误放行 |
| ADR-04 资源事实各归其主 | Node/设备、远端可见性、Queue 以及 Workflow 绑定事实分别由对应组件提供 | 需要约定数据新鲜度和同一资源不可重复计数 |
| ADR-05 持久化逻辑预约 | 使用受版本约束且可恢复的预约权威，禁止以进程内 map 作为全局账本 | 增加仲裁读写成本；具体状态转换在 IR-02 定义 |
| ADR-06 非抢占公平调度 | 稳定 FIFO 比较域 + PR/Nightly 分时优先级 + Reserve Node + 安全 Backfill | 在资源碎片场景中可能牺牲局部吞吐以兑现等待承诺 |
| ADR-07 跨调度器明确保证级别 | 预测性准入与有下游协调的预约保证是两种不同契约 | 无协调时不能对外承诺设备已锁定 |
| ADR-08 安全降级 | 授权和核心一致性事实不可信时不得放行；可观测性故障不影响调度判定 | 关键依赖故障会降低可用性，但防止资源越权 |
| ADR-09 单一有效预约仲裁 | 同一资源竞争域内的状态更新需要版本与代际保护 | 避免多实例同时承诺同一容量；需要恢复和对账路径 |
| ADR-10 最小权限及租户隔离 | 用户身份、Queue 权限和远端授权分开验证；控制面权限受最小权限约束 | 管理复杂度换取明确的信任边界 |
| ADR-11 原因链诊断 | 需求、候选、等待、预约、Workflow 交接使用同一关联链 | 需要控制高基数指标与事件保留范围 |

## 4 NPU 需求与容量准入设计（IR-01、IR-03）

### 4.1 准入的目标语义

准入是对**未来 Workflow 的可执行性**进行判断，而不是单纯判断 Runner Pod 能否绑定。Runner 可能只需要 CPU，不能因此将 Runner 的 CPU NodeSelector 误用作 Workflow 的 NPU 放置约束；两种工作负载分别具有自己的约束和调度责任。

一个有效需求由以下内容组成：

- resourceName：规范化扩展资源名及型号，不同型号不得互相抵偿。
- count：正整数卡数，必须属于受信任的授权规格。
- WorkflowShape：单节点或明确的多节点形态，每节点需求、拓扑亲和/反亲和及设备/网络条件。
- executionScope：允许的本地或远端集群、资源池与授权限制。
- tenant/queue：用于确定资源归属及配额语义。
- identity：Runner UID、namespace、Job attempt 关联及策略版本，禁止仅凭名称推导身份。

**输入非法（INVALID_DEMAND）**、**未授权（UNAUTHORIZED）**、**无可行拓扑（INFEASIBLE）**、**暂时无余量（INSUFFICIENT_CAPACITY）**、**资源事实不可信（DATA_UNAVAILABLE）**是不同结果。

### 4.2 资源事实与候选域

**资源事实四层：**

1. **物理容量**：Node 对指定资源名公布的 Allocatable。它是 Kubernetes 的可调度资源上限，不是实时 AI Core 使用率。
2. **已分配量**：按照 Kubernetes 资源请求与对应调度器已认可的分配事实计量。必须遵循 Kubernetes 对普通容器、InitContainer 等有效 Requests 的语义。
3. **未兑现预约**：尚未被真实 Workflow 分配计量接管的逻辑承诺，按相同资源域扣减；已交接部分不得重复扣减。
4. **可用量**：物理容量减已分配量和有效的未交接预约，并按 Node/Zone/Queue/授权/拓扑等约束收缩，负余量按不可用处理，不借其他节点正余量掩盖。

按节点和资源型号定义：

~~~text
Free(n, r, v) = max(0, Allocatable(n,r,v)
                       - Allocated(n,r,v)
                       - ActiveUnhandedReservation(n,r,v))
~~~

其中 v 表示经过一致性校验的事实版本。公式**仅用于同一事实域内的资源账面估算**；当请求可能受其他独立 Scheduler 消耗且其分配不可见时，不能据此声明已获得强保证。

**资源域判定原则：**

- 本地资源域由可见物理节点构成；每个节点独立检查型号和放置约束。
- Liqo 远端资源域必须先验证 NamespaceOffloading 授权，再确认远端映射与最终 Workflow 放置语义。一个聚合虚拟节点不应自动被视为一台具有同等卡数的真实物理节点。
- 不同 Cluster/ResourceDomain 的空闲卡数不能用于满足同一单节点需求；多节点形态只有在明确支持跨节点协同、拓扑和组调度语义时才允许组合。
- 同一物理卡通过不同视图被映射多次时，应按可追踪的物理资源身份去重。无法确认去重则不得把重复映射累加。
- 外部 Queue 的限制应按其**实际适用的作用域、额度与已占用口径**参与判定；读取 Queue 对象不能自行证明跨调度器已执行强配额。

### 4.3 形式化可执行性条件

设需求 D 的规格为 r、WorkflowShape、授权范围 A(D)，资源事实快照为 S，预约版本为 R。定义候选执行计划 P 为节点及资源分配的集合，执行域为 domain(P)。

~~~text
Feasible(D,S,R) ⇔
    ∃ P:
      Authorized(P,D)
      ∧ SameCoherentExecutionScope(P,D)
      ∧ ResourceModelMatch(P,D)
      ∧ CapacityFit(P,D,S,R)
      ∧ PlacementAndTopologyFit(P,D)
      ∧ QueueQuotaFit(P,D,S,R)
      ∧ WorkflowSchedulerSupports(P,D)
      ∧ FactsFreshAndConsistent(P,S,R)
~~~

**SameCoherentExecutionScope** 并非总要求单个 Node：对于单节点 Workflow，P 必须落在一个满足全部条件的物理节点；对于明确的多节点 Workflow，P 可包含多个节点，但必须满足声明的组调度、网络拓扑和每节点需求，且不能从无协同关系的集群中拼接卡数。

**CapacityFit** 比较该计划中每个节点实际可分配的对应型号卡数，而非只比较集群总和。**QueueQuotaFit** 与 **Authorized** 同时约束本地及远端候选；**FactsFreshAndConsistent** 要求计划中引用的数据具有可核验版本及有效性。

得到有效 P 只是资源可行性结论，不等于公平仲裁已经准许该任务，也不等于另一个 Scheduler 必定在未来兑现分配。

### 4.4 决策顺序与算法

~~~mermaid
flowchart TD
  A["Runner 代表的 Workflow 需求"] --> B{"需求合法且授权？"}
  B -- 否 --> E1["拒绝：INVALID_DEMAND / UNAUTHORIZED"]
  B -- 是 --> C["构造本地/远端授权执行域"]
  C --> D{"关键资源事实有效？"}
  D -- 否 --> E2["等待重评估：DATA_UNAVAILABLE"]
  D -- 是 --> F["按型号、拓扑、Node 约束生成 CandidatePlan"]
  F --> G{"存在可执行计划？"}
  G -- 否 --> H{"规格在允许域内理论可执行？"}
  H -- 否 --> E3["拒绝：INFEASIBLE"]
  H -- 是 --> E4["等待资源：INSUFFICIENT_CAPACITY"]
  G -- 是 --> I["交给公平策略仲裁"]
  I --> J{"此需求允许获得资源？"}
  J -- 否 --> E5["等待：FAIRNESS_WAIT"]
  J -- 是 --> K["有版本约束的逻辑预约"]
  K --> L{"预约成功？"}
  L -- 否 --> E6["竞争冲突：RETRY_WITH_FRESH_FACTS"]
  L -- 是 --> M["接受 Runner 准入，并声明保证级别"]
~~~

算法步骤：

1. 校验可信 DemandID、resourceName、正整数 count、WorkflowShape、租户与 Queue 授权；非法输入直接拒绝。
2. 从授权策略与 Liqo Offloading 限制构造执行域，剔除不可达、不可用和未授权域；不采用“无选择器即允许所有远端”的隐式默认值。
3. 获取按资源域/节点/型号组织的 CapacityView，检查版本、新鲜度以及预约账本可用性；不可验证的域暂时不参与候选。
4. 在每个执行域内部求完整 CandidatePlan。对单节点需求检查存在满足全部条件的单节点；多节点需求按明确的每节点规格与拓扑求可行映射。
5. 如果无计划，区分**理论配置永远不可能满足的规格**与**资源暂时被占用**，返回不同原因，并为可能恢复的请求定义重评估触发条件。
6. 如果存在候选计划，将候选和证据交给公平仲裁；公平策略仅在可行计划集合内选择，不得扩大范围。
7. 获得公平许可后，以版本和唯一 DemandID 进行逻辑预约；竞争冲突必须获取新资源事实后重新判定，不按旧快照重复放行。
8. 输出准入结果、所选执行域、关联 ID、事实版本、策略版本和保证级别；后续 Workflow 真实分配及预约交接遵循 IR-02 约束。

**候选一致性不变量：**

~~~text
AdmittedCandidate ⊆ AuthorizedCandidate ∩ FeasibleCandidate
FilterCandidate   ⊆ FeasibleCandidate
ScoreDomain       ⊆ FilterCandidate
~~~

这组关系约束最终放置相关的判断。Runner 自身绑定节点不必等于未来 Workflow 的 NPU 执行节点，因此不能将两者的 NodeSelector 合并为同一个约束集。

### 4.5 本地/远端选择

| 情况 | 决策约束 |
|---|---|
| 本地和远端都满足完整 WorkflowShape | 按公平策略、授权、策略性偏好和资源效能选取；优先偏好不能改变可行性 |
| 本地总余量大，但所有单节点都不足 | 对单节点需求不选本地；只有显式可执行的多节点形态可以组合 |
| 远端虚拟节点空闲量为聚合视图 | 必须获得对应成员真实放置条件，或者按可明确表达的非保证级别处理；不能把汇总卡数当作一台机器 |
| 本地无授权，远端充足 | 不因容量充足绕过授权 |
| 远端授权、映射或可达性异常 | 仅在本地独立满足所有要求时选本地；不得把未知远端视为可用 |
| 任何候选配额不足 | 仅保留配额满足的候选，配额必须以相同租户/Queue 口径应用 |

### 4.6 保证级别与跨 Scheduler 协作边界

| 级别 | 必要条件 | 可对外说明 | 不能承诺 |
|---|---|---|---|
| **ESTIMATED** 预测性准入 | 存在基于可信快照的完整 CandidatePlan，且策略允许机会性准入 | 在快照时刻该 Workflow 具备可行资源计划 | NPU 已锁定、Workflow 必然立即运行、不会与其他 Scheduler 竞争 |
| **COORDINATED** 受协调准入 | 除 ESTIMATED 条件外，下游 Scheduler/分配端确认同一资源域的预约、版本、交接和撤销语义 | 在协议明示的范围和有效期内保留协调承诺 | 跨协议边界或超出有效期后的绝对资源保证 |
| **WAIT / REJECT** | 关键事实缺失、权限不通过、不可执行或无剩余量 | 解释等待/拒绝的原因与重评估条件 | 用“已通过 Runner Filter”替代资源保证 |

逻辑预约不会自动升级 ESTIMATED 为 COORDINATED；若 Workflow Scheduler 不参与预约协议，只能保持预测性声明。**只有同一份资源占用与预约仲裁契约被所有相关分配参与者遵守时，才可要求防止跨调度器超卖。**

### 4.7 边界情景与判定

| 情景 | 正确判定 | 关键依据 |
|---|---|---|
| 两节点各空闲 4 卡，单节点 Workflow 要 8 卡 | INFEASIBLE 或等待具备 8 卡单节点的授权域 | 4+4 不能作为单节点 8 卡 |
| 两节点各空闲 4 卡，明确的 2×4 多节点任务 | 仅在拓扑、组调度和每节点约束均支持时可行 | 多节点是独立的 WorkflowShape |
| 本地 8 卡、远端 10 卡，单节点需求 12 卡 | 不准入 | 8+10 不属于一个可执行节点 |
| Runner 指向 CPU Node、Workflow 指向 NPU Node | 分别检查两个工作负载的约束 | CPU Runner NodeSelector 不决定 NPU Workflow 放置 |
| 匹配型号有 8 卡，其他型号还有 8 卡，请求某型号 12 卡 | 不准入 | 不同扩展资源名不可相加 |
| 一个节点 8 卡，真实已分配 2 卡，未交接预约 3 卡，请求 4 卡 | 不可立即准入 | 可用量仅 3 卡 |
| 同一物理 8 卡在本地视图和虚拟视图各出现一次 | 至多按 8 卡计入 | 必须去重，禁止镜像重复计量 |
| Liqo 不允许该 namespace 使用远端 | 远端不参与候选 | 授权优先于容量 |
| Queue 上限允许 2 卡，请求 4 卡 | 不得以充足物理容量绕过配额 | Queue 适用范围与真实已占用口径 |
| Informer/预约账本数据过期或不可确认 | WAIT DATA_UNAVAILABLE；仅独立可信域可继续 | 不能从未知事实推导容量 |
| 两次并发读取同一余量 | 预约须以版本仲裁，失败者重试 | 快照判断不是并发承诺 |
| 下游使用独立 Scheduler 且未共享预约 | ESTIMATED，不得声明 COORDINATED | 明确跨 Scheduler 竞争风险 |

### 4.8 正确性与异常契约

**不变量 INV-C01：** 一个通过准入的需求至少有一个同时满足授权、型号、容量、放置、拓扑及配额约束的 CandidatePlan；允许明确的预测误差，但不能把不满足硬约束的组合称为可行。

**INV-C02：** 同一决策依据的资源事实和预约状态具有可核验版本；并发冲突不能沿用已失效的可行性结果。

**INV-C03：** 计数不会因为相同 Pod 的物理占用、逻辑预约以及 Liqo 镜像视图而重复扣减，也不能因 Workflow 尚不可见而无依据地提前释放。

**INV-C04：** 未授权或未知状态不能被“容量足够”“评分较高”或“远端可见”等非授权信号覆盖。

**INV-C05：** Score 只对有效候选排序。不存在可行候选时，调度不会通过修改 Score 创造容量。

异常类型包括非法需求、权限不满足、资源永久不兼容、暂时资源不足、数据过期、竞态版本冲突和下游协调失败。可恢复异常必须声明重评估事件；安全性相关错误不得静默放行。

## 5 功能单元设计

### 5.1 IR-01：需求校验、资源计量和准入

| 场景 | 输入与判定 | 输出与约束 |
|---|---|---|
| SR-01 需求声明 | Runner 的可信来源、型号、数量、WorkflowShape、tenant/queue、唯一身份 | 形成规范化 Demand；不合法或越权则拒绝 |
| SR-02 容量核算 | Node/Pod 有效 Requests、逻辑预约、物理资源映射和快照版本 | 形成按执行域/Node/型号去重的 CapacityView |
| SR-03 可行性准入 | CandidatePlan、完整放置/拓扑条件、配额、数据新鲜度 | 按第 4 章决策状态返回，并不把估算等同协调承诺 |

**交互时序：**

~~~mermaid
sequenceDiagram
  participant A as ARC
  participant D as Demand Validator
  participant C as Candidate Planner
  participant Q as Authorization/Quota
  participant F as Fairness
  participant R as Reservation
  A->>D: Runner UID 与未来 Workflow 需求
  D->>Q: 验证来源、租户和可用域
  Q-->>D: 授权/策略版本
  D->>C: 规范化需求
  C->>C: 形成可行执行计划
  alt 无法形成计划
    C-->>A: INFEASIBLE / WAIT / DATA_UNAVAILABLE
  else 计划有效
    C->>F: 候选及证据版本
    F->>R: 公平准许后的候选
    R-->>A: 准入结果、保证级别、关联 ID
  end
~~~

**接口契约：** 输入的资源数量和 WorkflowShape 不允许靠默认值掩盖格式错误；输出包含原因码、计划证据及版本，不暴露跨租户敏感资源详情。

### 5.2 IR-02：逻辑预约与资源交接

**SR-04 预约。** 预约以 DemandID 唯一标识，包含资源域、数量、有效期、所有者、版本和状态。申请、续约、撤销须幂等且具有竞争仲裁；绑定失败和资源释放不得使预约无限留存。实例切换应从权威状态恢复，不因本地缓存为空而重复放行。

**SR-05 交接。** Runner 与 Workflow 必须通过带 namespace、UID、Job attempt 的显式关联进行匹配。Workflow 实际分配被确认后，逻辑预约对应的额度由“未交接”转成“已分配”；确认前不得仅凭名称或定时推测完成交接。取消、Workflow 终止和孤儿预约应由对账机制处理。

**接口边界：** 预约权威只负责逻辑承诺；真实设备占用由实际 Workflow Scheduler 和 Kubernetes 分配事实决定。状态迁移、租约期限、版本冲突、双重分配与恢复的精细契约在本功能单元中必须满足 AP-05 与 INV-C02/C03。

### 5.3 IR-03：资源域、配额和候选节点

**SR-06 Liqo 候选。** 根据明确的 NamespaceOffloading 授权和远端资源映射确定可见执行域。远端不可达或映射不可信时不能仅依据虚拟节点数量放行。

**SR-07 本地/远端选择。** 使用相同的 WorkflowShape、型号、每节点可分配资源和配额口径评价两种执行域；仅在均可行后根据策略偏好选择。不得用“本地空闲和”与“远端单节点最大值”作不同语义的可行性比较。

**SR-08 Queue 约束。** 按任务实际归属 Queue、租户及资源域作用范围校验容量与授权；外部 Queue 没有提供可确定配额语义时不能假设无限额度。

**SR-09 Filter 与 Score。** 节点 Filter 仅处理经可行性模块确认的候选；Score 只能在通过约束的候选内优化资源利用率或拓扑偏好。Runner 与 Workflow 分离时，对两种工作负载采用各自正确的放置语义。

### 5.4 IR-04：公平调度

**SR-10 竞争任务调度。** 公平性在具有可比较资源域、拓扑需求和租户授权的队列中定义。每个任务有稳定的到达顺序、任务类别和策略版本。PR 延迟敏感与 Nightly 吞吐敏感任务支持明确的分时优先级；优先级切换不得无条件抹除既有预留承诺。

Reserve Node 为满足较大需求保留未来可用资源的条件。Backfill 仅在证明不会推迟受保护任务的已承诺可执行时间时允许；没有足够的运行时间边界或可回收保证时，不能假设短任务能够安全回填。全程不主动抢占正在运行的 CI 任务。

公平调度不重新定义 NPU 物理可行性，必须使用第 4 章的 CandidatePlan；跨租户和跨拓扑域的公平权重、保护条件以及活性边界作为统一的公平策略契约定义。

### 5.5 IR-05：事件、状态一致性与诊断

**SR-11 事件与重评估。** 资源释放、Node 变化、Liqo 授权与可达性变化、Queue 变化、策略版本变动、预约释放和 Workflow 交接均可能使 WAIT 需求重新可行。事件驱动重评估应与定期一致性核对互补，避免仅依赖某一种外部事件。对无变化的拒绝应避免无界反复计算。

**SR-12 运行支撑。** 子系统应有清晰的故障域、单一有效预约写入者或等效并发仲裁、最小 RBAC 权限及故障恢复语义。数据源不可用时区分授权/一致性关键依赖与一般指标采集依赖，避免要么所有失败都放行、要么全部故障都停止服务。

**诊断契约：** 需求 ID、决策原因、候选执行域、数据和策略版本、预约状态、Workflow UID 应可跨组件关联。拒绝原因与资源不足、队首公平等待或数据不可用必须区分。

## 6 DFX 架构设计

### 6.1 正确性与安全性

- 所有可行性决策以第 4 章的 CandidatePlan、不变量和资源权威为基础。
- 未授权的需求、Queue 或远端资源不得进入候选集；不可信 Pod 元数据不得自动升级资源授权。
- 跨租户的公平和资源可用性必须由稳定租户身份划分，而不是允许用户修改标签影响他人的资源池。
- 候选和预约状态出现矛盾时选择可追踪的等待与恢复，不能依靠一次 Score 成功隐藏异常。

### 6.2 性能与容量

性能关键路径包括需求校验、资源视图读取、候选拓扑求解、公平队列裁决、预约仲裁和返回结果。规模参数包括节点数 N、Pod 数 P、有效预约数 R、待调度需求数 Q、执行域数 C 和拓扑约束复杂度 K。

需分别对**一次调度决策时延**、**单位时间可完成的准入次数**、**策略变化后的重评估时延**和**预约读写竞争**建立容量边界。不得把 N+P+R+Q 的简单扫描模型直接等同实际耗时；也不应要求在每次调度周期无条件全量扫描全部 Pod。

**资源效能约束：** 允许在不违反授权、公平和正确性不变量的前提下优化 NPU 占用效率；不能用平均吞吐改善掩盖大任务的无限等待。

### 6.3 可靠性与可用性

- 预约状态为逻辑单一权威，写入具有版本仲裁和恢复路径。
- 对节点失联、API 暂时不可用、跨集群远端失联、事件丢失、调度实例切换和 Workflow 异常终止分别定义控制边界。
- 已经接受的预约不能在主备切换后被无依据地重复承诺；过期、孤儿与已交接状态应可对账。
- 对无法验证授权或关键容量事实的候选禁止放行；可独立验证的其他执行域可按策略继续服务。
- 资源变化触发重评估，但应防止故障恢复阶段的重复事件造成重试风暴。

### 6.4 可服务性

最小诊断关联链：DemandID → DecisionID → CandidatePlan → FairnessReason → ReservationID → Runner UID → Workflow UID/Node。

建议的指标族为：按原因分类的等待需求数、可行性拒绝数、预约冲突数、交接超时数、孤儿预约数、资源事实新鲜度、调度决策耗时、策略变更重评估时延和公平队首等待时间。指标只描述调度问题，不替代设备计算利用率监控。

### 6.5 关键失效模式与架构响应

| 失效模式 | 必须采用的控制 | 必须可观察的信号 | 仍需约束的风险 |
|---|---|---|---|
| 资源片段总和满足、单节点不足 | WorkflowShape 和 CandidatePlan 判定 | INFEASIBLE 与无可行 Node 的证据 | 未来节点状态变动使估算失效 |
| Liqo 映射重复或远端失联 | 资源身份去重、授权域及新鲜度检查 | domainID、映射版本、失联原因 | ESTIMATED 级别不能保证远端真实绑定 |
| 并发预约和 Scheduler 切换 | 版本化预约仲裁、恢复对账 | 竞争冲突、代际/版本、孤儿数量 | 下游 Scheduler 不遵守协议时的跨域竞争 |
| 大任务队首等待且小任务持续到来 | 保护性预留与安全 Backfill | 队首年龄、预留边界、回填判断原因 | 资源永久不足时无有限等待时间承诺 |
| Queue 配额缺失或被伪造 | 授权校验、配额数据可信性要求 | UNAUTHORIZED、DATA_UNAVAILABLE | 外部 Queue 不提供统一配额语义时无法强承诺 |
| Workflow 身份误匹配 | 唯一 DemandID + UID 与显式交接 | 未交接预约、孤儿、重复关联 | 外部 Workflow 数据滞后的暂时不确定性 |
| API 或控制面故障 | 状态权威检查、有限域降级与重评估 | 数据版本、不可用域、等待原因 | 关键事实不可验证时调度可用性下降 |

该表是架构风险控制要求。质量属性的量化门槛、故障域边界和完整 FMEA 场景须与相关功能契约保持一致，不以系统是否已经运行作为判断依据。

## 7 需求与设计追踪

| 需求 | 主要设计章节 | 架构决策 | 质量属性 | 验收判据 |
|---|---|---|---|---|
| SR-01 需求校验 | 4.1、5.1 | ADR-02、ADR-10 | QA-01、QA-06 | 非法/越权需求不进入候选 |
| SR-02 资源计量 | 4.2、5.1 | ADR-04、ADR-05 | QA-01、QA-04 | 资源型号和映射去重，预约不重复核算 |
| SR-03 可执行性 | 4.3～4.8、5.1 | ADR-03、ADR-07 | QA-01 | 单节点 4+4/8 和跨域 8+10/12 反例均不能误放行 |
| SR-04 预约 | 3.4、5.2 | ADR-05、ADR-09 | QA-01、QA-04 | 并发冲突不导致同一逻辑容量双重承诺 |
| SR-05 交接 | 3.4、5.2 | ADR-05、ADR-07 | QA-04、QA-07 | 同名跨 namespace 不会错误抵消预约 |
| SR-06 Liqo 授权 | 4.2、4.5、5.3 | ADR-02、ADR-04 | QA-01、QA-06 | 无远端授权不可能选远端 |
| SR-07 池选择 | 4.3～4.7、5.3 | ADR-03、ADR-04 | QA-01 | 本地/远端使用同一可行性语义 |
| SR-08 Queue 约束 | 4.5、5.3 | ADR-04、ADR-10 | QA-01、QA-06 | 外部配额不能因缺失或错误默认无限 |
| SR-09 Filter/Score | 4.4、5.3 | ADR-01、ADR-03 | QA-01、QA-03 | 评分集合不得扩大可行候选集合 |
| SR-10 公平 | 5.4 | ADR-06 | QA-02、QA-03 | Backfill 不得推迟受保护需求的预留承诺 |
| SR-11 事件 | 5.5、6.3 | ADR-08、ADR-11 | QA-04、QA-05 | 关键事实变化能触发受控重评估 |
| SR-12 运行支撑 | 5.5、6.3～6.4 | ADR-08～11 | QA-05～07 | 控制面故障有明确响应及关联诊断 |

## 8 技术规范参考

- [Kubernetes Scheduling Framework](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/)：Scheduler 扩展点、调度/绑定周期、QueueSort、Reserve/Unreserve。
- [Kubernetes Running Multiple Schedulers](https://kubernetes.io/docs/concepts/scheduling-eviction/running-multiple-schedulers/)：多调度器间的节点和资源竞争边界。
- [Liqo 官方项目](https://github.com/liqotech/liqo)：虚拟节点与 Namespace Offloading 相关协议。
- [Volcano 官方文档](https://volcano.sh/docs/)：Queue、配额与工作负载调度语义。
