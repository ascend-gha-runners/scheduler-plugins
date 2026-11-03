# ARC NPU 调度子系统详细设计说明

## 1 设计目标与范围

### 1.1 业务目标

**BG-01 NPU 感知准入。** 解决调度对象未体现最终 NPU 需求，导致资源不足时任务过早进入执行阶段的问题。目标是在准入时识别可信的完整资源需求，结合授权、拓扑和资源状态判断可执行性；不可满足时保留等待，而不是仅凭资源总量提前放行。

**典型场景。** 在 ARC Container Mode 中，Runner Pod 自身可能仅申请 CPU，已启动的 Runner 随后创建真正消耗 NPU 的 Workflow Pod。例如，本地剩余 8 卡、远端剩余 10 卡，而 Workflow 需要单节点 12 卡。虽然空闲量合计为 18 卡，但不存在满足要求的单节点；如果只依据 Runner 自身资源或跨域总量放行，就会出现 Runner 已启动、Workflow 却持续等待 NPU 的情况。设计目标是在 Runner Pod 调度前利用可信的执行规格作出可行性判断。

**BG-02 公平等待与资源效能。** 解决资源竞争时到达顺序失真、大任务持续被插队以及严格 FIFO 队首阻塞影响吞吐的问题。目标是在明确的竞争范围内实行稳定 FIFO、PR/Nightly 分时优先级、Reserve Node 保护及安全 Backfill，使公平性与资源利用率兼顾，不通过抢占终止运行中的任务。

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

| IR | 需要解决的问题 | 设计目标 | 对应 SR | BG |
|---|---|---|---|---|
| IR-01 | 需求声明与最终执行规格脱节，聚合容量无法证明执行可行性 | 校验可信需求及授权，形成满足型号、容量、拓扑和放置约束的可行执行计划 | SR-01 需求校验；SR-02 资源核算；SR-03 可行性判断 | BG-01 |
| IR-02 | 并发准入可能重复承诺容量，预约与真实分配之间存在交接空窗 | 建立版本化、可恢复的逻辑预约及一致的分配交接与对账 | SR-04 预约和回收；SR-05 跨阶段关联与对账 | BG-01 |
| IR-03 | 跨资源域可见性、授权、Queue 配额和节点放置约束可能不一致 | 构建授权且可验证的候选资源域，在同一约束口径下选择、过滤和评分 | SR-06 Liqo 候选；SR-07 资源域选择；SR-08 Queue 约束；SR-09 Filter/Score 一致性 | BG-01 |
| IR-04 | 队列重试与任务规模差异造成持续插队，严格 FIFO 又可能阻塞无冲突资源 | 在明确竞争域内统一 FIFO、分时优先级、Reserve Node 及安全 Backfill | SR-10 竞争任务调度 | BG-02 |
| IR-05 | 外部状态变化和组件故障可能导致过期决策、难以恢复或无法定位等待原因 | 提供可追踪的重评估、数据一致性、故障恢复与诊断契约 | SR-11 状态变化与重评估；SR-12 运行支撑 | BG-01 / BG-02 |

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
| AccountingDomain | domainID、physicalResourceIdentity、nodeCapacityVector、reservationIndex、version、writerEpoch、PolicyEpoch、protectionVersion | 有界且物理互斥的容量及公平承诺原子域，见 5.2 和 6.2.4 | 
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
| ADR-12 有界容量仲裁与增量索引 | 物理不重叠的有界 AccountingDomain、增量资源/队列索引、预算化拓扑搜索和有界 CAS 重试 | 跨分区 Gang/全局 Queue 配额需要额外强协议；缺失时不能宣称强保证（6.2.4） |

## 4 NPU 需求与容量准入设计（IR-01、IR-03）

### 4.1 准入的目标语义

**问题：** 调度对象的资源申请和最终执行负载的要求可能不同；单独检查调度对象能否绑定，不能证明目标 NPU 需求能够满足。

**目标：** 对可信需求的完整执行规格进行可行性判定，并分别应用调度对象与实际执行负载的放置约束。准入需要同时检查授权、资源型号、可用量、拓扑、Queue 及数据有效性；任何一个必要条件不满足，都不能仅凭聚合容量放行。

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
  A["规范化 NPU 需求与执行规格"] --> B{"需求合法且授权？"}
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

| TC | 情景 | 正确判定 | 关键依据 |
|---|---|---|---|
| TC-C01 | 单节点需求 C；两个物理节点各自余量均小于 C，但余量合计大于或等于 C | 不得按合计余量放行；根据授权域是否具备理论容量区分 INFEASIBLE / WAIT | 单节点需求必须由同一可执行节点满足 |
| TC-C02 | 明确声明多节点形态及每节点需求，所有指定节点余量和拓扑均满足 | 仅在拓扑、组调度和每节点约束均成立时可行 | 多节点需求必须按完整 WorkflowShape 判断 |
| TC-C03 | 本地与远端资源域各有不足以独立满足单节点需求的余量，跨域求和表面充足 | 不准入 | 不同执行域不得拼接成单节点容量 |
| TC-C04 | Runner 指向 CPU Node、Workflow 指向 NPU Node | 分别检查两个工作负载的约束 | CPU Runner NodeSelector 不决定 NPU Workflow 放置 |
| TC-C05 | 匹配型号有 8 卡，其他型号还有 8 卡，请求某型号 12 卡 | 不准入 | 不同扩展资源名不可相加 |
| TC-C06 | 一个节点 8 卡，真实已分配 2 卡，未交接预约 3 卡，请求 4 卡 | 不可立即准入 | 可用量仅 3 卡 |
| TC-C07 | 同一物理 8 卡在本地视图和虚拟视图各出现一次 | 至多按 8 卡计入 | 必须去重，禁止镜像重复计量 |
| TC-C08 | Liqo 不允许该 namespace 使用远端 | 远端不参与候选 | 授权优先于容量 |
| TC-C09 | Queue 上限允许 2 卡，请求 4 卡 | 不得以充足物理容量绕过配额 | Queue 适用范围与真实已占用口径 |
| TC-C10 | Informer/预约账本数据过期或不可确认 | WAIT DATA_UNAVAILABLE；仅独立可信域可继续 | 不能从未知事实推导容量 |
| TC-C11 | 两次并发读取同一余量 | 预约须以版本仲裁，失败者重试 | 快照判断不是并发承诺 |
| TC-C12 | 下游使用独立 Scheduler 且未共享预约 | ESTIMATED，不得声明 COORDINATED | 明确跨 Scheduler 竞争风险 |

### 4.8 正确性与异常契约

**不变量 INV-C01：** 一个通过准入的需求至少有一个同时满足授权、型号、容量、放置、拓扑及配额约束的 CandidatePlan；允许明确的预测误差，但不能把不满足硬约束的组合称为可行。

**INV-C02：** 同一决策依据的资源事实和预约状态具有可核验版本；并发冲突不能沿用已失效的可行性结果。

**INV-C03：** 同一物理容量不得因 Liqo 重复映射而重复计入。Workflow 分配确认前不得提前解除逻辑承诺；未确认交接时允许暂时保守双计，确认后必须归并，禁止持续双计或少计（见 5.2.3、INV-R03）。

**INV-C04：** 未授权或未知状态不能被“容量足够”“评分较高”或“远端可见”等非授权信号覆盖。

**INV-C05：** Score 只对有效候选排序。不存在可行候选时，调度不会通过修改 Score 创造容量。复杂拓扑搜索超预算为证据不足，不自动构成不可执行证明（见 6.2.3）。

异常类型包括非法需求、权限不满足、资源永久不兼容、暂时资源不足、数据过期、竞态版本冲突和下游协调失败。可恢复异常必须声明重评估事件；安全性相关错误不得静默放行。

## 5 功能单元设计

### 5.1 IR-01：需求校验、资源计量和准入

**问题：** 需求身份和资源规格可能缺失或越权，容量统计可能忽略分配事实、拓扑与放置限制，导致错误准入。

**目标：** 将需求规范化并完成授权校验，以同一有效事实版本核算可用容量，为完整执行规格生成可验证的 CandidatePlan，并输出明确的准入状态与原因。

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

**问题：** 并发决策可能重复承诺同一资源，预估、预约和真实分配之间的状态变更也可能产生少计、重复计量或不安全释放。

**目标：** 为 **SR-04 逻辑预约与回收**、**SR-05 跨阶段关联及计量交接**定义单一权威、原子仲裁、恢复和对账契约。预约应在重试、失败与组件切换后保持一致，直到获得可信交接或释放证据。**逻辑预约不是设备锁**；强协调保证以最终分配端参与同一协议为前提（ADR-05、ADR-07、ADR-09）。

#### 5.2.1 预约权威与一致性范围

**单一权威：** 按不重叠的物理资源竞争域和资源型号建立 AccountingDomain；每个域具有持久化权威账本、可比较版本和有效写入者代际（writerEpoch）。同一域的账本包含全部受保护节点的可用资源向量及有效预约。不同虚拟节点若映射同一物理资源，必须受同一个权威仲裁，不得各自独立承诺该容量。域对象与活动索引必须有界，禁止无界单对象扩张；跨域 Gang 无统一原子协议时不得宣称整体 COORDINATED（ADR-12、6.2.4）。

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

**问题：** 多资源域的可见容量、物理映射、租户授权和 Queue 约束未必采用相同口径，资源池选择可能偏离真正可执行的候选范围。

**目标：** 仅在授权、拓扑、配额和数据有效性同时满足时形成候选执行域，并保持资源池选择与最终 Filter/Score 的约束一致。

**SR-06 Liqo 候选。** 根据 Namespace Offloading 授权、远端资源映射和成员集群可验证的物理拓扑形成执行域；资源仅具虚拟聚合视图时不能宣称满足单节点强可执行性。成员集群负责最终 NPU Node 放置（见 5.6.4）。

**SR-07 本地/远端选择。** 使用相同的 WorkflowShape、型号、每节点可分配资源和配额口径评价两种执行域；仅在均可行后根据策略偏好选择。不得用“本地空闲和”与“远端单节点最大值”作不同语义的可行性比较。

**SR-08 Queue 约束。** 按可信租户/Queue 身份及其实际作用域检查配额，区分 Volcano capability 上限、deserved 共享份额和 guarantee 保护语义；调度策略/Queue 使用事实无法核实时不得默认为无限额度（见 5.6.5）。

**SR-09 Filter 与 Score。** Framework Filter/Score 直接作用于 Runner Pod 的合法节点集合；未来 Workflow 的 NPU CandidatePlan 在独立可行性模块产生，且只能在合法计划集合内评分。两者可以位于不同 Node/Cluster，不能混用 NodeSelector 或把 Runner Node Score 当成 Workflow NPU 放置保证（见 5.6.3）。

### 5.4 IR-04：公平调度、优先级、资源预留与安全 Backfill

**问题：** 资源竞争中的重试、任务大小、不同租户及类别优先级，可能导致长期插队或无冲突容量无法利用。

**目标：** 为 **SR-10** 定义可解释的 FIFO 顺序、分时优先级和 Reserve Node 保护，在不破坏保护承诺的前提下允许安全 Backfill。可行性由 CandidatePlan 负责，是否获准竞争容量由公平策略仲裁；不能把单次 Pod 扫描或单机 QueueSort 等同于跨实例、跨租户公平保证。

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

