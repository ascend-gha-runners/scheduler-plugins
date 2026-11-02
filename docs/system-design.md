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
| FairnessOrder | fairnessPoolID、tenantID、jobClass、arrivalSequence、PolicyEpoch、deficit、protectionVersion | 同域竞争排序及跨租户服务份额；详见 5.4.1～5.4.2 |
| ReserveCommitment | protectedDemandID、fairnessPoolID、candidatePlanID、受保护节点向量、protectionVersion、PolicyEpoch、有效性/释放边界 | 队首软保护，非物理锁或已占用卡数；详见 5.4.3～5.4.4 |
| LogicalReservation | reservationID、DemandID、domainID、nodeResourceVector、owner、generation、writerEpoch、state、leaseDeadline、workflowRef、evidenceVersion | 预约生命周期权威及跨阶段交接；见 5.2 |
| AccountingDomain | domainID、physicalResourceIdentity、nodeCapacityVector、reservationIndex、version、writerEpoch | 同一物理容量的节点向量及预约原子仲裁域；见 5.2 | 
| AllocationEvidence | Workflow UID、实际 Requests、Node/拓扑、分配确认版本、关联 DemandID | 用于预约交接的可信分配证据 |
| DecisionEvidence | decisionID、reasonCode、policyVersion、snapshotVersion、guaranteeLevel | 解释准入、等待和拒绝原因 |

### 3.5 关键架构决策

| ADR | 目标决策与理由 | 取舍 |
|---|---|---|
| ADR-01 调度框架复用 | 复用 Scheduling Framework 的调度与绑定边界 | 必须遵守扩展点生命周期与单队列排序规则 |
| ADR-02 可信需求 | 需求由可信模板和准入授权约束，并在调度周期规范化 | 增加授权策略维护；禁止任意 Pod 元数据自行授予资源 |
| ADR-03 最终执行可行性 | 以 WorkflowShape 在一个被授权且连贯的执行范围内存在有效 CandidatePlan 为准入依据 | 比简单求和保守，但排除资源碎片造成的错误放行 |
| ADR-04 资源事实各归其主 | Node/设备、远端可见性、Queue 以及 Workflow 绑定事实分别由对应组件提供 | 需要约定数据新鲜度和同一资源不可重复计数 |
| ADR-05 持久化逻辑预约 | 使用受版本约束且可恢复的预约权威，禁止以进程内 map 作为全局账本 | 增加仲裁读写成本；具体状态转换在 IR-02 定义 |
| ADR-06 非抢占公平调度 | 同一资源竞争域内采用层次化租户公平、稳定 FIFO、PR/Nightly 分时优先级、Reserve Node 与安全 Backfill；公平版本和预约权威联合仲裁 | 允许空间无冲突回填；借保护容量必须具可信强制运行上限与保护窗口，可能牺牲短期吞吐以兑现承诺 |
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
3. **未兑现预约**：尚未被可信 Workflow 分配证据确认交接的逻辑承诺，按相同资源域扣减；确认前若真实分配也可见可暂时保守双计，确认后必须归并。
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

**INV-C03：** 同一物理容量不得因 Liqo 重复映射而重复计入。Workflow 分配确认前不得提前解除逻辑承诺；未确认交接时允许暂时保守双计，确认后必须归并，禁止持续双计或少计（见 5.2.3、INV-R03）。

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

### 5.2 IR-02：逻辑预约与 Runner/Workflow 资源交接

本功能单元覆盖 **SR-04 逻辑预约与回收**、**SR-05 Runner/Workflow 关联及计量交接**。目标是防止对同一资源重复作出逻辑承诺，并使预约在调度竞争、Runner 绑定失败、Workflow 迟到和 Scheduler 切换时可恢复。**逻辑预约不是设备锁**；只有参与相同仲裁和交接协议的下游 Scheduler 才受其约束（ADR-05、ADR-07、ADR-09）。

#### 5.2.1 预约权威与一致性范围

**单一权威：** 按不重叠的物理资源竞争域和资源型号建立 AccountingDomain；每个域具有持久化权威账本、可比较版本和有效写入者代际（writerEpoch）。同一域的账本包含全部受保护节点的可用资源向量及有效预约。不同虚拟节点若映射同一物理资源，必须受同一个权威仲裁，不得各自独立承诺该容量。

**容量余量与预约记录必须在一次原子竞争检查中共同更新。** 仅为每个 DemandID 创建一条带版本的记录，不能阻止两个需求分别读到相同余量而双重承诺。目标选择**域级条件更新（CAS）**：在同一个版本化 AccountingDomain 对象内，检查整个 CandidatePlan 所涉及的节点向量，再整体扣减并记录预约，或整体失败。单一域内的多节点计划不可部分提交。跨域 Gang 若无法置于同一仲裁域，须定义跨域原子协议；在此之前不声称支持跨域原子预约。

| 权威事实 | 责任组件 | 约束 |
|---|---|---|
| 物理 NPU 资源与实际 Workflow 分配 | Kubernetes Node/Pod、最终 Workflow Scheduler 和设备插件 | 来源于真实分配，不能由本地预约表伪造 |
| AccountingDomain 及逻辑预约 | Reservation Coordinator 管理的持久化 Kubernetes API 状态 | 受保护资源的所有承诺者竞争相同域版本；同域只允许有效代际写入 |
| Runner/Workflow 身份关联 | 可信 ARC 需求身份及 Workflow 唯一 UID | 禁止从 Pod 名称相似性推断同一任务 |
| Scheduler CycleState / Informer | 前述状态的派生视图 | 不承担跨实例权威；缓存失效时不得用旧余量继续强预约 |

**边界：** 域级 CAS 可约束参与协议的逻辑承诺，不能自行阻止**未参与协议**的 Volcano/其他 Scheduler 或设备分配者使用同一物理资源。这类路径只能维持 ESTIMATED，不得宣称 COORDINATED。

#### 5.2.2 领域数据模型

| 对象 | 必需字段 | 不变量 |
|---|---|---|
| ReservationKey | clusterID、namespace、Runner UID、可信 Job/attempt ID、DemandID | 重试身份稳定；不同任务尝试不可复用有效预约 |
| AccountingDomain | domainID、physicalResourceIdentity、resourceName、nodeCapacityVector、reservationIndex、version、writerEpoch、PolicyEpoch、protectionVersion | 同一容量与公平保护共用原子仲裁边界；Node/型号维度独立核算 |
| LogicalReservation | reservationID、DemandID、tenant、Queue、CandidatePlanID、nodeResourceVector、guaranteeLevel、state、generation、writerEpoch、leaseDeadline、owner、workflowRef、evidenceVersion | 同一 Demand/generation 至多一个有效预约；转移须检查代际及合法状态 |
| HandoffEvidence | reservationID、Workflow namespace/UID、实际 Requests、Node/物理资源映射、分配事实版本 | 必须对应受保护资源域；Runner 已绑定、Workflow 名称相同均不足以证明实际占用 |
| ReconcileDecision | reservationID、observedState、evidenceVersion、action、reasonCode | 幂等且可复核；不确定时保留容量，不直接释放 |

预约使用每节点每型号向量 h(d,n,r)，而非只有域内总卡数；这样多节点形态不会被一个聚合数掩盖。单个 Demand 不得同时在本地和远端维护两个有效承诺；跨域迁移须先按可信证据撤销旧承诺，再申请新域，除非另有原子的迁移协议。

#### 5.2.3 预约状态机

| 状态 | 进入条件 | 计量原则 | 允许的后续状态 |
|---|---|---|---|
| **PLANNED** | 可行计划已生成，尚未取得有效预约 | 不扣减 | HELD / WAIT / REJECT |
| **HELD** | 域级原子仲裁成功 | 全额计入未交接预约 | HANDOFF_PENDING / REVOKING |
| **HANDOFF_PENDING** | Runner 绑定或交接意图可见，实际 Workflow 分配尚未可信确认 | 保留预约 | ALLOCATED / REVOKING / UNCERTAIN |
| **ALLOCATED** | 已核对 Workflow UID、Node、请求和真实分配版本 | 相应逻辑扣减转交真实占用计量 | REVOKING / UNCERTAIN / CLOSED |
| **REVOKING** | 取消、绑定失败、租约到期或要求撤销 | 未证实释放的部分继续保留 | CLOSED / UNCERTAIN |
| **UNCERTAIN** | 分配证据冲突、远端失联或恢复时状态未知 | 对应容量保守隔离 | HANDOFF_PENDING / ALLOCATED / REVOKING / CLOSED（均需新证据） |
| **CLOSED** | 已确认未分配，或最终释放得到可信证明 | 不再计入逻辑预约 | 终态；再次申请须新 generation |

PLANNED、WAIT、REJECT 可是调度决策状态，不要求进入持久账本。其他状态是权威逻辑状态。**租约超时不等于资源已释放**：到期触发 REVOKING 与下游核验，证据不完整时进入 UNCERTAIN。

~~~mermaid
stateDiagram-v2
  [*] --> PLANNED
  PLANNED --> HELD: 域级CAS成功
  PLANNED --> [*]: WAIT / REJECT
  HELD --> HANDOFF_PENDING: Runner绑定 / 交接开始
  HELD --> REVOKING: 绑定失败 / 取消 / 到期
  HANDOFF_PENDING --> ALLOCATED: 确认Workflow实际分配
  HANDOFF_PENDING --> REVOKING: 明确撤销
  HANDOFF_PENDING --> UNCERTAIN: 状态或证据不明
  ALLOCATED --> REVOKING: Workflow退出
  ALLOCATED --> UNCERTAIN: 分配状态不可信
  REVOKING --> CLOSED: 确认释放
  REVOKING --> UNCERTAIN: 释放无法确认
  UNCERTAIN --> HANDOFF_PENDING: 恢复交接证据
  UNCERTAIN --> ALLOCATED: 恢复分配证据
  UNCERTAIN --> REVOKING: 恢复撤销证据
  UNCERTAIN --> CLOSED: 证明无占用
  CLOSED --> [*]
~~~