**Framework 边界**：QueueSort 一次只能采用一个插件，只定义该 Scheduler 内活跃队列的排序，不自行形成多实例跨租户的强公平事实。队列排队、统一 Fairness Policy 仲裁、PreFilter/Filter 的资源可行性检查及 Reserve 阶段的最终权威预约必须使用同一策略版本与同一 AccountingDomain。跨 Scheduler 的共享公平与资源保护若没有统一权威参与，就不能声称该保证成立（ADR-01/05/06/07）。扩展点组合和失败事件协议见 5.6。

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

**问题：** 外部资源、策略和预约状态异步变化，若缺少可靠重评估与证据关联，将导致等待无法及时解除、过期决策或故障原因不清。

**目标：** 让关键状态变化可靠触发有界重评估，建立版本一致的诊断证据和故障恢复规则，同时保持授权与容量正确性。

**SR-11 事件与重评估。** 资源释放、Node 变化、Liqo 授权、Queue 变化、PolicyEpoch 变动、预约释放和 Workflow 交接应经明确的重评估链影响 WAIT 需求。Pod/Node 可利用 Framework 事件提示；Liqo/Volcano CR 变化不能假定自动进入 QueueingHint，应定义显式桥接或有界周期核对，保留稳定队列身份和有界退避（见 5.6.6）。

**SR-12 运行支撑。** 子系统应定义 Runner 调度与 Workflow 实际调度的分界、单一有效预约权威、各外部依赖的失败语义、最小权限、跨组件关联证据。关键授权/账本/远端物理事实未知时停止相应域的强承诺，而独立可信域可以继续；指标与诊断输出失败不改变准入正确性（见 5.6.1、5.6.6～5.6.8）。

**诊断契约：** 需求 ID、决策原因、候选执行域、数据和策略版本、预约状态、Workflow UID 应可跨组件关联。拒绝原因与资源不足、队首公平等待或数据不可用必须区分。

### 5.6 跨组件接口与 Scheduler Framework 契约

本节将 IR-01～IR-05 的逻辑设计映射到真实的系统责任边界。所谓“接口”，是目标协议的输入、输出、状态权威及异常语义，不要求新增特定控制器或采用特定部署形态。**NPU Scheduler 只调度 Runner Pod；Workflow Pod 的绑定由其实际 schedulerName 指向的 Scheduler 执行**。外部系统不参与协议时，不能从本子系统的局部判断推导全链路资源保证。

#### 5.6.1 外部交互总览

| 边界 | 输入及可信来源 | 本子系统的输出与作用 | 责任边界及降级 |
|---|---|---|---|
| **ARC Controller / Runner / Hooks** | Runner UID、Namespace、Job attempt 关联、可信 NPU Demand 与实际作业模式 | 需求准入、Runner Pending 原因、关联 DemandID/ReservationID | ARC Listener 决定接单；hooks/Runner 在约定的作业模式下创建 Workflow Pod；无可信前瞻需求时不能声称 NPU 感知排队 |
| **Kubernetes Scheduling Framework** | 待调度 Runner Pod、合法调度 Profile、Runner Node 信息和 Scheduler CycleState | Demand 验证、Workflow 候选证据、Runner 节点过滤/评分、域级预约、绑定前校验、失败回退 | 只能对 Runner 的 schedulerName 所属调度周期直接产生绑定行为；既有通用 Filter 对 Runner 自身的约束仍生效 |
| **Kubernetes API / Device Plugin** | Node Allocatable、Pod Requests、实际绑定状态、资源版本和可见性 | CapacityView、版本化预约状态、状态变化触发 | 扩展资源的物理权威属于设备插件/成员集群；API 快照不能证明卡上 AI Core 实际利用率 |
| **Liqo** | Namespace Offloading 授权、被选远端、虚拟节点映射、资源可见性 | 过滤可执行的远端候选域，并说明成员集群的最终物理放置责任 | 远端映射若不包含可核验的物理拓扑，则不可宣称单节点/多节点强可行性 |
| **Volcano Queue** | 可信任务 Queue 身份、Queue 生效策略、配额/使用量版本和生效范围 | 检查 Queue 约束，形成可追踪配额判断 | capability 是使用上限；deserved/guarantee 含不同份额语义；读取 Queue 对象不是执行配额锁定 |
| **Workflow Scheduler（Volcano 或其他）** | 真实 Workflow UID、Requests、PodGroup/拓扑、其自身的调度策略 | 消费可验证的预约关联、执行真实放置、提供交接和释放证据 | 未支持共同仲裁和撤销协议时只能 ESTIMATED；本子系统不得把其选择结果写成自身的节点绑定 |
| **预约权威与 Fairness Policy** | 域级节点向量、PolicyEpoch、protectionVersion、writerEpoch、候选证据 | 原子承诺/回退、受保护公平次序与重评估 | 只约束参与相同协议的执行者；与实际 Node/Pod 分配证据保持可对账 |

**需求存在的前置条件：** ARC Runner 与作业负载可以分成两个 Pod 的路径，必须由可信平台模板或工作流契约提供**与实际作业 Pod 一致的资源规格**，而不是从 Runner Pod 本身的 CPU Requests 猜测。对于不通过容器 hooks 创建独立作业 Pod 的 Workflow，不能机械套用该路径；需要分别确定实际 NPU 消费者及其调度责任。若无法可信地预先获知需求，受 NPU 准入管理的任务应返回 DEMAND_UNAVAILABLE 并保留等待/拒绝语义，不能默认按 0 卡放行。

#### 5.6.2 Runner/Workflow 身份与需求协议

| 协议字段 | 生产者与可信性 | 消费者与约束 |
|---|---|---|
| DemandID、Runner UID、Namespace、Job attempt | ARC/平台可信关联层产生或授权；标识应包含稳定的集群作用域 | Demand Validation、Reservation、Workflow 关联；同名 Pod 不代表同一个 Job |
| resourceName、count、WorkflowShape | 被授权的作业规格/模板；必须与最终 Pod Requests 与拓扑相一致 | 候选规划器；无来源、格式错误或不匹配视为 INVALID_DEMAND |
| queueID、tenantID、allowedDomains | 平台认证与租户授权策略决定，不接受任意用户标签作为最终授权 | Queue 约束、Liqo 授权、FairnessPool |
| guaranteeLevel、CandidatePlanID、domainID | 子系统根据参与者能力和最新事实计算，附有效期与版本 | Runner 准入通知、下游 Workflow 调度端；不得由用户要求直接升级 |
| reservationID、generation、writerEpoch、protectionVersion | 预约权威与公平仲裁产生 | 跨阶段交接、幂等更新、故障恢复；所有者身份须核验 |
| workflowRef（Cluster/Namespace/UID）、allocationEvidenceVersion | 最终 Workflow 调度/Pod 事实提供，经可信关联验证 | Reservation Reconciler；只有证据满足目标资源域和型号才能交接 |

**模式判定**：在 ARC Kubernetes container mode 创建独立作业 Pod 的场景，Runner 资源约束与 Workflow Pod 资源约束分别计算；如无独立作业 Pod 则以真正的 NPU 消费者为准。若作业 Pod 的实际 Requests 大于声明、型号不符或 Node/拓扑约束不同，**不得继承旧预约保证**；返回 WORKFLOW_SPEC_MISMATCH，保留或受控撤销原承诺，再对真实需求重新准入。

**最小关联协议：** Runner/Job 标识 → DemandID → ReservationID → Workflow UID。身份是逻辑契约，不规定传递载体必须是 annotation、CRD 或 RPC；但应保证不可信任务不能伪造有效授权或交接证据。

#### 5.6.3 Scheduling Framework 扩展点职责

| 扩展点 | 目标职责 | 不可承担的职责 |
|---|---|---|
| **QueueSort** | 对同一 Scheduler 进程只配置一个合法队列比较器；多 Profile 必须采用一致的队列排序行为，局部映射稳定公平顺序 | 不假定独立 Scheduler 进程的队列天然同序；不能同时启用多个 QueueSort 实现 |
| **PreEnqueue（可选）** | 根据可信且已可得的准入/策略结果阻止明显不可进入活动队列的 Runner | 不依赖它完成长期阻塞任务的唯一唤醒；不可把不可信旧缓存变成硬拒绝 |
| **PreFilter** | 规范化 Runner 需求、准备 Workflow CandidatePlan、读取同版本授权与 Fairness 判定，输出本轮 CycleState | 不能在此直接修改持久预约，亦不应在 Runner CPU 节点上判断 Workflow NPU 是否被物理绑定 |
| **Filter** | 过滤**Runner Pod 自己**不符合 Kubernetes 节点/Pod 约束的节点；校验本轮所选资源域的 Runner 可达/交互限制 | 不把将来 Workflow 的 NPU CandidatePlan 节点硬套成 Runner 的运行节点；不把 Filter 成功等于实际 NPU 分配 |
| **Score/NormalizeScore** | 在 Runner 合法候选内优化 Runner 节点放置偏好；Workflow 执行域的策略评分由 CandidatePlanner 独立决定 | 不把未满足硬约束的 Node 通过高分重新引入可行集合 |
| **Reserve** | 对已获公平许可及有效 Workflow CandidatePlan 执行 AccountingDomain 的域级 CAS，生成 HELD 逻辑预约；附域版本、策略与保护版本 | Framework 的本轮 Reserve 不是对另一个 Scheduler 的物理设备保留 |
| **Unreserve** | 为当前调度轮的失败请求发出幂等撤销意图；由权威账本/对账确认最终释放 | 不以回调发生就推定 Workflow 已终止；不依赖回调必达或进程不崩溃 |
| **Permit（按需）** | 对存在明确下游协议的 Runner 可声明有界等待/允许策略；仅在 Framework 支持的语义内使用 | 不通过 Permit 假装别的 Scheduler 已占有 NPU，也不无限阻塞绑定周期 |
| **PreBind / Bind / PostBind** | 绑定前检查该 reservationID/generation 仍有效、所选 Runner Node 合法且受保护公平承诺未失效；由合法 Bind 路径绑定 Runner；绑定后关联通知与恢复对账 | 不要求成功 CAS 之后整个域版本永远不变；不把 PostBind 当作唯一可靠交接依据；成功 Bind Runner 不保证 Workflow Bind |
| **EnqueueExtension / 重新入队** | 将本插件关注的 Pod/Node 变化映射为重试提示，并为外部状态变化定义有界、可靠的重评估触发链 | 不假设所有任意 CRD 更新都会自动触发 Framework 的队列提示；不得只依赖一次事件 |

**原生阶段顺序**：同一 Scheduler 的调度周期串行执行，绑定周期可并发，因此不同调度上下文可能在绑定期间竞争；对外状态变化后，CycleState 快照可能变旧，因此 Reserve 必须对当前域级版本执行原子比较，绑定前则检查**已成功取得的预约本身仍有效**，不能因别的任务合法提交导致域版本变更就无故丢弃有效预约。Pod 调度周期失败应映射为 Framework 可识别的不可调度/错误类别：永久非法规格和权限问题在相关授权/需求改变前不可恢复，临时容量与公平等待属于可重评估不可调度，内部读取/状态异常必须显式区别并限制重试风暴。**合法资源不足与系统内部错误不能混成同一个 Status**。不启用会对运行中的任务执行抢占的路径，PostFilter 的救济不得绕过非抢占原则。

**重要映射规则**：NPU CandidatePlan 是后续 Workflow 的放置计划；Runner Node 是 kube-scheduler 正在选择的节点。两者是**不同对象**。若产品需要二者同域或同节点，必须把这个关联作为显式运行时/数据访问约束写入 WorkflowShape，不能靠复用 Filter(NodeInfo) 的参数产生该保证。

#### 5.6.4 Liqo 与成员集群职责

~~~mermaid
sequenceDiagram
  participant NS as NPU Scheduler / CandidatePlanner
  participant L as Liqo可见性与Offloading
  participant API as 本地Kubernetes API
  participant VR as 虚拟Node
  participant MC as 成员集群Scheduler
  NS->>L: 查询Namespace授权、允许的远端集群
  L-->>NS: 授权远端集合/策略版本
  NS->>API: 读取远端映射、资源事实
  API-->>NS: VirtualNode / 可验证容量证据
  alt 映射包含完整可信物理执行拓扑
    NS->>NS: 形成远端CandidatePlan
  else 仅聚合容量或映射不可信
    NS-->>API: 不能声明具体物理可执行或强保证
  end
  NS->>API: 表达已授权的远端候选域约束（非物理卡绑定）
  API->>VR: VirtualNode对应Offloading选择
  VR->>MC: Offloading后的Workflow Pod由远端调度
  MC->>MC: 按物理Node、拓扑、设备进行最终放置
  MC-->>API: 真实分配证据供交接
~~~

**决策权**：本地 Scheduler + Liqo 只决定是否可以把工作负载提交到**已授权的远端资源域**；成员集群 Scheduler 才有权选择真实 NPU Node。Liqo virtual Node 的聚合容量，不自动等于成员集群中存在单台满足需求的物理机器。

**一致性要求**：Offloading 授权与虚拟资源映射应使用同一有效策略版本；远端物理 Node 能否被实际分配必须由可信成员事实或参与的下游协议确认。远端集群失联、Offloading 撤销、映射冲突时将该域标记为不可验证并触发重评估，不从“虚拟节点仍存在”推断远端容量仍可用。若只拿到聚合可见性、无法证明满足完整 WorkflowShape，应返回 REMOTE_TOPOLOGY_UNKNOWN / DATA_UNAVAILABLE，并等待成员集群可信拓扑证据；**不得给出 ESTIMATED 准入，更不能给出 COORDINATED 保证**。ESTIMATED 也要求存在符合第 4.3 节的完整 CandidatePlan。

#### 5.6.5 Volcano Queue 与 Workflow Scheduler 协同

**Queue 不是一个简单整数**。Volcano 的 capability、deserved、guarantee 是不同语义；Queue 的分配/回收/优先级效果还取决于 Volcano 调度配置。对应不同资源域和租户，须核对 Queue 所属、配额作用域、有效使用量以及所启用的策略插件，才能形成 QueueQuotaFit 结论。

| Volcano 契约 | 本子系统必须明确 | 边界 |
|---|---|---|
| Queue 身份 | 可信 Job/tenant 到 Queue 的映射、适用集群与版本 | Pod annotation 本身不授予使用其他 Queue 的权利 |
| capability | 请求及已占用的同资源维度上限；区分物理事实、预约及下游 Queue 计量 | 只能按适用的配额范围检查，不能因本地总容量大而绕过 |
| deserved/guarantee | 按 Volcano 已定义的共享份额/保留语义影响策略，不能当作额外物理 Allocatable | 不把借用或软份额误写成不可撤销的 NPU 锁 |
| Scheduler 配置 | 明确 enqueue、allocate、reclaim、preempt 与 Queue 插件的策略边界 | 当前设计不允许通过外部抢占打破“不抢占运行任务”的目标 |
| Workflow Pod/PodGroup | 由真正执行端判定 Gang、Node 选择和作业生命周期 | Runner 预约不等于 Volcano PodGroup 已满足组调度要求 |
| 协调保证 | 下游必须接入同一 AccountingDomain 授权、提交、交接、撤销及版本协议 | 没有该协议只能 ESTIMATED；只读取 Queue CR 不能升级 COORDINATED |

**策略冲突规则**：该目标架构的受管理资源域**不得启用**会驱逐正在运行的 CI 任务的默认抢占、Volcano preempt 或 reclaim 路径；当下游无法按此约束关闭抢占/回收时，不能将其纳入非抢占调度承诺作用域。Volcano deserved 的共享资源可被 reclaim 的官方语义不能与“已运行任务绝不抢占”的目标同时无条件保证。不得通过修改 Volcano 本身的实际调度决定来代替本子系统的公平和容量仲裁。

#### 5.6.6 统一错误、重试和重评估协议

| 触发边界 | 错误/等待原因 | 目标响应与再评估条件 | 能否保留既有承诺 |
|---|---|---|---|
| ARC 缺少可信的前瞻 NPU 需求 | DEMAND_UNAVAILABLE | 不按 0 卡放行；需求补齐或作业模式明确后重评估 | 已合法建立的预约维持其协议 |
| Runner/Workflow 规格不一致 | WORKFLOW_SPEC_MISMATCH | 现有预约进入受控撤销/隔离，对真实规格重新做授权准入 | 不得沿用不匹配的旧承诺 |
| Framework 资源不足/可行集为空 | INSUFFICIENT_CAPACITY / INFEASIBLE | 分别随资源释放或规格/授权变化重评估；拒绝永久不可能的规格 | 无新承诺 |
| Framework Reserve CAS 冲突 | RESERVATION_CONFLICT | 获取新资源/策略/保护版本后重算，不循环复用旧 CycleState | 已由其他任务取得的承诺有效 |
| Kubernetes API 关键版本/账本不可用 | RESERVATION_STATE_UNAVAILABLE | 停止受影响域的 COORDINATED 提交；API 与账本恢复后对账 | 未证实可释放的承诺维持保护 |
| Liqo Offloading 取消或远端不可达 | REMOTE_DOMAIN_UNAVAILABLE | 移除失效候选，独立本地域如可信可重算；远端恢复后重评估 | 远端在途预约进入保守核对 |
| Liqo 远端只暴露汇总、缺失物理映射 | REMOTE_TOPOLOGY_UNKNOWN | 不宣称单节点可行或强保证；等待可信物理拓扑证据 | 不新增无法兑现的强预约 |
| Volcano Queue 状态未知或配额不可信 | QUEUE_STATE_UNAVAILABLE | 受影响 Queue 不绕过配额；Queue 数据/策略恢复后重评估 | 已获有效授权的承诺须有明确撤销机制 |
| Volcano Queue 已达硬上限 | QUEUE_QUOTA_EXCEEDED | 等待配额释放或授权的 Queue 变更 | 不以其它 Queue 闲置额度自动冒用 |
| Fairness Policy/保护版本变化 | FAIRNESS_STATE_UNAVAILABLE / FAIRNESS_WAIT | 按新 PolicyEpoch、protectionVersion 重算 | 不可悄悄取消已有有效保护 |
| Workflow Scheduler 未确认预约协议 | DOWNSTREAM_UNCOORDINATED | 只声明 ESTIMATED，并向调用方说明仍可能有 Workflow Pending | 不宣称物理排他 |
| Workflow 绑定/释放证据缺失或矛盾 | RESERVATION_HANDOFF_MISMATCH | 转 UNCERTAIN、保守计量并对账，证据更新后解除 | 不提前释放逻辑预约 |

**重评估信号**：Pod/Node 资源变化、Runner/Workflow 状态变化、有效预约释放、Offloading 授权变化、Queue 状态变化、公平策略窗口切换，以及明确的账本恢复事件。Kubernetes Framework 对原生 Pod/Node 事件可以使用 EnqueueExtension/QueueingHint；Liqo/Volcano 自定义对象变化须经**显式事件桥接到被调度 Runner 的可重评估状态**，或通过有界周期核对补偿。不得声称自定义 CRD 自动具备通用 QueueingHint 能力。

**故障恢复原则**：重复事件保持幂等；关键事实过期则等待；与失败域无关的资源域可继续；对疑似已占用的 NPU 容量保守保护；诊断/指标输出不可用不影响授权和容量正确性。重试采用按原因分类的事件触发与有界退避，避免控制面故障触发全池高频扫描。

#### 5.6.7 关键跨组件时序与保证级别

~~~mermaid
sequenceDiagram
  participant ARC as ARC / Runner
  participant SCH as Runner NPU Scheduler
  participant API as Kubernetes API
  participant LIQO as Liqo
  participant VOL as Volcano/Workflow Scheduler
  participant LED as AccountingDomain
  ARC->>API: Runner Pod / 可信DemandID及WorkflowShape
  API->>SCH: Runner调度周期
  SCH->>LIQO: 远端可见性和授权
  LIQO-->>SCH: domain映射和策略版本
  SCH->>API: 读取Node/Pod/Queue等资源事实
  SCH->>SCH: 检查可行CandidatePlan和公平许可
  SCH->>LED: Reserve: CAS(计划,域/保护版本,epoch)
  alt 预约/公平许可冲突
    LED-->>SCH: RETRY_WITH_FRESH_FACTS
    SCH-->>API: Runner继续等待
  else 获得有效逻辑预约
    LED-->>SCH: HELD + reservationID
    SCH->>API: 按Runner自身约束Bind
    ARC->>API: Hooks创建具关联的Workflow Pod
    VOL->>LED: 若接入协议则校验并接受预约
    VOL->>API: 对Workflow实际放置和绑定
    API-->>LED: 可信分配事实/交接证据
    LED-->>SCH: ALLOCATED或UNCERTAIN
  end
  Note over SCH,VOL: 无共同仲裁和交接时仅有ESTIMATED
~~~

**保证范围**：PreFilter/Filter/Score 不产生资源排他；Runner Bind 不代表下游 Workflow 成功启动；只有预约权威和最终资源分配端共同遵守契约时才能声明 COORDINATED。成员集群对物理 NPU Node 的选择、真实设备分配与故障处理是其明确的责任，不能由 Liqo 虚拟节点或 Runner Scheduler 代行。

#### 5.6.8 一致性不变量与设计场景

**INV-I01**：ARC 需求声明与真实 Workflow 消费者必须关联同一个 Job attempt 和资源规格；缺少可信来源不允许按零卡假定。

**INV-I02**：Runner 自身 Node Filter 与 Workflow NPU CandidatePlan 不得混淆；两个 Scheduler 的绑定行为不互相替代。

**INV-I03**：远端只有经过 Offloading 授权且可验证物理拓扑的候选才能作为强可执行计划；虚拟聚合容量本身不证明某个单节点存在。

**INV-I04**：Volcano Queue 使用真实适用范围及版本；本子系统不会在 Queue/身份未知时绕过硬授权或额度，也不隐式启用抢占。

**INV-I05**：Fairness 许可、Reservation CAS 和 Runner 绑定前一致性检查引用可核验的保护/容量/策略版本；失败后重新计算而不复用旧许可。

**INV-I06**：跨 Scheduler 的 COORDINATED 等级以共同参与预约验证、交接、释放和补偿对账为必要条件；没有下游确认时永远不能自称强保证。