**跨阶段计量：** 相同资源只能归属于可承诺余量、未交接逻辑预约或已确认的真实分配。交接证据未完成核对，但真实 Pod 分配已出现在资源视图时，**允许临时保守双计**；不得为消除双计而先删除预约。完成验证后，在一致状态变更中解除对应逻辑扣减，同时保留真实分配计量。部分多节点 Workflow 的未交接资源向量仍须保留。

#### 5.2.4 并发仲裁与正确性不变量

~~~text
CommittedFree[d,n,r] =
    Allocatable[d,n,r]
  - ConfirmedAllocated[d,n,r]
  - EffectiveLogicalHold[d,n,r]

Commit(D, P, v, epoch, fairnessPermit):
  REQUIRE Authorized(D) AND FeasibleCandidate(P)
  REQUIRE AccountingDomain.version == v
  REQUIRE AccountingDomain.writerEpoch == epoch
  REQUIRE fairnessPermit.DemandID == D.DemandID
  REQUIRE fairnessPermit.PolicyEpoch == AccountingDomain.PolicyEpoch
  REQUIRE fairnessPermit.protectionVersion == AccountingDomain.protectionVersion
  REQUIRE FairnessCommitmentPreserved(P, fairnessPermit)
  REQUIRE all required node/resource coordinates:
          CommittedFree[d,n,r] >= P.request[n,r]
  ATOMIC: record reservation + deduct nodeResourceVector
  OTHERWISE: reject conflict and recompute from fresh facts
~~~

提交契约：

1. 读取 CandidatePlan、可信的分配事实版本与同一 AccountingDomain 账本版本。
2. 按 DemandID/generation 查重。同一请求幂等返回相同 reservationID；身份相同但 tenant、规格或计划冲突时拒绝覆盖。
3. 原子验证授权、PolicyEpoch、公平许可及其 protectionVersion、writerEpoch、每节点容量及完整计划；将节点向量和预约记录在同一个 CAS 中提交。
4. CAS 冲突者刷新事实、重新评估可行性与公平许可；禁止重用旧的“容量足够”判断。
5. 多节点计划整体成功或整体失败；跨不同独立资源域不能把多个局部成功冒充整体承诺。
6. 后续转移按 reservationID、generation、writerEpoch、可信证据版本做幂等条件更新。旧实例不可覆盖新代际状态。

**INV-R01 唯一性：** 相同物理容量只存在一个有效逻辑仲裁域；同一 Demand/generation 不存在两个有效预约。

**INV-R02 容量安全：** 所有相关分配参与者均遵循共享仲裁协议时，每个 Node/资源型号的未交接逻辑承诺与已确认实际分配总量不得超过可承诺容量；未知分配不得当作零。

**INV-R03 无交接空窗：** 实际分配确认之前不得提前解除逻辑扣减；允许临时保守双计，禁止少计。

**INV-R04 幂等与隔离：** 重复/乱序事件不得重复扣减、提前释放或影响其他 Namespace、租户、Job attempt、generation。

**INV-R05 代际保护：** 新 writerEpoch 生效后，旧实例不允许写入或撤销新代际承诺；进程内缓存不产生强承诺。

**INV-R06 安全回收：** 只有可信未分配、撤销或资源释放证据才能使承诺进入 CLOSED。失联和超时只触发核对、隔离及恢复。

以上保证均受参与者范围约束；下游独立 Scheduler 不使用共享预约时，只能承诺预测性准入，不得由 INV-R02 推导跨 Scheduler 的物理排他性。

#### 5.2.5 Runner 到 Workflow 的交接时序

~~~mermaid
sequenceDiagram
  participant A as ARC / Runner
  participant S as NPU Scheduler
  participant L as AccountingDomain权威
  participant K as Kubernetes API
  participant W as Workflow Scheduler
  participant R as Reconciler
  S->>L: Commit(DemandID, CandidatePlan, version, epoch)
  alt 原子预约成功
    L-->>S: reservationID / HELD
    S->>K: Runner绑定
    alt 绑定失败
      S->>L: Revoke(reservationID,generation)
      L-->>S: REVOKING（确认无下游分配后CLOSED）
    else 绑定成功
      S->>L: RunnerBound(reservationID)
      L-->>S: HANDOFF_PENDING
      A->>K: Workflow UID / DemandID关联
      W->>L: 校验预约域与有效代际
      W->>K: Workflow实际放置
      W-->>R: 分配事实及版本
      R->>K: 核对UID、Node、Requests
      R->>L: ConfirmHandoff(可信证据)
      L-->>R: ALLOCATED / 计量归并
    end
  else 容量或版本冲突
    L-->>S: WAIT / RESERVATION_CONFLICT
  end
  Note over W,L: 未参与共享协议时仅有ESTIMATED保证
~~~

交接必须以显式 DemandID、Runner UID、Workflow UID、namespace 与实际 Node/资源证明身份和范围，不允许通过名称去后缀猜测关联。下游不配合预约核验和撤销协议时，Runner 可以获得 ESTIMATED 结论，但不能升级 COORDINATED；Workflow 的实际分配仍可能等待。

对于 Gang 任务，只有个别成员 Pod 出现不代表整个多节点承诺已经交接完成。必须按照每节点向量分别确认，未确认部分继续占用逻辑额度，并遵循整体任务的失败/撤销策略。

#### 5.2.6 故障、租约与恢复

| 事件 | 状态与恢复行为 | 容量语义 |
|---|---|---|
| Runner 绑定失败 | HELD → REVOKING，核对下游未启动 | 证明无分配后 CLOSED |
| Runner 已绑定，Workflow 迟迟未创建 | 核查 Job attempt、Runner 存续和最大持有期限 | 期限只触发撤销，不能单凭过期释放 |
| Workflow 终止/取消/删除 | 通过 UID 与真实分配释放证据核对 | 确认回收后 CLOSED |
| Scheduler 故障或主备切换 | 提升 writerEpoch，恢复账本并与 Workflow/Pod 对账 | 权威未确认之前暂停该域新的 COORDINATED 承诺 |
| 重复、乱序或迟到事件 | 依据 generation、epoch 和 evidenceVersion 校验 | 不得覆盖较新的有效承诺 |
| Liqo 远端失联或资源映射变化 | 标记受影响预约 UNCERTAIN，隔离未知容量 | 其他独立可信域可继续 |
| Node Allocatable 下降 | 重算容量及现有有效承诺，暴露冲突 | 不因容量变化自动删除已有预约 |
| 同名但不同 Namespace 的 Workflow | 禁止以名称匹配作交接 | 身份不明时保持预约 |
| 下游分配端拒绝/丢失确认 | 保留逻辑预约，进入 HANDOFF_PENDING/UNCERTAIN | 不把未确认视为已释放 |

**可用性取舍：** 未知资源保守隔离可能降低短时吞吐，但必须依靠补偿式对账使 UNCERTAIN 最终收敛；不能依靠无条件 TTL 删除消除容量泄漏。恢复时限、故障域和告警阈值由 QA-04/05 后续统一约束。

#### 5.2.7 原因码和诊断证据

| 原因码 | 含义 | 调用方行为 |
|---|---|---|
| RESERVATION_CONFLICT | 域版本或容量竞争冲突 | 刷新资源事实、计划和公平许可后重试 |
| RESERVATION_IDENTITY_CONFLICT | 同一 Demand/generation 的规格、租户或身份不符 | 拒绝覆盖并报告身份异常 |
| RESERVATION_STATE_UNAVAILABLE | 权威账本或代际不可确认 | 等待恢复，不升级保证 |
| RESERVATION_HANDOFF_PENDING | Workflow 实际占用尚未核对 | 继续保留逻辑预约 |
| RESERVATION_HANDOFF_MISMATCH | Workflow 证据与预约资源域、UID 或规格不一致 | 隔离并重新对账 |
| RESERVATION_REVOKE_PENDING | 已要求撤销但释放尚未确认 | 保留不确定容量 |
| RESERVATION_RECOVERY_REQUIRED | 切换/远端失联时发现不可确认预约 | 标记 UNCERTAIN 并停止相关强承诺 |
| RESERVATION_CLOSED | 可信证据证明承诺已释放 | 重新评估同域等待队列 |

最小诊断字段：domainID、reservationID、DemandID、Runner UID、Workflow UID（如已知）、节点资源向量、state、generation、writerEpoch、资源事实版本和原因码。跨租户可见事件不得泄漏他人的预约详情。

#### 5.2.8 DFX 设计验收场景

| TC | 场景 | 可验证的目标结论 |
|---|---|---|
| TC-R01 | 空闲 8 卡节点，两项 6 卡需求并发读取旧余量 | 至多一个域级 CAS 成功，另一个刷新后等待 |
| TC-R02 | 两节点各需 4 卡，但其中一节点不足 | 整个计划提交失败，不产生半份有效预约 |
| TC-R03 | 同一请求重复 Commit、撤销或绑定通知 | 状态及 reservationID 幂等；不重复扣减或释放 |
| TC-R04 | Runner 绑定失败，撤销先于下游状态可见 | 确认无分配后 CLOSED，否则保守保留 |
| TC-R05 | Runner 已绑定，Workflow 未创建且租约到期 | 进入受控撤销核查，不能直接释放容量 |
| TC-R06 | 实际分配可见但交接证据尚未处理 | 可以暂时保守双计，不能存在计量空窗；确认后归并 |
| TC-R07 | 两个 Namespace 有相同名称的 Runner/Workflow | 只有可信 DemandID/UID 对应者能够交接 |
| TC-R08 | 旧 Scheduler 写入迟到，新实例已提高 writerEpoch | 旧代际的写入被拒绝 |
| TC-R09 | 远端失联且状态不可确认 | 相关域不能新增 COORDINATED 承诺，未知容量保留 |
| TC-R10 | Workflow 终止先于 Node 资源释放视图刷新 | 只在可信释放证据具备时释放逻辑承诺 |
| TC-R11 | Workflow Scheduler 不参与预约协议 | 只能声明 ESTIMATED，不能声称跨调度器物理排他 |
| TC-R12 | 同一物理节点映射为两个虚拟资源域 | 不允许双重仲裁承诺；映射冲突时禁止强承诺 |

上述 TC 是**设计可判定场景**，不是测试执行记录；须与 INV-R01～R06、ADR-05/07/09 和 QA-01/04/05/07 保持追踪一致。