| TC | 架构场景 | 可判定的设计结论 |
|---|---|---|
| TC-I01 | ARC Runner Pod 不请求 NPU，但可信规格表明后续 Workflow 请求 8 卡 | PreFilter 基于 WorkflowShape 准入；Runner 的 CPU Node 与未来 NPU Node 分别过滤 |
| TC-I02 | Container Mode 的 Workflow 没有可信 NPU 需求声明 | DEMAND_UNAVAILABLE；不以 0 卡默许 Runner 占位 |
| TC-I03 | Runner 调度成功而 Workflow 使用另一 schedulerName | 仍可 Pending；未接共享仲裁时只报告 ESTIMATED |
| TC-I04 | Framework 的 Runner Node Filter 与 Workflow CandidatePlan 拥有不同 NodeSelector | 两者各按自身约束检查，不合并为一个 NodeSelector |
| TC-I05 | Reserve 成功但 Bind 失败，Unreserve 重复或未到达 | 由持久状态受控撤销/对账；不因回调丢失泄漏或误释放 |
| TC-I06 | Liqo 虚拟节点仅提供聚合余量，但映射到的物理节点均不满足完整单节点需求 | 不以虚拟聚合量宣称单节点可执行，须验证成员节点放置条件 |
| TC-I07 | Liqo Offloading 撤销而 Pod/Node 缓存尚未刷新 | 停止受影响域的新承诺并重核授权；不继续沿用旧候选 |
| TC-I08 | Volcano Queue capability 仅余 2 卡而资源域物理还剩 8 卡，任务需 4 卡 | 因 Queue 额度不足等待，不借用未授权的 Queue 份额 |
| TC-I09 | Volcano Queue 有 deserved 与 guarantee，但其策略插件不一致或版本未知 | QUEUE_STATE_UNAVAILABLE；不能把不同份额语义都视为硬上限或设备锁 |
| TC-I10 | 公平保护在 Reserve 与 PreBind 之间发生变化 | 绑定前复核保护/预约版本；协议不再有效时受控撤销并重排 |
| TC-I11 | 账本故障，但 Node 空闲且 Score 值很高 | 停止 COORDINATED，不能由 Score 绕过权威预约 |
| TC-I12 | Workflow Scheduler 拒绝或不支持接入协议 | 不升级为 COORDINATED；如路径允许预测性放行须明确残余风险 |
| TC-I13 | Liqo/Volcano CR 更新，但没有关联的原生 Pod/Node 事件 | 必须有桥接或有界周期重评估，不可依赖不存在的自动唤醒 |
| TC-I14 | 应用的 PostFilter 具备可抢占行为，而本子系统目标不抢占 | 相应调度路径不允许通过抢占来满足 Queue/Reserve Node 的资源承诺 |

这些场景只用于验证**设计规则可推演**，不包含测试执行、上线或代码现状。它们应追踪 INV-I01～I06、ADR-01/04/07/08/10 及 SR-06～SR-09、SR-11、SR-12。

## 6 DFX 架构设计

### 6.1 正确性、安全性与租户隔离（QA-01、QA-06）

**威胁目标**：本子系统允许来自不同开源项目及 PR 的不可信作业竞争共享 NPU，但不允许它们自行决定租户身份、Queue 权限、作业优先级或可用远端资源范围。安全性仅覆盖**调度控制面与资源授权**：不以本设计承诺工作负载运行时的进程、文件系统或设备隔离，也不展开沙箱方案。所有强保证要求被分配端遵守相同的授权与预约协议。

#### 6.1.1 信任边界与攻击面

| 边界 | 不可信输入 | 可信责任方 | 设计约束 |
|---|---|---|---|
| GitHub Job/PR → ARC | PR 内容、workflow 参数、任意用户传入标签/环境变量 | 平台已配置的 ARC Runner 模板和作业来源策略 | 不从 PR 自声明中直接授予高优先级或更多 NPU |
| ARC → Runner Pod | labels、annotations、nodeSelector、schedulerName、NPU 需求字段 | 具备认证身份的 ARC 控制器及 Kubernetes Admission Policy | 关键字段必须校验来源和授权；普通 Namespace 用户不能篡改已认证需求 |
| Runner → Workflow Pod | 实际 Requests、拓扑、Queue、PodGroup、Workflow UID | 可信 hooks/工作负载适配器和最终调度器 | 消费者实际规格不得超出关联 Demand 的授权与保留范围 |
| Liqo virtual Node / Offloading | 远端映射、Node 名称、聚合容量、namespace 选择器 | Liqo 权威策略 + 成员集群的物理资源事实 | 虚拟容量只是一种视图，不代表租户被授权或拥有设备 |
| Volcano Queue | Queue annotation/名称、资源额度与使用量 | 平台 Queue 归属策略 + Volcano Queue 权威 | 不能通过更改 annotation 冒用别人的 Queue |
| Scheduler → 预约权威 | reservationID、writerEpoch、generation、domainID、节点向量 | 具备最小写权限的 Coordinator/有效调度实例 | 所有写入经授权、CAS 与代际保护；不能由普通租户创建硬承诺 |
| 诊断与可观测性 | 错误文本、业务 ID、候选域/其他租户信息 | 受控事件与指标出口 | 租户只能看到自身作业的必要原因；不暴露他人的 Pod 名称、卡量与身份 |

**身份策略**：tenantID、jobClass、Queue 身份、allowedDomains 和资源规格上限必须来自经认证的配置/策略事实。Pod label/annotation 可作为传输载体，但不是授权事实本身。任务来源、Runner UID、Workflow UID、Namespace 与真实资源请求相互核对；未认证或不匹配时返回 UNAUTHORIZED / WORKFLOW_SPEC_MISMATCH，禁止默认放行。

#### 6.1.2 权限模型和最小 RBAC 能力

| 组件主体 | 允许的资源动作（架构最小集合） | 禁止与隔离要求 |
|---|---|---|
| 需求校验/候选规划 | 读取授权范围内必要的 Pods、Nodes、Liqo Offloading、Queue、预约视图 | 不修改租户 Pod Spec、Secret；不获得用于绕过授权的 Queue 写权限 |
| Scheduler Framework 主体 | 按 Kubernetes Scheduler 需要读取 Pods/Nodes 并执行合法 Pod Binding/必要状态记录 | 不以 cluster-admin 为常态；跨 namespace 读取仅用于被授予的调度视图 |
| 预约 Coordinator | 对限定域的 AccountingDomain/Reservation 对象执行必要的 get/list/watch/create/update/patch；执行受版本检查的变更 | 普通 Runner、Workflow ServiceAccount 无权直接写账本或修改 writerEpoch |
| Fairness Policy | 读取 Queue/租户策略，写入受域版本保护的 arrivalSequence、PolicyEpoch 和保护状态 | 不允许用户通过 CR 字段直接抬高自己的历史顺序、租户权重或优先级 |
| Workflow 关联适配器 | 写入经过授权的自身关联引用与交接证明 | 无权更改别人的 DemandID、reservationID、物理容量或保护版本 |
| 诊断与监控 | 发布必要的聚合指标和按授权分发的 Job 事件 | 不读取/输出 Token、Secret、完整原始 Job 环境变量或跨租户敏感数据 |

**RBAC 边界**：Kubernetes 原生 Scheduler 需哪些权限由其自身 Profile/绑定机制决定；这里规定额外插件与预约状态访问的最小权限，不以“所有组件均可全局读写”作为简化前提。由于标准 RBAC 不能单靠 ResourceName 对所有 create/list 场景实现细粒度域隔离，跨域写入还必须由**准入校验、租户身份与域权威逻辑**强制约束；不能以 RBAC 已配置就推断业务级授权闭环。

#### 6.1.3 防篡改、重放和混淆身份

1. **不可变可信关联**：初始受信任需求与任务/Runner identity 一起生成；任何后续规格变更须产生明确新版本与再准入。允许用户变更的普通标签不会覆盖权威需求字段。
2. **请求级检查**：在 Demand Validation、域级 Commit、Workflow ConfirmHandoff 三处重新验证身份及必要授权；只在入口查一次不够，尤其要防止权限撤销、过期 PolicyEpoch 后旧预约继续新增强承诺。
3. **防重放**：对 DemandID+JobAttempt+generation 做幂等；writerEpoch、evidenceVersion 防止旧实例或旧 Workflow 证据覆盖新决定；过期消息不能创建新的逻辑容量。
4. **节点与型号防混淆**：物理资源身份由可信 Node/设备事实提供；tenant 自定义 Node 标签、Liqo 镜像名和自由文本型号不能形成独立容量。
5. **Queue/优先级防提升**：nightly/PR 分类、Queue 权重和资源域授权仅由可信调度策略映射产生。来源不可靠时按授权规则拒绝或进入有证据的等待状态，不默认授予高优先级。
6. **秘密管理**：控制面调用 Kubernetes API 使用运行主体可撤销的身份/凭据；不在共享 Pod label/annotation、公开事件或指标里携带 bearer token、云平台访问凭据。令牌轮换和失效不能允许旧主体继续变更预约状态。

#### 6.1.4 跨租户的公平与容量安全

- **租户作用域**：按受信任的 tenantID、Queue、Namespace 和 clusterID 关联公平统计、配额及预约。不同 Namespace 同名 Job 不得重用别人的 reservationID。
- **资源共享条件**：多租户任务可以共享同一个物理 AccountingDomain；域账本保证所有合法预约受相同容量约束，但租户可见的诊断结果按授权过滤。
- **资源配额**：硬额度的适用范围可能跨多个 AccountingDomain，必须由可共享的权威扣减；同域卡数充足不意味着该租户 Queue 仍有额度。
- **权限撤销**：Queue、Offloading 或租户授权变化后，未提交的新请求立即按新策略阻止；已有 HELD/ALLOCATED 的撤销遵循 5.2 的可信交接与释放证据，不能以安全事件为借口提前重新承诺卡数。
- **抗拒绝服务**：单租户大量非法需求在执行昂贵候选求解前接受身份校验与速率限制；处理预算按域、租户保持有界，避免高频 PR 请求饿死其他合法租户。

#### 6.1.5 安全失败语义

| 威胁/异常 | 目标响应 | 禁止的行为 |
|---|---|---|
| 伪造 NPU count/Queue/jobClass | UNAUTHORIZED 或 INVALID_DEMAND；不进入公平候选 | 信任任意 Pod annotations |
| 未经授权修改调度策略 | 保持最后可信的有效策略版本；对相关域暂停新强授权 | 由修改者自行抬高优先级或延长 Reserve Node 保护 |
| 伪造 Workflow UID/交接凭据 | RESERVATION_HANDOFF_MISMATCH；进入受控对账 | 用名称相似/标签相同直接释放预约 |
| 访问别人的 reservationID | 鉴权拒绝；诊断不披露对方容量和身份 | 返回跨租户详细资源记录 |
| 过期 writerEpoch / 重放 generation | FENCED_OUT 或 RESERVATION_IDENTITY_CONFLICT | 允许旧主体撤销新实例合法承诺 |
| 授权/Queue 权威暂时不可读取 | D3 DATA_UNAVAILABLE/QUEUE_STATE_UNAVAILABLE | fail-open 或以缓存推断新资源授权 |
| 远端只提供不可验证聚合资源 | REMOTE_TOPOLOGY_UNKNOWN；无法建立强保证 | 把远端聚合卡数当作本域真实物理卡数 |

#### 6.1.6 安全不变量与设计场景

**INV-S01**：租户/Queue/JobClass 的有效授权只能来自受信任策略；自填 label/annotation 不得提高额度或优先级。

**INV-S02**：Demand、Runner、Workflow 和预约的身份关系必须包含明确的 Cluster/Namespace/UID/JobAttempt 与代际；跨租户或重放事件不能占用、修改或释放其他人的承诺。

**INV-S03**：预约权威和 Fairness 状态只能由受控的组件主体在授权范围内写入；RBAC、准入规则和业务授权同时生效。

**INV-S04**：未验证的 Liqo 映射、Queue 额度或设备型号不能代替物理资源事实；在授权和配额关键事实失效时拒绝新增强承诺。