### 5.3 IR-03：资源域、配额和候选节点

**SR-06 Liqo 候选。** 根据明确的 NamespaceOffloading 授权和远端资源映射确定可见执行域。远端不可达或映射不可信时不能仅依据虚拟节点数量放行。

**SR-07 本地/远端选择。** 使用相同的 WorkflowShape、型号、每节点可分配资源和配额口径评价两种执行域；仅在均可行后根据策略偏好选择。不得用“本地空闲和”与“远端单节点最大值”作不同语义的可行性比较。

**SR-08 Queue 约束。** 按任务实际归属 Queue、租户及资源域作用范围校验容量与授权；外部 Queue 没有提供可确定配额语义时不能假设无限额度。

**SR-09 Filter 与 Score。** 节点 Filter 仅处理经可行性模块确认的候选；Score 只能在通过约束的候选内优化资源利用率或拓扑偏好。Runner 与 Workflow 分离时，对两种工作负载采用各自正确的放置语义。

### 5.4 IR-04：公平调度、优先级、资源预留与安全 Backfill

本功能单元对应 **SR-10**。将“可执行性”与“公平准许”分开判定：第 4 章给出有效 CandidatePlan，Fairness Policy 决定一个需求是否可以竞争该计划的容量。目标是保留可解释的 FIFO 顺序，同时在租户竞争、PR/Nightly 优先级变化、资源碎片和短任务插空情况下避免长期插队。**不得把 PreFilter 的单次旧 Pod 扫描、QueueSort 的单机队列顺序等同于跨实例或跨租户的公平保证**。

#### 5.4.1 比较范围与队列身份

**竞争域（FairnessPool）**由同一 AccountingDomain 下相同资源型号及可重叠的候选拓扑集合确定。只有候选计划可能争用同一底层容量，才应互相阻塞。互不重叠的物理域、型号或可行候选集可以并行推进。拓扑集合部分重叠而不等价时，使用资源冲突关系及统一裁决，不将任意标签字符串相同误认为真正同一池，也不得形成两个互相不可见的“FIFO”承诺。

**层次化公平**：

1. **资源域层**：对同一物理竞争域串行仲裁会冲突的容量承诺；不跨不相干的资源池设立人为全局队首。
2. **租户层**：可信 tenantID 由平台授权策略产生，按策略配置的租户权重与已兑现 NPU 卡时形成服务份额；持续超额使用的租户在有竞争者时不得永久保持优先。权重不是 Pod 自填标签，默认不构成硬配额。
3. **租户内任务层**：同一个可比较类别的任务按持久到达顺序 FIFO；跨 PR/Nightly 类别先应用分时优先级与防饥饿规则。跨租户选择采用按有效权重归一化的服务欠账（deficit）优先；欠账相同时，按持久到达顺序和 DemandID 稳定比较。
4. **拓扑竞争层**：当不同任务候选集交叉但不相等时，按对相同资源有效的仲裁顺序决定优先者；一个任务可以有多个 CandidatePlan，但只能在一次公平授权中选定一个执行计划和预留集合，不能在多个池里同时占据队首权益。

**排序键**：arrivalSequence 由可信队列接收点持久化单调分配；同一序列冲突时使用 DemandID 稳定打破平局。Pod CreationTimestamp 只可用作辅助证据，不承担跨集群严格排序权威。失败重试或 Scheduler 切换不改变 arrivalSequence；重新提交的独立 Job attempt 使用新键。拒绝越权或永久不可能执行的需求不得阻塞后继。

#### 5.4.2 PR/Nightly 分时优先级与防饥饿

定义策略版本 PolicyEpoch，包含 IANA 时区、日/夜优先级时间窗口、权重、平局规则、保护阈值及状态变更时刻。跨夏令时和地区使用明确的时区解释，不使用服务器本地隐式时间。策略切换由平台时间事件触发，并为等待队列生成新裁决版本。

| 场景 | 调度政策 |
|---|---|
| 白天 PR 优先 | 同一竞争域内优先授予 PR 可执行任务；Nightly 继续入队并保留原始到达顺序 |
| 夜间 Nightly 优先 | 优先授予可执行 Nightly；白天未完成的 PR 不被取消，其有效预留仍受保护 |
| 窗口切换 | 对**尚未被承诺**的需求按新策略重排；已建立的 ReserveCommitment/HELD 按其有效协议保持或受控迁移 |
| 连续高优先级任务涌入 | 长期等待的低优先级租户/类别通过策略规定的最大连续跳过次数与保留机会获得候选仲裁权 |
| 永久不可执行需求 | 返回 INFEASIBLE 并退出该池的有效竞争顺序，不永久封锁后继 |
| 暂时资源不足 | 保留到达顺序，进入有界保护集合，并在资源释放时重新判断，不以繁忙重试次数修改公平顺序 |

**活性保证是条件性的**：在授权和拓扑持续有效、所需资源最终能够释放、后续任务遵守本域仲裁协议、旧任务数及有效跳过上界可控时，受保护需求不会因不断到来的新请求无限次插队。不能把这一性质扩展成“任意资源永远不足时仍能在有限时间完成”。分时优先级不主动抢占运行中的 CI 作业。

#### 5.4.3 Reserve Node：预留承诺与软保护

Reserve Node 是为当前受保护的未满足需求选择一组具体节点/拓扑和未来资源释放边界的**软保护策略**，不同于 5.2 的 HELD 逻辑预约：**软保护不表示当前已经占有全部卡数，也不能被当作设备锁或当前使用率**。

一个 ReserveCommitment 至少包含：protectedDemandID、fairnessPoolID、candidatePlanID、受保护节点及每节点资源向量、建立时间、policyEpoch、队列版本、domainVersion、保护范围、可证明的释放条件，以及（存在可信上限时）最早可承诺就绪时刻。保护集合仅覆盖完成该任务所必需的容量，不把整个池的所有节点无差别冻结。

**选择规则：**

1. 从可执行规格的候选计划中选择**有可能通过已运行任务释放而满足**的最小有效保护集合；优先避免让稀缺多卡/同拓扑资源继续碎片化。
2. 保护前先考虑已有已兑现资源、Queue 约束、其他有效预留以及运行任务的不可抢占属性。资源释放时间不可信时不声明确定的时间承诺。
3. 将 ReserveCommitment 与公平版本写入同一个 AccountingDomain 的受保护决策状态，并让随后申请 HELD 的任务在域级 CAS 时检查该保护版本；避免“公平允许”与“容量写入”之间出现 TOCTOU 竞态。
4. 保护可因需求取消、失去授权、已满足、拓扑故障或策略明确的受控迁移而释放/重算；不得仅因新高优先级任务到来就悄悄抹除已承诺的保护。
5. 新的更紧迫任务可竞争**未保护**的容量，也可依明确规则请求保护迁移；迁移必须证明原受保护任务不受损，或明确声明取消旧保证并形成可追溯的策略决策。

**有界保护**：每域受保护任务数量和预留容量须有可配置上限；优先从有稳定顺序的等待队首选取保护对象；不能因为预留全部节点而让本可并行的任务永久空转。剩余容量可按安全 Backfill 使用。

#### 5.4.4 安全 Backfill 判定

安全回填的目标是提高空闲 NPU 使用，而**不推迟已建立的有效 ReserveCommitment**。与保护对象无资源冲突的空间回填可不依赖任务结束时间；需要借用保护节点资源的时间回填，则必须具备可信且可执行的任务最大运行时间和保护窗口。

~~~text
SafeBackfill(B, P, R, S, V) =
  Authorized(B)
  AND ValidCandidatePlan(P, S)
  AND PreservesAllProtectedPlans(P, R, S, V)
  AND ValidFairnessVersion(V)
  AND (
        SpatiallyDisjoint(P, R)
        OR
        (EnforcedRuntimeBound(B)
         AND VerifiableProtectionHorizon(R)
         AND LatestRequiredRelease(B) + SafetyMargin <= ProtectedStart(R))
      )
~~~

其中 SpatiallyDisjoint 表示回填任务使用的每节点/资源向量不占用受保护任务的必要容量，且不改变其可行 CandidatePlan；不是简单比较 Node 名称。**只有在可行分配不相交时，才能无需运行上限直接回填**。若有多个 ReserveCommitment，必须逐一保持所有有效承诺，不能只检查当前队首。

**时间回填的严格边界：**

- 任务预计耗时、历史 P90/P99、用户自行声明的未经执行时间**不构成安全运行上限**。只有平台可信、受实际执行端约束且覆盖**从借用受保护资源开始到完整释放为止**的端到端最大时长（包括 Runner/Workflow 准备、执行、清理、释放与状态传播），再加独立安全余量，才能形成时间回填证据。
- 如果任务可能超过窗口且不允许抢占，那么在保护节点上的时间回填必须被拒绝；不得依赖未来临时驱逐或杀死正常运行任务来补救。
- 若保护任务的就绪时刻取决于无上限的在运行 Job，不能凭估计值许诺确定开始时间；仅允许不破坏保护容量的空间回填。
- 时间回填成功后，任务也需要通过 5.2 的域级预约 CAS；公平授权和预留承诺版本必须与容量版本一致，竞争失败后重新评价。
- 不允许通过频繁重估保护时刻将受保护任务的预期启动时间不断向后推移；保护一经承诺，时间边界只能因明示的资源故障、授权失效或控制策略变更而受控修订并留下原因证据。

**判定顺序**：先求有效 CandidatePlan → 按公平顺序选保护对象 → 为受保护任务确定资源向量和可证明的保护边界 → 评估后继请求是否空间不冲突或具备安全时间窗口 → 原子校验公平版本及域容量 → 允许回填/等待。