**INV-S05**：普通租户诊断内容不披露别人的 UID、需求、Queue 用量、资源域详情或凭据；高基数业务关联只流经权限控制的追踪通道。

**INV-S06**：来源不可信的任务可在授权范围内参与调度，但不能通过密集重试或自声明时间/优先级突破资源和公平保护；本章不承诺工作负载内核/设备隔离。

| TC | 设计攻击/故障场景 | 应满足的架构约束 |
|---|---|---|
| TC-S01 | 非可信 PR 修改 Pod annotations 要求 16 卡、PR 最高优先级 | 规格超授权/类别不可信时拒绝，不改变其它租户的容量 |
| TC-S02 | 普通租户修改 Pod 的 Queue annotation 指向高额度 Queue | Queue 权限按可信身份独立核验，拒绝冒用 |
| TC-S03 | 两个 Namespace 复用相同 Pod 名称和 GitHub Job 名称 | 预约/Workflow 交接不跨 Namespace 混淆 |
| TC-S04 | 旧 Job attempt 重放有效 reservationID | generation/JobAttempt 检查拒绝更新新预约 |
| TC-S05 | 不可信 Workflow 伪造已分配 Node 和对应物理卡数 | 仅信任实际调度与 Kubernetes 分配证据，不解除原预约 |
| TC-S06 | Liqo 远端虚拟节点重复映射本地卡 | 基于物理资源身份去重，禁止重复容量 |
| TC-S07 | 普通 Runner SA 请求直接创建/更新 AccountingDomain | RBAC 与授权拒绝，不得写入承诺 |
| TC-S08 | 用户合法但持续提交上千条无法执行的多节点请求 | 身份/额度预检查、限制规划预算，不拖垮其它租户 |
| TC-S09 | 用户要求读取另一租户的等待原因与节点型号分布 | 只返回自身关联的必要结果，不泄漏全域明细 |
| TC-S10 | 认证策略撤销 Queue 使用权限，旧 HELD 尚未完成 Workflow 交接 | 新授权停止；旧资源安全撤销，不提前重新承诺 |
| TC-S11 | Workflow 交接凭据超过有效期或 writerEpoch 已切换 | 拒绝旧证据，不提升 COORDINATED |
| TC-S12 | 控制面日志和指标包含 OAuth Token/Secret | 设计禁止敏感值入事件/指标，控制面只记录可关联标识 |

这些是安全性**设计验收场景**，不涉及对实际不可信代码的执行或沙箱方案。

### 6.2 性能与容量（QA-03）

本节规定**目标架构设计预算、容量包络和退化行为**。数值均为设计目标，而非现网规模、实测性能或交付结论。不得为了满足预算牺牲 NPU 可行性、授权、域级 CAS、公平顺序与 Reserve Node 保护。

#### 6.2.1 性能指标和计量边界

| 指标 | 计时边界 | 统计要求 |
|---|---|---|
| PolicyDecisionLatency | 单次需求校验、可信资源候选求解与公平判定的计算耗时；排除外部 API 往返 | P50/P95/P99，按单节点/多节点复杂度区分 |
| ReservationCommitLatency | 提交一次 AccountingDomain 条件更新到获得成功/冲突/失败响应 | 区分 CAS 冲突、Kubernetes APF/429、网络/API 故障 |
| AdmissionOverhead | Runner 一次调度尝试产生的准入开销，含版本检查和有界 CAS 重试，不含等待资源可用的时间 | 不与 CI Provider 接单或 Workflow 实际排队混合 |
| AdmissionThroughput | 每秒完成的有效准入决定数，包含成功、资源等待和授权拒绝 | 按正常竞争和写热点分别统计 |
| ReevaluationLatency | 已接收权威资源或策略变化后，相关可执行队首再次获得判定的时间 | 不计入外部尚未传达事件的时间 |
| DecisionWork / CASConflictRate | 决策访问的 Node、Queue、预约索引数量及 CAS 冲突比率 | 衡量是否随全局 Pod/Queue 规模线性退化 |

**统计窗口：固定连续 5 分钟。** 每个有效窗口使用 P50/P95/P99、吞吐与样本量，正常竞争和外部故障分开统计；没有样本不能认定达标。诊断使用低基数资源域/任务类型维度，DemandID、Runner UID 不作为高基数时序标签（见 6.4）。

#### 6.2.2 规划容量包络和性能预算

选择 **S1** 作为目标设计基准负载，不代表现网统计值：N（物理 Node）=256、P（相关 Pod）=20,000、R（有效预约）=2,000、Q（待决需求）=5,000、C（资源域）=32。为避免单对象无限膨胀，每个 AccountingDomain 规划不超过 **32 个节点及 256 个活动预约索引**；跨分区需求仍须满足 5.2.1、5.2.4 的原子性约束。

| 预算 | S1 正常依赖条件下的设计目标 | 边界 |
|---|---|---|
| PB-01 计算延迟 | PolicyDecisionLatency P95 ≤100 ms，P99 ≤250 ms | 含有界候选搜索；排除 Kubernetes API 时延 |
| PB-02 预约写入 | 单次 ReservationCommitLatency P99 ≤500 ms | API 正常可用；429 与限流须另分类 |
| PB-03 准入延迟 | **首次无冲突** AdmissionOverhead P99 ≤1 s；单次调度尝试最多两次立即 CAS | CAS 冲突重试单独计量；不能由 PB-02 的两次各 500 ms 上限推导整体仍在 1 s 内；超预算返回队列 |
| PB-04 决策吞吐 | ≥10 次准入决策/s/调度实例 | 输入供给与 API 服务能力匹配，非持续单域写热点 |
| PB-05 重新评估 | ReevaluationLatency P99 ≤5 s | 资源/策略变化事件已经到达子系统、相关队首具备重评估条件 |
| PB-06 复杂度 | 正常单次在线调度**零次全量 Pod List、零次全局等待队列扫描** | 初始化和故障恢复独立于在线热路径 |
| PB-07 候选搜索 | 一次 CandidatePlan 规划计算预算 ≤100 ms | 无法形成结论时返回不确定等待，而非误报不可执行 |

这些预算是明确、可修订的架构约束。必须同时满足正确性与公平不变量。超过 S1 包络应进行容量分区或明确报告超出设计容量，不能隐式降低保证级别。端到端 CI 等待还包括真实 NPU 资源占用和外部 CI Provider 时间，不能从 PB-03 推断 CI Job 只需 1 s 排队。

#### 6.2.3 关键路径与复杂度

定义 D 为当前授权候选域涉及的节点数，Qc 为本次竞争队列长度，H 为受保护预留数，K 为拓扑约束检查成本。在线决策应满足：

| 环节 | 目标复杂度 | 避免的风险 |
|---|---|---|
| Demand/授权校验 | 按可信身份与策略键索引，O(1) 查找另加输入解析成本 | 每次遍历全部租户策略 |
| ResourceSnapshot 与空闲量 | O(D) 或 O(D log N)，按 Node/型号/域增量索引 | 每轮 O(N+P+R) 重新汇总所有 Pod 和预约 |
| 单节点 CandidatePlan | O(D·K) 的有界匹配 | 全集群硬件扫描与重复解析拓扑 |
| 多节点 CandidatePlan | 有界搜索，预算 PB-07；超限返回 UNKNOWN | 组合爆炸导致调度线程长期阻塞 |
| 租户/FIFO 公平裁决 | 目标 O(log Qc+H)，保护集合 H 有上限 | 每个 Pod 扫描全部 Pending Runner |
| Backfill 冲突判定 | O(H·D) 上界，逐个维持保护约束 | 只检查一个队首、遗漏其他有效保护 |
| AccountingDomain 预约 | 域级一次条件写入；与**有界分区**规模相关 | 对象随全局 R 永久膨胀、写热点集中 |
| 队首重新评估 | 定向重评估 affectedPools；索引检索目标 O(log Qc) | 一个外部 CR 更新唤醒全体 Q |

单节点规划可直接判断有无有效 Node。多节点拓扑组合可能非多项式可解：**搜索超预算不是永久不可执行的证明**。无法得到肯定或否定证明时，返回 FEASIBILITY_BUDGET_EXCEEDED，保留等待和后续更充分评估机会，不返回 INFEASIBLE 或伪造可行 CandidatePlan。

#### 6.2.4 有界资源域与并发扩展

第 5.2 节定义的域级 AccountingDomain CAS 保证逻辑预约的一致性；如果每个域包含无界活动预约，会导致对象大小、事件广播、CAS 竞争和 API 写延迟随 R 增加。**必须同时限制单域规模与物理容量边界。**

1. **物理不重叠分区**：按真实 NPU Node/资源身份及拓扑约束划分 AccountingDomain。一个物理设备不能被两个独立 CAS 域重复承诺；虚拟 Node 映射必须指向同一事实权威。
2. **有界账本**：每个原子域容量按 S1 的 32 节点/256 活动预约上限约束；已关闭的历史记录独立保留审计事实，不进入热路径活动索引。必须保留足够的代际/去重信息以拒绝迟到重复 Commit，不因关闭归档导致同一旧任务再次取得承诺。
3. **多节点原子性**：同域 CandidatePlan 由同一次域级 CAS 整体成功或失败；跨独立域的 Gang 只有在定义跨域整体提交/回滚协议后才可宣称 COORDINATED，否则返回 CROSS_DOMAIN_ATOMICITY_UNAVAILABLE，不允许逐域 HELD 后冒充完整预约。
4. **有界冲突重试**：正常调度尝试最多两次立即提交；CAS 冲突重新计算 CandidatePlan、公平 PolicyEpoch 和 protectionVersion；冲突持续时按资源域限流及退避，不能造成 API 重试风暴。
5. **配额原子性**：跨域共享的硬 Queue 配额必须有相同强度的全局额度仲裁；否则只可提供相应协议边界的预测性结果，不能说局部分区合计一定不超配。

分区降低热点，但引入跨域协调成本；此取舍不能通过弱化 INV-R01/02、INV-F01/05 来隐藏。达到上限且不能安全分区时，该域停止新强承诺，独立可信域仍可继续。

#### 6.2.5 缓存、重评估和流控

- Node/Pod/Liqo/Volcano/预约资源事实按 informer/watch 增量维护；按物理资源身份、Node、Queue、domainID 与 FairnessPool 建反向索引。一次资源变化只重评估受影响的需求，而非全队列遍历。
- 缓存键覆盖资源型号、可信授权、拓扑、policyVersion、snapshotVersion 和有效期；脏/过期证据可以用于缩小候选或触发重新计算，不可用于提升 COORDINATED。
- 合并同域重复事件、按域唤醒可执行队首；涉及权限、writerEpoch 或预约生命周期的版本变化不能被错误丢弃。QueueSort 的本地队列排序不代替持久化 arrivalSequence。
- 预约/API 写入应在受控并发与域级有界重试下执行。Kubernetes API Priority and Fairness（APF）的等待与 HTTP 429 是外部依赖压力信号，不应重试无限次，不得被解释成 NPU 容量不足。
- 资源事实更新优先以 watch 进入内存视图；避免在 PreFilter/Filter 热路径实时 List 全部 Pod。异常恢复可重新构建索引，但恢复范围和容量承诺须受权威版本保护。

#### 6.2.6 过载及降级规则