~~~mermaid
flowchart TD
  A["有效CandidatePlan及公平队列"] --> B{"当前优先受保护任务可启动?"}
  B -- 是 --> C["优先授予受保护任务"]
  B -- 否 --> D["建立/保持ReserveCommitment"]
  D --> E["评估后继候选Backfill"]
  E --> F{"不占用受保护必要资源?"}
  F -- 是 --> I["空间回填候选"]
  F -- 否 --> G{"可信执行上限 + 可证明保护窗口?"}
  G -- 否 --> H["FAIRNESS_WAIT"]
  G -- 是 --> J{"最迟释放 + 裕量 <= 保护开始?"}
  J -- 否 --> H
  J -- 是 --> K["时间回填候选"]
  I --> L["域级CAS同时校验容量、保护版本"]
  K --> L
  C --> L
  L --> M{"版本和容量仍有效?"}
  M -- 否 --> N["刷新事实和策略后重算"]
  M -- 是 --> O["许可与逻辑预约"]
~~~

#### 5.4.5 与 Scheduler Framework、预约权威的接口

| 决策接口 | 必需输入 | 输出与约束 |
|---|---|---|
| ClassifyDemand | DemandID、可信租户/JobClass、WorkflowShape、arrivalSequence、PolicyEpoch | 稳定公平身份，不由用户自由修改分类 |
| SelectHead | FairnessPool、租户权重/已兑现服务份额、保护状态、策略窗口 | 受保护头部与可比较次序；不将不可执行任务永久设为头部 |
| CreateProtection | CandidatePlan、域容量、在运行任务的释放证据、策略版本 | ReserveCommitment（节点向量/保护版本/可证明边界） |
| TryBackfill | 后继候选计划、保护集合、时长契约、容量快照 | ALLOW_SPATIAL / ALLOW_TEMPORAL / FAIRNESS_WAIT，附证据 |
| CommitFairness | DemandID、CandidatePlan、domainVersion、protectionVersion、policyEpoch | 与 LogicalReservation 同一仲裁边界作最终 CAS，消除排队许可与资源预约竞态 |
| ReevaluateFairness | 资源释放、保护变化、时间窗口、Queue/授权改变、运行结束 | 保持稳定到达顺序；只影响被变化资源域涉及的任务 |

**Framework 边界**：QueueSort 一次只能采用一个插件，只定义该 Scheduler 内活跃队列的排序，不自行形成多实例跨租户的强公平事实。队列排队、统一 Fairness Policy 仲裁、PreFilter/Filter 的资源可行性检查及 Reserve 阶段的最终权威预约必须使用同一策略版本与同一 AccountingDomain。跨 Scheduler 的共享公平与资源保护若没有统一权威参与，就不能声称该保证成立（ADR-01/05/06/07）。T6 负责细化扩展点组合及事件语义。

#### 5.4.6 公平性不变量与故障行为

**INV-F01（比较范围）：** 只有同一或重叠资源竞争域的需求才互相限制；相同资源通过多种虚拟节点视图暴露时不得形成分裂队列。

**INV-F02（稳定顺序）：** 同租户/可比较任务类别的 FIFO 顺序由持久 arrivalSequence 决定；重试、实例切换、分时优先级变化不改变原始到达顺序。

**INV-F03（保护不后退）：** 新任务的回填或优先级变化不得破坏已承诺的受保护可行容量/时间边界；发生真实资源失效必须显式重算、告警并追踪影响。

**INV-F04（安全回填）：** 借用保护资源的任务只有在可信且受执行端约束的时长与释放裕量满足保护窗口时才可获得许可，否则只能空间无冲突回填。

**INV-F05（公平与容量一致）：** 公平准许必须包含 DemandID、PolicyEpoch、protectionVersion，并与域级逻辑预约在同一个 AccountingDomain 版本化条件更新中验证；并发冲突后不复用失效的公平许可。

**INV-F06（条件性活性）：** 授权资源持续可用并最终释放、任务具有可行拓扑且有效竞争受公平规则约束时，已获得保护资格的旧任务不会因新请求不断到达而无限次失去机会；不承诺在硬件永远不足时给出有限完成时间。

| 故障模式 | 目标行为 | 不可接受行为 |
|---|---|---|
| Fairness Policy 或保护账本不可读取 | 对受影响域返回 FAIRNESS_STATE_UNAVAILABLE，保护现有承诺并等待重评估 | 直接 fail-open 让新任务绕过排序 |
| 事件重复、顺序错乱 | 使用 arrivalSequence、PolicyEpoch、保护版本幂等处理 | 改写历史顺序或重复占用保护容量 |
| PR/Nightly 窗口切换 | 对未承诺任务重评估，保留有效旧承诺 | 无记录地收回已有保护 |
| 远端资源失联 | 隔离失联域、重算独立可信域的候选 | 用其他无关域的空闲卡数填补保护承诺 |
| 运行任务没有可信上限 | 仅在空间不冲突时回填 | 依据平均耗时允许借用保护节点 |
| 资源域 CAS 冲突 | 刷新域版本、保护状态及 CandidatePlan 重评估 | 许可仍按旧候选继续 Reserve |

#### 5.4.7 反例和设计验收场景