| 触发 | 性能保护 | 正确性边界 |
|---|---|---|
| 复杂 Gang 拓扑预算耗尽 | 返回 FEASIBILITY_BUDGET_EXCEEDED 并按需求变化重评估 | 不误报 INFEASIBLE，不强行估算可执行 |
| Q 增加、队首频繁变化 | 稳定 Queue 索引、定向重评估、重复事件合并 | 不重置 FIFO 顺序、租户欠账或保护承诺 |
| 单域 CAS 热点 | 并发配额、两次立即重试、域级退避 | 不放松 CAS 或跨域重复承诺 |
| 单域活动记录达到有界上限 | 如物理不重叠可分区；否则等待域容量释放 | 不生成无限增长的单个 API 对象 |
| API 429 或长尾请求 | 有界退避、缓存只读、保留有效预约 | 不能跳过授权或权威写入提升保证 |
| Liqo 远端中断 | 隔离失效域，按独立可信域重算 | 不合并虚拟容量填补真实物理缺口 |
| 大量 Workflow 交接积压 | 按 AccountingDomain 与证据版本分批对账 | 未确认分配不能提前释放逻辑承诺 |

#### 6.2.7 性能 ADR 和不变量

**ADR-12：有界容量仲裁与增量索引。** 选择物理不重叠、规模有界的 AccountingDomain 与增量资源/队列索引，并对候选搜索、CAS 冲突和事件重评估设置工作量上限；以降低单个写热点与在线全量扫描。代价是跨分区 Gang、全局 Queue 强配额需要额外原子协议，缺失时必须明确返回未协调状态。

**INV-P01**：在线正常调度不全量扫描全部 Pod、Pending Runner 或历史预约；只查询相关索引和候选域。

**INV-P02**：算法开销、API 延迟、资源等待、ARC 接单耗时分开计量，不以等候 NPU 的时间评价调度计算性能。

**INV-P03**：分区不导致同一物理资源重复承诺，跨域 Gang 不进行伪原子部分提交。

**INV-P04**：搜索超预算不改变可行性结论，CAS 冲突耗尽不能绕过预约权威及公平许可。

**INV-P05**：API 限流与事件风暴不得重置队列顺序、撤销有效保护或导致重试风暴。

**INV-P06**：PB 指标以固定窗口、S1 包络、依赖类别及样本量判定；容量超出或无样本不能伪称达标。

#### 6.2.8 性能设计场景

| TC | 输入/压力情景 | 可判定的设计结论 |
|---|---|---|
| TC-P01 | S1，Node/Pod/Queue 持续变化 | PB-01：决策计算 P95/P99 ≤100/250 ms |
| TC-P02 | S1，两实例对同域并发提交 | PB-02：一次 CAS P99 ≤500 ms；冲突者不重复承诺 |
| TC-P03 | S1，正常 API、无 CAS 冲突与非热点负载 | PB-03/04：首次准入 P99 ≤1 s、持续 ≥10 次决策/s/实例；冲突路径另计 |
| TC-P04 | 队首等待 NPU 释放，事件已到达 | PB-05：重评估 P99 ≤5 s |
| TC-P05 | Pod 从 1,000 增至 20,000 | 不随 P 增加执行每次全量 Pod 计量 |
| TC-P06 | Queue 从 100 增至 5,000 | 局部队首判定不按全局 Q 遍历 |
| TC-P07 | 单域活动预约达到 256 | 不无限扩展单个 CAS 对象，不复制物理容量 |
| TC-P08 | CandidatePlan 跨两个独立 AccountingDomain | 无跨域原子协议则不能宣称整体 COORDINATED |
| TC-P09 | 多节点组合求解超过 100 ms | 返回 FEASIBILITY_BUDGET_EXCEEDED；不伪造 INFEASIBLE |
| TC-P10 | API 返回 429、CAS 冲突激增 | 有界重试/退避，保持公平位置与有效保护 |
| TC-P11 | 10,000 条事件只影响一个域 | 合并事件、只唤醒受影响的可执行队首 |
| TC-P12 | PR/Nightly 时间窗切换且 Q 高 | 保护版本仍受 CAS 约束，事件不会全池失控 |
| TC-P13 | 两个 Liqo 虚拟 Node 映射相同物理卡 | 分区去重，容量不会因虚拟视图数量增加 |
| TC-P14 | 资源事实过期、但性能预算即将超时 | 不为达到 PB 目标使用旧事实提升保证 |

上述是**可验证的目标设计**，不是性能测试执行报告。若指标未达到预算，需要按计算、API、CAS、缓存、FairnessPool 等不同责任域分析，而非削弱正确性以提高吞吐。

### 6.3 可靠性与可用性（QA-04、QA-05）

**可靠性目标**：调度进程、Kubernetes API、Liqo、Volcano 或 Workflow Scheduler 发生故障时，不重复承诺同一 NPU 容量、不错误归还可能被占用的资源、不丢失有效公平顺序，并在事实恢复后使符合条件的排队任务重新获得调度机会。**可用性不能以降低授权和一致性校验为代价。**

#### 6.3.1 故障域和失效隔离

| 故障域 | 失效信号及影响面 | 子系统的设计行为 |
|---|---|---|
| 调度实例进程 | 进程退出、过期 leader 持有本地缓存、未完成 Reserve/Bind | 由持久权威重建；writerEpoch 阻止旧实例继续写入；Runner 与预约交接由对账恢复 |
| Kubernetes API / etcd | API 超时、429、资源版本冲突、控制面不可读/写 | 不做新的 COORDINATED 承诺；已有承诺保留；失效域隔离，等待 API 权威恢复 |
| Node / Device Plugin | Node NotReady、Allocatable 降低、设备型号事实丢失 | 隔离受影响节点，重算可执行计划；未知已占用容量不按零计入 |
| Liqo 远端资源域 | Offloading 授权失效、虚拟 Node 映射变化、远端断联 | 仅受影响域等待/撤销核验；本地独立可信域不因远端故障停止 |
| Volcano Queue | Queue 删除、配额版本失效、所需调度策略不可确认 | 对受影响 tenant/Queue 不绕过配额；不影响不共享配额的独立池 |
| Workflow Scheduler | 分配不可见、PodGroup 部分完成、绑定/终止事件丢失 | 交接进入 UNCERTAIN；未证实已释放的物理容量继续保护 |
| 策略/公平权威 | PolicyEpoch 漂移、保护状态不可读、arrivalSequence 回退 | 停止涉及同池的新许可，保存旧到达顺序与 ReserveCommitment |
| 监控/日志后端 | 指标无法写入、事件采集迟到 | 维持正确性与准入处理，允许延后发布诊断，不生成第二套资源权威 |

故障域由真实共享依赖关系确定。两个 AccountingDomain 若共享同一个必须强一致更新的全局 Queue 额度或同一物理卡，就不属于可独立降级的域，不能把故障影响误标为“仅一个域”。

#### 6.3.2 主备、代际和原子恢复协议

~~~mermaid
sequenceDiagram
  participant O as 原有效调度实例
  participant API as Kubernetes API/预约权威
  participant N as 新有效调度实例
  participant W as Workflow Scheduler/Pod事实
  O->>API: 预约写入 (writerEpoch=e)
  Note over O: 故障或与API失联
  N->>API: 获取新有效代际 e+1
  API-->>N: writerEpoch=e+1及账本快照版本
  O->>API: 迟到的旧代际写入 (e)
  API-->>O: FENCED_OUT
  N->>W: 关联Runner/Workflow UID及分配事实
  W-->>N: 分配/释放证据
  N->>API: 对账、保留不确定额度、更新状态
  N->>API: 原子提交新版本的合法预约
~~~

**恢复顺序**：先通过 Kubernetes API 权威记录完成代际接管（不能仅依赖进程本地自认领导权），再重建 AccountingDomain 与 FairnessPool 的权威版本；第三步核查活跃 Runner、Workflow、物理资源映射和 HELD/HANDOFF_PENDING/UNCERTAIN 状态；第四步隔离证据不完整的容量；最后按域重新开放新预约及队首评估。

- **防脑裂**：租约 leader 选举可负责减少活跃实例，但真正的写入保护依赖与域级 CAS 共同验证的 writerEpoch；旧实例即使继续运行也必须被拒绝权威写入。
- **原子状态边界**：同一域内公平保护、节点向量和预约索引在可比较的一次条件更新中仲裁；对 Kubernetes Node/Pod 的跨对象快照不能宣称全局串行一致，须显式考虑事实时滞和下游参与边界。
- **事件幂等**：相同 reservationID/generation/evidenceVersion 的恢复、撤销和交接重复操作不改变已确认的新状态；延迟到达的旧证据不覆盖新证据。
- **崩溃空窗**：Reserve CAS 成功但 Runner 绑定结果未知时，不因 Scheduler 进程退出而释放 HELD；应根据 Runner UID/绑定及 Workflow 事实核对后转移。
- **容量下降**：已作出的逻辑承诺不可因为 Node Allocatable 降低而悄悄删除；标记受影响域容量债务，禁止新增不安全承诺并触发受控恢复。

#### 6.3.3 降级等级与允许的操作

| 等级 | 有效事实和依赖 | 允许的决策 | 禁止的行为 |
|---|---|---|---|
| **D0 正常** | 授权、物理资源、Queue/保护/预约权威及参与的下游协议可信 | ESTIMATED；满足协同时可 COORDINATED | 跨域超额预约 |
| **D1 局部域隔离** | 部分域失效，但其它域授权/预约/物理事实独立且可信 | 对可信域继续正常决策，失效域返回 REMOTE_DOMAIN_UNAVAILABLE 或 DATA_UNAVAILABLE | 将失败域容量挪至另一个域计算 |
| **D2 协调能力降级** | 真实拓扑和授权仍可信，但下游共享预约协议不可用 | 对明确允许机会性准入的请求只可输出 ESTIMATED；原协调承诺保持待核对 | 把预测性判断伪称为下游设备锁 |
| **D3 关键状态不可用** | AccountingDomain 版本/代际、Queue 硬额度、授权或真实物理可执行性不可验证 | 返回 RESERVATION_STATE_UNAVAILABLE / QUEUE_STATE_UNAVAILABLE / DATA_UNAVAILABLE，保存既有权利 | 绕过权威写入、默认为零占用或删除未知预约 |
| **D4 恢复对账** | 控制面恢复，但存在 orphan/UNCERTAIN、事件缺口或旧版本冲突 | 针对可信且已核完的子域逐步恢复；未核完部分继续隔离 | 在对账未完成时整域无条件开放 |

D2 只适用于**下游尚不共享协议、但授权与可行性事实仍可独立验证**的路径；不得从 D3 无授权/过期事实直接退化为 ESTIMATED。D4 要求按资源域与证据版本收敛，不能用“等待固定时长后删除”假装恢复完成。

#### 6.3.4 设计目标、保护时间与恢复边界

本节给出可评审的**设计目标**，不把这些数值称作生产环境 SLO 或实测值。

| 目标 | 设计预算 | 生效边界 |
|---|---|---|
| RA-01 有效代际排他 | 同一域任一时刻至多一个有效写入代际；旧代际写入拒绝率 100% | Kubernetes API 与仲裁权威可用 |
| RA-02 故障隔离 | 独立资源域故障不导致其它不共享物理资源或硬配额的域拒绝服务 | 不存在共享硬依赖 |
| RA-03 有界重新唤醒 | 权威恢复、变化事件到达后，受影响队首在 PB-05 的 5 s P99 预算内获再次判定 | API 正常、S1 包络内；不含物理资源释放等待 |
| RA-04 状态恢复目标 | Scheduler 实例失效且 API/下游证据正常时，目标在 60 s 内完成可确认域的代际接管与可恢复预约对账 | UNCERTAIN 和失联域不强制在 60 s 内释放；60 s 是设计预算 |
| RA-05 预约资源安全 | 任何未确认释放的预约不提前归还；最终有可信释放证据后不无限期保持隔离 | 下游需能提供可验证证据 |
| RA-06 幂等与活性 | 同一需求/generation 的重复/乱序事件不会增加承诺，且恢复后可继续重试 | 按 5.2 状态机和版本契约 |