| TC | 场景 | 应满足的设计结论 |
|---|---|---|
| TC-F01 | 同池旧需求 8 卡、新需求 1 卡，旧任务尚未满足 | 旧需求获得必要的 Reserve Node 保护；新任务不能通过持续插队让旧任务失去受保护容量 |
| TC-F02 | 旧需求需要 A 节点 8 卡，新任务只用物理无关的 B 节点 1 卡 | 允许空间回填，不因全局 FIFO 阻塞无冲突资源 |
| TC-F03 | 旧 8 卡在一节点已有 4 卡运行、4 卡空闲；新 1 卡需借空闲受保护卡，但无可靠运行上限 | 不允许时间回填；保留队首完成所需的 4 卡保护 |
| TC-F04 | 可信保护窗口还余 60 分钟，新任务可信执行上限加释放裕量为 45 分钟 | 在完整 Node、租户、配额与域级 CAS 均有效时可时间回填 |
| TC-F05 | 保护窗口 60 分钟，新任务仅历史 P90 为 30 分钟，无可信运行上限 | 禁止将该预测当作安全时间回填证据 |
| TC-F06 | 白天 PR 优先、夜间 Nightly 优先，切换时队列内已有有效保护 | 新的未承诺任务按新 PolicyEpoch 排序，旧承诺不得被隐式撤销 |
| TC-F07 | 夜间持续有 Nightly，PR 排队达到保护阈值，第二天仍有新 PR | 按欠账及跳过上界保留等待权利，不能被无限次越过 |
| TC-F08 | 两个租户竞争相同域，租户 A 权重高且已长期占满资源，B 正在等待 | 按已兑现 NPU 卡时和权重计算服务欠账，不能只以绝对权重长期选 A |
| TC-F09 | Liqo 同一物理节点出现两个虚拟视图，任务分属不同 Namespace | 候选物理冲突必须进入相同公平与容量仲裁边界 |
| TC-F10 | 两个任务拓扑约束部分重叠、互不相等 | 只限制真实冲突的可行计划；不按字符串相同或不同机械划池 |
| TC-F11 | Fairness 许可后其他实例改变保护或域容量版本 | 原 CAS 被拒绝，必须重算，不按旧许可绑定 |
| TC-F12 | 队首要求的型号/授权域永远不具备所需硬件 | 返回 INFEASIBLE 并退出有效队首，不形成永久阻塞 |
| TC-F13 | Scheduler 重启导致本地 QueueSort 队列重建 | 从持久公平状态恢复 arrivalSequence、保护与 PolicyEpoch，不重排既有权利 |
| TC-F14 | 受保护任务所需的在运行资源没有可信释放时刻 | 不承诺确定启动时刻，也不通过运行时间估计借用保护容量 |

以上 TC 是**目标架构的可验证场景**，不是工程执行记录。设计结论须与 INV-F01～F06 和 ADR-06 一致。

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
| 大任务队首等待且小任务持续到来 | 分域稳定 FIFO、Reserve Node、严格的空间/时间 Backfill 门槛 | 队首年龄、保护版本、策略窗口、回填证据 | 物理资源长期不足时仍无法承诺有限等待时长 |
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
| SR-04 预约 | 3.4、5.2.1～5.2.4、5.2.6～5.2.8 | ADR-05、ADR-09 | QA-01、QA-04、QA-05 | 域级 CAS、状态机和对账符合 INV-R01～R06；TC-R01～R05、R08～R09、R12 |
| SR-05 交接 | 3.4、5.2.2～5.2.3、5.2.5～5.2.8 | ADR-05、ADR-07 | QA-01、QA-04、QA-07 | 可信 UID/分配证据交接及保守计量；TC-R06～R07、R10～R11 |
| SR-06 Liqo 授权 | 4.2、4.5、5.3 | ADR-02、ADR-04 | QA-01、QA-06 | 无远端授权不可能选远端 |
| SR-07 池选择 | 4.3～4.7、5.3 | ADR-03、ADR-04 | QA-01 | 本地/远端使用同一可行性语义 |
| SR-08 Queue 约束 | 4.5、5.3 | ADR-04、ADR-10 | QA-01、QA-06 | 外部配额不能因缺失或错误默认无限 |
| SR-09 Filter/Score | 4.4、5.3 | ADR-01、ADR-03 | QA-01、QA-03 | 评分集合不得扩大可行候选集合 |
| SR-10 公平 | 5.4.1～5.4.7、3.4 | ADR-06、ADR-05 | QA-02、QA-03、QA-04、QA-07 | INV-F01～F06；TC-F01～F14：分时优先级、稳定公平顺序、保护性预留及安全 Backfill |
| SR-11 事件 | 5.5、6.3 | ADR-08、ADR-11 | QA-04、QA-05 | 关键事实变化能触发受控重评估 |
| SR-12 运行支撑 | 5.5、6.3～6.4 | ADR-08～11 | QA-05～07 | 控制面故障有明确响应及关联诊断 |

## 8 技术规范参考

- [Kubernetes Scheduling Framework](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/)：Scheduler 扩展点、调度/绑定周期、QueueSort、Reserve/Unreserve。
- [Kubernetes Running Multiple Schedulers](https://kubernetes.io/docs/concepts/scheduling-eviction/running-multiple-schedulers/)：多调度器间的节点和资源竞争边界。
- [Liqo 官方项目](https://github.com/liqotech/liqo)：虚拟节点与 Namespace Offloading 相关协议。
- [Volcano 官方文档](https://volcano.sh/docs/)：Queue、配额与工作负载调度语义。