#### 6.3.5 可靠性不变量

**INV-A01**：旧 writerEpoch 不能覆盖较新的预约、保护或交接权威状态；实例重启不改变独立 Demand 的公平到达顺序。

**INV-A02**：Reserve/Bind 中任意点崩溃后，先从权威账本核对预约状态；不得以新的 Scheduler 内存为空作为释放证明。

**INV-A03**：资源域或共享硬额度的实际依赖关系决定故障影响范围；独立可信域可运行，不假定互相独立的假象。

**INV-A04**：强保证依赖缺失时不得新增 COORDINATED；授权和物理可行性仍可信且策略允许时才可提供 ESTIMATED。

**INV-A05**：释放资源必须有可信状态证据，租约 TTL 到期仅触发核查；不确定的物理占用在对账前不可重新承诺。

**INV-A06**：事件丢失后必须可通过权威状态的周期核对与版本推进收敛，不依赖 PostBind、Unreserve 或单个 Watch 事件必达。

#### 6.3.6 可靠性设计验收场景

| TC | 失效场景 | 目标设计结论 |
|---|---|---|
| TC-A01 | Scheduler 进程在域级 CAS 成功后、Runner Bind 前退出 | 账本保留 HELD；新实例核对 Runner/Workflow 后决定保留或撤销 |
| TC-A02 | 旧实例无法看到新 leader，持续以旧 writerEpoch 写入 | 权威拒绝旧代际写入，不双重承诺 |
| TC-A03 | Kubernetes API 不可用，节点 Snapshot 显示 8 卡空闲 | 停止新强预约，不用本地缓存直接放行 |
| TC-A04 | 仅 Liqo 远端 B 失联，本地 A 完全独立且可信 | A 继续调度；B 保守隔离、不重复计入 A |
| TC-A05 | 两个表面不同的池共享一个全局 Queue 硬额度 | 任一额度权威不可用时两个池均受影响，不虚称故障隔离 |
| TC-A06 | HELD 租约超时但下游 Workflow 状态无法读取 | 进入 REVOKING/UNCERTAIN；不得仅按时间释放 |
| TC-A07 | Workflow 已终止但分配释放事件丢失 | 周期对账最终获取证据、转 CLOSED 并唤醒队首 |
| TC-A08 | Node Allocatable 减少，已存在预约 | 冻结不安全的新承诺、记录容量债务；不删掉原承诺 |
| TC-A09 | 配额信息过期但节点空闲 | 不绕过 Queue 硬上限；独立可信池可继续 |
| TC-A10 | 只失去下游协调协议，仍有可信可行性快照 | 仅对允许的机会性路径降级 ESTIMATED，不能伪称设备锁 |
| TC-A11 | API 恢复后出现大量重复 Watch 事件 | 同 Demand/version 幂等，对账合并/限流、不改变已有公平位置 |
| TC-A12 | 活跃 Workflow UID 与 Runner DemandID 的关联矛盾 | 进入 UNCERTAIN、保留未知容量、拒绝自动交接 |
| TC-A13 | 新实例完成代际接管但部分远端仍未恢复 | 可核对域分批重新开放；UNCERTAIN 域不强行解冻 |
| TC-A14 | 新实例获取过期资源快照并尝试恢复旧预约 | 基于权威代际和分配证据重建，不依据旧快照恢复强承诺 |

场景是**架构可推导的验收规则**；不描述真实故障注入或上线状态。与 SR-04/05/11/12、ADR-05/07/08/09/12 及 QA-04/05 追踪一致。

### 6.4 可服务性与跨组件诊断（QA-07）

#### 6.4.1 事件与关联身份

诊断要回答：Runner 为什么等待、由哪个组件决定、等待发生在哪个资源域、是否取得预约、后续 Workflow 是否真正获得 NPU。只归因本子系统及**直接依赖的可信事实**；缺少外部信号时明确标为 UNKNOWN。

| 阶段 | 事实来源 | 必需事件与字段 |
|---|---|---|
| Demand 校验 | 可信 ARC 需求/Validator | DEMAND_RECEIVED、DEMAND_VALIDATED/REJECTED；DemandID、Runner UID、JobAttempt、policyVersion |
| CandidatePlan | 资源视图/Feasibility | CANDIDATE_EVALUATED；domainID、snapshotVersion、reasonCode、guaranteeLevel |
| 公平决策 | FairnessPolicy | FAIRNESS_WAIT/GRANTED；arrivalSequence、PolicyEpoch、protectionVersion |
| 预约仲裁 | ReservationCoordinator | RESERVATION_HELD/REVOKING/UNCERTAIN/CLOSED；reservationID、generation、writerEpoch、evidenceVersion |
| Runner 绑定 | Kubernetes Scheduler/Pod | RUNNER_BOUND；cluster/namespace/Runner UID、绑定版本 |
| Workflow 分配 | 实际 Workflow Scheduler/Pod | WORKFLOW_SEEN、ALLOCATION_CONFIRMED；Workflow UID、Node、Requests、分配证据版本 |
| 交接完成 | Reconciler | HANDOFF_COMPLETED；reservationID、已交接节点资源向量 |

所有事件都应带 sourceComponent、schemaVersion、decisionID、eventTime、observedTime 和版本。状态收敛以权威版本与合法状态机为准，不仅以时间戳排序。可信链为 JobAttempt → DemandID → Runner UID → DecisionID → ReservationID → Workflow UID/Node；**名称相似或时间接近不是身份证明**。

#### 6.4.2 等待及资源效能口径

| 指标 | 定义 | 责任边界 |
|---|---|---|
| RunnerQueueWait | Runner 进入 NPU Scheduler 排队到绑定成功的时间 | 不含 ARC Listener 接单前时间 |
| NPUAdmissionWait | 有效 Demand 进入准入等待至成功或终止 | 可拆为容量、公平、预约冲突、依赖异常 |
| FairnessBlockedTime | FAIRNESS_WAIT 为主要原因的区间之和 | 与同一时刻的容量等待不能双重加总 |
| CapacityBlockedTime | 物理容量/拓扑/Queue 不足的等待区间 | 永久 INFEASIBLE 不作为正常等待 |
| HandoffPendingTime | Runner 绑定到实际 Workflow 分配确认或结束 | 外部无可信阶段信息则 UNKNOWN |
| LogicalReservationCardHours | 有效未交接逻辑预约数量乘持续时间 | 不是实际设备进程运行卡时 |
| AllocatedNPUCardHours | 由可信 Pod/设备分配事实核算的卡时 | 不是 AI Core 计算利用率 |
| ProcessActiveNPUCardHours | NPU 上有进程运行的卡时，须由设备/进程监控直接提供 | 本调度器不从 Pod Requests 或预约推断 |

每个等待时间区间有唯一的**主原因**。Runner/Workflow 阶段可按时间拆分，但交叠区间不得简单累加。外部 CI Provider 或 Workflow Scheduler 未提供阶段证据时，仅报告本系统可观察范围内的时间与覆盖率，不能推断 GitHub Job 端到端等待。

#### 6.4.3 原因码与证据门槛

| 原因族 | 示例码 | 诊断输出 |
|---|---|---|
| DEMAND/SECURITY | DEMAND_UNAVAILABLE、INVALID_DEMAND、UNAUTHORIZED、WORKFLOW_SPEC_MISMATCH | 需求来源与认证策略版本 |
| CAPACITY | INFEASIBLE、INSUFFICIENT_CAPACITY、FEASIBILITY_BUDGET_EXCEEDED | 可行性结论、候选域、事实新鲜度 |
| FAIRNESS | FAIRNESS_WAIT、FAIRNESS_STATE_UNAVAILABLE | PolicyEpoch、保护版本、可重评估条件 |
| RESERVATION | RESERVATION_CONFLICT、RESERVATION_HANDOFF_MISMATCH、RESERVATION_RECOVERY_REQUIRED | reservationID、generation、状态与权威版本 |
| DEPENDENCY | QUEUE_STATE_UNAVAILABLE、REMOTE_DOMAIN_UNAVAILABLE、DOWNSTREAM_UNCOORDINATED | 责任组件、是否具备直接证据 |
| UNKNOWN | EVIDENCE_MISSING、EVENT_GAP、EXTERNAL_STAGE_UNKNOWN | 缺失的信号、覆盖率、下一步可核对来源 |

**最低诊断输出**：阶段、原因码、责任组件、起止时间/持续时间、数据和策略版本、关联 ID、保证级别、能否重试和重评估触发；不可信或不存在的原因必须 UNKNOWN。跨租户诊断只显示自身作业必要信息，不显示其他租户的 UID、队列额度和预约细节。

#### 6.4.4 指标、保留与失效

PB-01～PB-07 的决策耗时和 RA-01～RA-06 的可靠性事件以**5 分钟固定统计窗口**给出 P50/P95/P99、样本量和依赖异常分类。公平队首年龄、等待原因族、CAS 冲突、UNCERTAIN、交接超时、事件覆盖率均需可观察。

- 指标标签只使用低基数资源类别、域类别、状态和原因族；DemandID/JobAttempt/Pod UID 在受控日志和 Trace 关联，不进入高基数时序标签。
- 授权、预约状态转换和释放证据属于可信审计事件，不因性能 Trace 的采样而丢失其**权威记录**。异步事件缺口须通过版本化对账恢复，并显式标示 EVENT_GAP。
- Metrics/Trace 后端失效不阻断准入；但缺少证据时不得伪造完整性、声称下游已完成交接或把等待归责于某个外部组件。
- 一项诊断覆盖率的分母是本系统接收到的相关 Demand；外部 Job 信息缺失时不纳入“已完整观察”计数。

#### 6.4.5 不变量与设计场景

**INV-O01**：每个等待/拒绝决策有唯一主原因和证据版本，区分永久不可能、资源不足、公平等待与依赖异常。

**INV-O02**：跨阶段关联只使用可信 UID、DemandID、JobAttempt 和交接证据。

**INV-O03**：逻辑预约卡时、实际分配卡时及进程活跃卡时分别计量，不相互替代。

**INV-O04**：Provider 或下游独立 Scheduler 信号缺失时报告 UNKNOWN 及覆盖率，不臆测内部队列原因。

**INV-O05**：普通租户只获得自己作业的必要诊断；高基数标识不进入时序标签。

**INV-O06**：采样 Trace 与审计权威分离；事件缺口可通过版本化对账识别，不能用缺失事件推断资源已释放。

| TC | 设计场景 | 可判定结论 |
|---|---|---|
| TC-O01 | Runner 被 Queue 配额阻止，物理 NPU 仍空闲 | 记录 QUEUE_QUOTA_EXCEEDED，不报资源不可执行 |
| TC-O02 | 受保护队首阻止后继 Backfill | 记录 FAIRNESS_WAIT 与保护版本，不泄露别人身份 |
| TC-O03 | Workflow 被独立 Scheduler 排队且无其内部信号 | 下游原因 UNKNOWN，不猜测 Volcano 内部状态 |
| TC-O04 | Workflow 分配可见但交接事件缺失 | 以可信分配证据对账，保留 EVENT_GAP |
| TC-O05 | 两 Namespace 有同名 Runner | 不合并不同 UID/DemandID |
| TC-O06 | 逻辑预约 8 卡、设备真实分配 8 卡尚未完成交接 | 分开计量，不宣称实际消耗 16 卡 |
| TC-O07 | Metrics 后端不可用 | 不改变授权/预约正确性，显式反映诊断缺口 |
| TC-O08 | API 429 造成决策等待 | 记录 API 依赖原因，不误报 NPU 容量不足 |
| TC-O09 | GitHub 接单时间不可获取 | 不虚构 Job 总排队时间，仍能计算 RunnerQueueWait |
| TC-O10 | 大量 Workflow 缺少 UID 关联 | 关联覆盖率下降，不凭名称补全 |
| TC-O11 | PR/Nightly 分时窗口发生切换 | 诊断关联 PolicyEpoch 与等待原因 |
| TC-O12 | 预约超时但分配事实未知 | 状态为 UNCERTAIN，不能输出已释放的结论 |

上述均为目标设计判据，不是运行监控报表或测试执行记录。

### 6.5 FMEA：关键失效模式与残余风险

以下是**目标架构的风险分析**，不陈述历史故障、现网频率或测试执行状态。严重度 H 表示可能造成授权/容量正确性事故，M 表示公平/可用性退化，L 表示诊断质量退化。

| FMEA | 根因及后果 | 严重度 | 架构控制 | 可核验证据 | 特定残余风险 |
|---|---|---|---|---|---|
| FM-01 | 跨 Node/域累加空闲卡数，误判单节点需求可执行 | H | WorkflowShape、CandidatePlan 硬约束 | TC-C01～C03、TC-I06 | 远端物理拓扑变化仍可使 ESTIMATED 候选失效 |
| FM-02 | Liqo 虚拟节点重复映射物理卡，造成双重容量 | H | physicalResourceIdentity 去重与域隔离 | TC-C07、TC-S06、TC-A04 | 成员资源身份不可核实时必须禁用该域强保证 |
| FM-03 | 预约独立更新、同一域并发超卖 | H | 域级 CAS 和 Node 向量一次原子更新 | TC-R01/R02、TC-P02 | 跨域全局硬配额需额外一致性协议 |
| FM-04 | Reserve 后崩溃，Runner 状态未知而预约悬挂 | M | 持久状态与对账，禁止 TTL 直接释放 | TC-R04/R05、TC-A01 | 下游一直不可查询时部分容量持续隔离 |
| FM-05 | Workflow 交接前释放逻辑预约 | H | UID/Node/Requests 可信证据、无交接空窗 | TC-R06/R10、TC-O06 | 分配事实滞后会导致短期保守双计 |
| FM-06 | 旧 Scheduler 故障转移后继续写入 | H | writerEpoch + CAS 与恢复屏障 | TC-R08、TC-A02 | API 无法提供权威写入时强预约暂不可用 |
| FM-07 | PR/Nightly 策略切换破坏既有队首保护 | M | arrivalSequence、PolicyEpoch、protectionVersion | TC-F06/F07/F13 | 物理容量长期不足时不保证有限等待 |
| FM-08 | Backfill 根据历史运行 P90 误判释放时间 | M | 空间回填优先，时间回填强制时长上限 | TC-F03/F05/F14 | 最坏清理和传播时间必须纳入安全裕量 |
| FM-09 | 不可信 PR 伪造 Queue/规格/优先级 | H | 授权策略、可信身份、最小权限 | TC-S01/S02/S07 | 上游可信身份服务故障会阻止新强授权 |
| FM-10 | Volcano capability/deserved/guarantee 混用 | H | 按 Queue 策略和作用域核验硬额度 | TC-I08/I09、TC-A05 | 不支持共享配额协议时无法跨域强承诺 |
| FM-11 | Watch 事件丢失，资源释放后未重新唤醒 | M | 事件桥接与有界补偿对账 | TC-I13、TC-A07、TC-O04 | API 长期限流会延迟重评估 |
| FM-12 | 大规模 Q/P/R 导致全量扫描和 CAS 热点 | M | PB 预算、索引、域上限与有界退避 | TC-P05～P11 | 跨分区 Gang 暂不能强保证整体原子性 |
| FM-13 | 用 Pod 名称猜测 Workflow，误交接其它租户 | H | UID/JobAttempt/证据版本一致性校验 | TC-S03/S05、TC-O05 | 外部不提供 UID 时诊断覆盖率降低 |
| FM-14 | 诊断系统故障被当作调度故障 | L | 决策/诊断分离，标记证据缺口 | TC-O07、TC-A11 | 部分非关键 Trace 可能不完整 |

**FMEA 处理准则**：H 风险在证据不足时不新增强承诺，M 风险保留公平/预约权威并允许局部降级，L 风险不阻断合法调度。残余风险按上述具体条件保留，不用笼统的“加强监控”代替。

## 7 需求与设计追踪

### 7.1 IR/SR/ADR/DFX/TC 追踪矩阵

| SR | 主要设计 | ADR | QA | 设计证据 |
|---|---|---|---|---|
| SR-01 可信需求 | 4.1、5.1、5.6.2、6.1 | 02、10 | 01、06 | INV-S01～S04；TC-C10、TC-S01～S04 |
| SR-02 容量计量 | 4.2、4.7、5.1、5.2 | 03、04、05 | 01、04 | INV-C01～C05、INV-R02/R03；TC-C01～C09、TC-R01 |
| SR-03 可执行性 | 4.3～4.8、5.1、6.2.3 | 03、07、12 | 01、03 | TC-C01～C12、TC-I01/I03、TC-P09 |
| SR-04 预约 | 3.4、5.2、6.2.4、6.3 | 05、09、12 | 01、04、05 | INV-R01～R06、INV-A01～A06；TC-R01～R12、TC-A01～A14 |
| SR-05 交接 | 5.2.3、5.2.5～5.2.8、6.4 | 05、07、11 | 01、04、07 | TC-R04～R12、TC-O03～O06 |
| SR-06 Liqo | 4.2、4.5、5.3、5.6.4 | 02、04 | 01、06 | INV-I03；TC-C07/C08、TC-I06/I07 |
| SR-07 资源域 | 4.3～4.7、5.3、5.6.4 | 03、04、07 | 01、03 | TC-C01～C04、TC-I04/I06 |
| SR-08 Queue | 4.5、5.3、5.6.5、6.1 | 04、10 | 01、06 | INV-I04、INV-S01；TC-C09、TC-I08/I09、TC-S02 |
| SR-09 Filter/Score | 4.4、5.3、5.6.3 | 01、03 | 01、03 | INV-I02；TC-C04、TC-I01～I05 |
| SR-10 公平/Backfill | 5.4、6.2 | 05、06、12 | 02、03、04 | INV-F01～F06；TC-F01～F14、TC-P06/P12 |
| SR-11 事件 | 5.5、5.6.6、6.2.5、6.3、6.4 | 08、11、12 | 03～05、07 | INV-A06、INV-O06；TC-I13、TC-A07、TC-O04 |
| SR-12 运行诊断 | 5.5、5.6、6.1～6.4 | 07～12 | 04～07 | PB-01～PB-07、RA-01～RA-06；TC-A01～A14、TC-S01～S12、TC-O01～O12 |

表中两位数字是 ADR-XX、QA-XX 的简写，定义分别见 3.5 和 2.2。每个 SR 有设计章节、关键决策、质量属性及可判断场景。

### 7.2 DFX 反向追踪

| QA | 目标设计关注点 | 不变量及验收入口 |
|---|---|---|
| QA-01 正确性 | 物理拓扑可行、容量 CAS、交接无空窗 | INV-C/R/I、TC-C/R/I；FM-01～03、05 |
| QA-02 公平性 | 稳定 FIFO、PR/Nightly、Reserve Node 与安全 Backfill | INV-F、TC-F01～F14；FM-07～08 |
| QA-03 性能与容量 | S1、PB-01～PB-07、有界索引和仲裁 | INV-P、TC-P01～P14；FM-12 |
| QA-04 可靠性 | 代际隔离、幂等、对账和恢复 | INV-R/A、TC-R/A；FM-03～06、11 |
| QA-05 可用性 | 局部降级 D0～D4、RA-01～RA-06 | INV-A、TC-A01～A14；FM-04、06、11 |
| QA-06 安全性 | 可信授权、最小权限、跨租户隔离 | INV-S、TC-S01～S12；FM-09～10、13 |
| QA-07 可服务性 | 原因码、关联 UID、等待及卡时口径、覆盖率 | INV-O、TC-O01～O12；FM-11、13～14 |

### 7.3 场景索引与架构验收准则

| 场景组 | 章节 | 数量 | 设计关注点 |
|---|---|---:|---|
| TC-C01～C12 | 4.7 | 12 | 容量、拓扑、授权和准入反例 |
| TC-R01～R12 | 5.2.8 | 12 | 预约状态、幂等、交接 |
| TC-F01～F14 | 5.4.7 | 14 | 公平、优先级、保护和回填 |
| TC-I01～I14 | 5.6.8 | 14 | Framework 和外部接口 |
| TC-P01～P14 | 6.2.8 | 14 | 性能与容量包络 |
| TC-A01～A14 | 6.3.6 | 14 | 故障与恢复 |
| TC-S01～S12 | 6.1.6 | 12 | 身份、授权、租户隔离 |
| TC-O01～O12 | 6.4.5 | 12 | 可服务性、证据覆盖及计量 |

**共 104 个目标架构场景**，并非声称代码测试通过。每个场景必须可追踪到输入条件、规则/状态、责任组件和预期结论；无法推导清楚的场景属于待整改的设计问题。

## 8 技术规范参考

### 8.1 设计适用条件

本设计只保证目标架构定义的需求校验、可执行性判定、资源域公平、逻辑预约和跨阶段交接**契约**。若下游未采用共同的资源仲裁、配额与撤销协议，则只能声明 ESTIMATED，不能宣称物理 NPU 强预留。多节点跨独立 AccountingDomain 的整体强预约需要额外原子协议，未定义之前不得以多个局部 HELD 代替整体成功。

**非目标**：不设计 ARC Listener 接单机制、不代替成员集群的真实 NPU Node 调度、不定义运行时沙箱；不陈述代码完成度、部署计划、落地步骤或性能实测。章节中的性能与可靠性数值仅是目标设计预算，104 个 TC 为设计可判定场景，不代表执行测试已经通过。


- [Kubernetes Scheduling Framework](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/)：Scheduler 扩展点、调度/绑定周期、QueueSort、Reserve/Unreserve。
- [Kubernetes Running Multiple Schedulers](https://kubernetes.io/docs/concepts/scheduling-eviction/running-multiple-schedulers/)：多调度器间的节点和资源竞争边界。
- [GitHub ARC Runner Scale Sets](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/deploy-runner-scale-sets)：ARC Kubernetes Container Mode 与 runner-container-hooks 创建作业 Pod 的责任边界。
- [Liqo Offloading](https://docs.liqo.io/en/latest/features/offloading.html)：Namespace Offloading、远端虚拟节点及 Pod 选择策略。
- [Volcano Queue Resource Management](https://volcano.sh/docs/keyfeatures/queueresourcemanagement/)：capability、deserved、guarantee 的不同资源份额语义及 Queue 调度责任。
