package arcsync

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Name              = "ARCSync"
	RequiredNPUCount  = "ascend-ci.com/required-npu-count"
	ResourceDomain    = "ascend-ci.com/npu-resource-domain"
	ResourceModel     = "ascend-ci.com/npu-resource-model"
	AllocatedNPUCount = "ascend-ci.com/npu-count"
	stateKey          = Name + "/state"
)

type reservation struct {
	nodeName  string
	count     int64
	timestamp time.Time
	baseName  string
	namespace string
}

// drainTarget pins one node per scheduling pool whose free NPU is being
// accumulated for the pool head. Keyed by the head's UID so a new head
// triggers a fresh pick.
type drainTarget struct {
	headUID  types.UID
	nodeName string
}

type ARCSync struct {
	handle               framework.Handle
	podLister            corev1listers.PodLister
	inFlightReservations map[string]reservation
	poolDrainTargets     map[string]drainTarget
	mu                   sync.Mutex
	nsOffloadingLister   nsOffloadingLister
	queueLister          queueLister
}

type preFilterState struct {
	requiredCount int64
	resourceName  v1.ResourceName
	nodeFreeNPU   map[string]int64
}

func (s *preFilterState) Clone() framework.StateData {
	return s
}

var _ framework.PreFilterPlugin = &ARCSync{}
var _ framework.FilterPlugin = &ARCSync{}
var _ framework.ScorePlugin = &ARCSync{}
var _ framework.ReservePlugin = &ARCSync{}
var _ framework.PostBindPlugin = &ARCSync{}
var _ framework.EnqueueExtensions = &ARCSync{}

func New(ctx context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	pl := &ARCSync{
		handle:               h,
		podLister:            h.SharedInformerFactory().Core().V1().Pods().Lister(),
		inFlightReservations: make(map[string]reservation),
		poolDrainTargets:     make(map[string]drainTarget),
	}

	dynamicClient, err := dynamic.NewForConfig(h.KubeConfig())
	if err != nil {
		return pl, nil
	}

	dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 30*time.Second)

	var syncs []cache.InformerSynced

	nsOffloadingInformer := dynamicInformerFactory.ForResource(gvrNamespaceOffloading)
	if crdExists(dynamicClient, ctx, gvrNamespaceOffloading) {
		pl.nsOffloadingLister = &dynamicNSOffloadingLister{lister: nsOffloadingInformer.Lister()}
		go nsOffloadingInformer.Informer().Run(ctx.Done())
		syncs = append(syncs, nsOffloadingInformer.Informer().HasSynced)
	}

	queueInformer := dynamicInformerFactory.ForResource(gvrQueue)
	if crdExists(dynamicClient, ctx, gvrQueue) {
		pl.queueLister = &dynamicQueueLister{lister: queueInformer.Lister()}
		go queueInformer.Informer().Run(ctx.Done())
		syncs = append(syncs, queueInformer.Informer().HasSynced)
	}

	if len(syncs) > 0 {
		go func() {
			cache.WaitForCacheSync(ctx.Done(), syncs...)
		}()
	}

	return pl, nil
}

func crdExists(dc dynamic.Interface, ctx context.Context, gvr schema.GroupVersionResource) bool {
	_, err := dc.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if err == nil {
		return true
	}
	if apierrors.IsNotFound(err) {
		return false
	}
	return true
}

func (pl *ARCSync) Name() string {
	return Name
}

func (pl *ARCSync) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return []framework.ClusterEventWithHint{
		{Event: framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.Delete | framework.Update | framework.Add}},
		{Event: framework.ClusterEvent{Resource: framework.Node, ActionType: framework.Add | framework.Update}},
	}, nil
}

// canScheduleOnNode excludes cordoned nodes (Unschedulable: true) and any node
// carrying NoSchedule/NoExecute taints that the pod does not tolerate. Liqo
// virtual nodes ship with their own taints, which Liqo tolerates on offloaded
// pods, so those nodes remain eligible; a custom taint (e.g. 752t) that the pod
// does not tolerate correctly removes the node from the candidate set.
func canScheduleOnNode(pod *v1.Pod, node *v1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	return podToleratesNodeTaints(pod, node)
}

// podToleratesNodeTaints reports whether pod tolerates all NoSchedule/NoExecute
// taints on node. PreferNoSchedule is intentionally ignored: it is a soft
// preference, not a hard scheduling constraint (matching the built-in
// TaintToleration filter).
func podToleratesNodeTaints(pod *v1.Pod, node *v1.Node) bool {
	_, isUntolerated := corev1helpers.FindMatchingUntoleratedTaint(node.Spec.Taints, pod.Spec.Tolerations, func(t *v1.Taint) bool {
		return t.Effect == v1.TaintEffectNoSchedule || t.Effect == v1.TaintEffectNoExecute
	})
	return !isUntolerated
}

func nodeMatchesSelector(node *v1.Node, selector map[string]string) bool {
	for k, v := range selector {
		if node.Labels[k] != v {
			return false
		}
	}
	return true
}

func getBaseName(name string) string {
	suffix := "-workflow"
	if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
		return name[:len(name)-len(suffix)]
	}
	return name
}

// podSchedulesBefore orders pods by CreationTimestamp (UID tie-break).
// Using CreationTimestamp avoids the backoff side-effect where older
// (more-retried) pods accumulate longer backoff delays and get jumped by
// newer pods.
func podSchedulesBefore(a, b *v1.Pod) bool {
	at, bt := a.CreationTimestamp.Time, b.CreationTimestamp.Time
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return string(a.UID) < string(b.UID)
}

// olderPoolHead returns the pool head — the oldest unbound runner pod (same
// NPU type, same namespace, same scheduling pool) older than pod — or nil
// when pod itself is the head. The head schedules unconditionally (FIFO);
// younger pods may only backfill capacity that provably cannot delay the
// head (see backfillCheck).
//
// Only unbound pods (Spec.NodeName == "") are compared — pods already assigned to
// a node are past the scheduling decision and must not block new pods. Namespace
// isolation prevents cross-namespace blocking. Pool-based grouping (via
// npuFIFOPool when NamespaceOffloading is active, or nodeSelector comparison
// otherwise) ensures that runners targeting different scheduling pools do not
// block each other.
func (pl *ARCSync) olderPoolHead(pod *v1.Pod, nsHasOffloading bool) *v1.Pod {
	if pl.podLister == nil {
		return nil
	}
	resDomain := pod.Labels[ResourceDomain]
	resModel := pod.Labels[ResourceModel]

	allPods, err := pl.podLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "ARCSync: failed to list pods for FIFO check, failing open")
		return nil
	}

	myPool := npuFIFOPool(pod)

	var head *v1.Pod
	for _, p := range allPods {
		if p.UID == pod.UID {
			continue
		}
		if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
			continue
		}
		if p.Spec.NodeName != "" {
			continue
		}
		if p.Namespace != pod.Namespace {
			continue
		}
		if p.Labels[RequiredNPUCount] == "" {
			continue
		}
		if p.Labels[ResourceDomain] != resDomain || p.Labels[ResourceModel] != resModel {
			continue
		}
		if nsHasOffloading {
			if npuFIFOPool(p) != myPool {
				continue
			}
		} else {
			hasUnsharedConstraint := false
			for k, v := range p.Spec.NodeSelector {
				if pod.Spec.NodeSelector[k] != v {
					hasUnsharedConstraint = true
					break
				}
			}
			if hasUnsharedConstraint {
				continue
			}
		}
		if podSchedulesBefore(p, pod) && (head == nil || podSchedulesBefore(p, head)) {
			head = p
		}
	}
	return head
}

func npuFIFOPool(pod *v1.Pod) string {
	if v, ok := pod.Spec.NodeSelector["liqo.io/remote-cluster-id"]; ok {
		return v
	}
	return "local"
}

// fifoPoolKey identifies a scheduling pool for drain-target bookkeeping.
// Callers must derive the key from the pool head so all members of a pool
// agree on it. Without NamespaceOffloading, pools are nodeSelector
// equivalence classes, so the selector is folded into the key to keep
// distinct classes from sharing one drain target.
func fifoPoolKey(pod *v1.Pod, nsHasOffloading bool) string {
	key := pod.Namespace + "/" + pod.Labels[ResourceDomain] + "/" + pod.Labels[ResourceModel] + "/" + npuFIFOPool(pod)
	if !nsHasOffloading {
		keys := make([]string, 0, len(pod.Spec.NodeSelector))
		for k := range pod.Spec.NodeSelector {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			key += "|" + k + "=" + pod.Spec.NodeSelector[k]
		}
	}
	return key
}

// headCouldHostFree returns, for every node that could ever host the pool
// head's complete requirement (allocatable >= headReq, honoring the head's
// nodeSelector, tolerations and the namespace's virtual-node eligibility),
// that node's current free NPU. Free capacity on other nodes can never become
// a complete block for the head, so younger pods may consume it without
// delaying the head. The second return value is the head's total allocatable
// NPU across all its eligible nodes, used to detect heads that can never be
// satisfied at all.
func headCouldHostFree(head *v1.Pod, headReq int64, nodeInfos []*framework.NodeInfo, fullResourceName v1.ResourceName, nodeTotalOccupied map[string]int64, nsHasOffloading bool, nsOffloading *unstructured.Unstructured) (map[string]int64, int64) {
	var eligibleVirtuals map[string]bool
	if nsHasOffloading && nsOffloading != nil {
		eligibleVirtuals = getEligibleVirtualNodes(nodeInfos, nsOffloading)
	}
	couldHost := make(map[string]int64)
	var totalAllocatable int64
	for _, ni := range nodeInfos {
		node := ni.Node()
		if node == nil || !canScheduleOnNode(head, node) {
			continue
		}
		if !nodeMatchesSelector(node, head.Spec.NodeSelector) {
			continue
		}
		if isVirtualNode(node) && !eligibleVirtuals[node.Name] {
			continue
		}
		allocatable := node.Status.Allocatable[fullResourceName]
		totalAllocatable += allocatable.Value()
		if allocatable.Value() >= headReq {
			couldHost[node.Name] = allocatable.Value() - nodeTotalOccupied[node.Name]
		}
	}
	return couldHost, totalAllocatable
}

// updateDrainTarget pins one node per pool whose free NPU is left to
// accumulate for the pool head, and returns it ("" when the head has no
// eligible node). The pin is sticky for a given head: re-picking the argmax
// every cycle would drain several nodes at once and never finish any of them.
// It is re-picked only when the head changes or the pinned node stops being
// a candidate (cordoned, tainted, reconfigured).
func (pl *ARCSync) updateDrainTarget(key string, head *v1.Pod, couldHostFree map[string]int64) string {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.poolDrainTargets == nil {
		pl.poolDrainTargets = make(map[string]drainTarget)
	}
	if cur, ok := pl.poolDrainTargets[key]; ok && cur.headUID == head.UID {
		if _, stillCandidate := couldHostFree[cur.nodeName]; stillCandidate {
			return cur.nodeName
		}
	}
	best := ""
	var bestFree int64
	for name, free := range couldHostFree {
		if best == "" || free > bestFree || (free == bestFree && name < best) {
			best, bestFree = name, free
		}
	}
	if best == "" {
		delete(pl.poolDrainTargets, key)
		return ""
	}
	pl.poolDrainTargets[key] = drainTarget{headUID: head.UID, nodeName: best}
	klog.InfoS("ARCSync: pinned drain target for pool head",
		"pool", key, "head", head.Name, "node", best, "freeNPU", bestFree)
	return best
}

// backfillCheck relaxes the former strict-FIFO hold into EASY-style backfill:
// a younger pod may schedule ahead of the pool head as long as it provably
// does not delay the head. Two protections:
//
//  1. Aggregate headroom: after placing this pod, enough total free NPU must
//     remain for the head. When the pool is already below the head's need,
//     every card a younger pod takes pushes the head further out, so the pod
//     is held and capacity accumulates (cluster-level drain).
//  2. Drain-target protection: when the head's need is per-node (workflow
//     pods must fit on a single node) and fragmentation is what blocks it,
//     one sticky target node accumulates free NPU for it. Younger pods may
//     not consume that node unless it retains a full block for the head;
//     all other nodes stay open for backfill.
//
// Only the pool head is protected (EASY backfill): pod #3 may briefly delay
// pod #2, but every pod gains full protection once it becomes head, so a
// large job waits at most one drain ahead of its turn. A head that can never
// be satisfied (requirement above every eligible node's allocatable, or above
// the pool's total capacity) must not hold the pool hostage; backfill runs
// unrestricted and the head is left to operator intervention.
//
// Returns nil when the pod may proceed; nodeFreeNPU may have been pruned.
func (pl *ARCSync) backfillCheck(pod *v1.Pod, reqCount int64, head *v1.Pod, nsHasOffloading bool, nsOffloading *unstructured.Unstructured, nodeInfos []*framework.NodeInfo, fullResourceName v1.ResourceName, nodeTotalOccupied map[string]int64, nodeFreeNPU map[string]int64, totalNPUFree int64) *framework.Status {
	headReq, err := strconv.ParseInt(head.Labels[RequiredNPUCount], 10, 64)
	if err != nil || headReq <= 0 {
		return nil
	}

	headFree, headTotalAllocatable := headCouldHostFree(head, headReq, nodeInfos, fullResourceName, nodeTotalOccupied, nsHasOffloading, nsOffloading)
	impossible := headTotalAllocatable < headReq ||
		(podRequestsNPU(head, fullResourceName) && len(headFree) == 0)
	if impossible {
		klog.InfoS("ARCSync: pool head can never be satisfied, backfilling without hold",
			"pod", pod.Name, "head", head.Name, "headRequired", headReq)
		return nil
	}

	if totalNPUFree-reqCount < headReq {
		klog.V(4).InfoS("ARCSync: backfill hold — preserving aggregate NPU for pool head",
			"pod", pod.Name, "head", head.Name, "headRequired", headReq, "totalFree", totalNPUFree)
		return framework.NewStatus(framework.Unschedulable,
			fmt.Sprintf("backfill: preserving %d NPU for older pod %s", headReq, head.Name))
	}

	if target := pl.updateDrainTarget(fifoPoolKey(head, nsHasOffloading), head, headFree); target != "" {
		if free, ok := nodeFreeNPU[target]; ok && free-reqCount < headReq {
			delete(nodeFreeNPU, target)
		}
		if len(nodeFreeNPU) == 0 {
			return framework.NewStatus(framework.Unschedulable,
				fmt.Sprintf("backfill: node %s is draining for older pod %s", target, head.Name))
		}
	}
	return nil
}

func (pl *ARCSync) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	reqCountStr, ok := pod.Labels[RequiredNPUCount]
	if !ok {
		return nil, framework.NewStatus(framework.Success, "")
	}

	reqCount, _ := strconv.Atoi(reqCountStr)
	resDomain := pod.Labels[ResourceDomain]
	resModel := pod.Labels[ResourceModel]
	fullResourceName := v1.ResourceName(resDomain + "/" + resModel)

	nodeInfos, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, framework.NewStatus(framework.Error, "failed to get node snapshots: "+err.Error())
	}

	pl.mu.Lock()

	activeWorkflows := make(map[string]bool)
	knownPodUIDs := make(map[string]bool)
	nodePhysicalUsage := make(map[string]int64)
	nsLocalPhysicalUsage := make(map[string]int64)
	virtualNodes := make(map[string]bool)

	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil {
			continue
		}
		nodeName := node.Name
		var physUsage int64
		var nsUsage int64
		virt := isVirtualNode(node)
		if virt {
			virtualNodes[nodeName] = true
		}
		for _, podInfo := range nodeInfo.Pods {
			p := podInfo.Pod
			if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed || p.UID == pod.UID {
				continue
			}
			knownPodUIDs[string(p.UID)] = true
			baseName := getBaseName(p.Name)
			if p.Name != baseName {
				activeWorkflows[baseName] = true
			}
			if virt {
				continue
			}
			var podUsage int64
			for _, container := range p.Spec.Containers {
				if q, exists := container.Resources.Requests[fullResourceName]; exists {
					podUsage += q.Value()
				}
			}
			if val, exists := p.Labels[AllocatedNPUCount]; exists {
				if p.Labels[ResourceDomain] == resDomain && p.Labels[ResourceModel] == resModel {
					count, _ := strconv.ParseInt(val, 10, 64)
					if count > podUsage {
						podUsage = count
					}
				}
			}
			physUsage += podUsage
			if p.Namespace == pod.Namespace {
				nsUsage += podUsage
			}
		}
		if virt {
			physUsage = calcVirtualNodeOccupied(nodeInfo, resDomain, resModel)
		}
		nodePhysicalUsage[nodeName] = physUsage
		nsLocalPhysicalUsage[nodeName] = nsUsage
	}

	nodeTotalOccupied := make(map[string]int64)
	for nodeName, usage := range nodePhysicalUsage {
		nodeTotalOccupied[nodeName] = usage
	}

	now := time.Now()
	nsLocalReservated := make(map[string]int64)
	for uid, res := range pl.inFlightReservations {
		if !knownPodUIDs[uid] {
			if now.Sub(res.timestamp) > 10*time.Second {
				delete(pl.inFlightReservations, uid)
				continue
			}
		}
		if activeWorkflows[res.baseName] {
			continue
		}
		if virtualNodes[res.nodeName] {
			continue
		}
		nodeTotalOccupied[res.nodeName] += res.count
		if res.namespace == pod.Namespace {
			nsLocalReservated[res.nodeName] += res.count
		}
	}

	pl.mu.Unlock()

	nodeFreeNPU := make(map[string]int64)
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || !canScheduleOnNode(pod, node) {
			continue
		}
		if !nodeMatchesSelector(node, pod.Spec.NodeSelector) {
			continue
		}
		allocatable := node.Status.Allocatable[fullResourceName]
		free := allocatable.Value() - nodeTotalOccupied[node.Name]
		nodeFreeNPU[node.Name] = free
	}

	var nsOffloading *unstructured.Unstructured
	nsHasOffloading := false
	if pl.nsOffloadingLister != nil {
		nsOffloading, nsHasOffloading, _ = pl.nsOffloadingLister.Get(pod.Namespace)
	}

	if nsHasOffloading && nsOffloading != nil {
		nsLocalOccupied := make(map[string]int64)
		for nodeName, usage := range nsLocalPhysicalUsage {
			nsLocalOccupied[nodeName] = usage + nsLocalReservated[nodeName]
		}
		pl.applyLiqoComparison(nodeInfos, pod, resDomain, resModel, fullResourceName, nsLocalOccupied, nodeFreeNPU, nsOffloading, int64(reqCount), virtualNodes)

		if queueLimit, qFound := getQueueNpuLimit(pod, pl.queueLister, fullResourceName); qFound {
			var nsOccupied int64
			for _, usage := range nsLocalPhysicalUsage {
				nsOccupied += usage
			}
			for _, res := range nsLocalReservated {
				nsOccupied += res
			}
			if nsOccupied+int64(reqCount) > queueLimit {
				for nodeName := range nodeFreeNPU {
					if !virtualNodes[nodeName] {
						delete(nodeFreeNPU, nodeName)
					}
				}
				klog.InfoS("ARCSync: queue limit exceeded, removing local nodes",
					"pod", pod.Name, "queueLimit", queueLimit, "nsOccupied", nsOccupied, "required", reqCount)
			}
		}
	} else {
		for _, ni := range nodeInfos {
			node := ni.Node()
			if node != nil && isVirtualNode(node) {
				delete(nodeFreeNPU, node.Name)
			}
		}
	}

	// hasCandidate checks total NPU capacity across all local nodes
	// (regardless of the pod's nodeSelector) plus eligible virtual nodes.
	// Runner pods carry required-npu-count for reservation but may be bound
	// to CPU nodes by the default scheduler — the Reserve plugin stores the
	// reservation on the bound (CPU) node, not the NPU node. By summing
	// (allocatable - nodeTotalOccupied) across ALL nodes, reservations on
	// CPU nodes appear as negative values (0 − reservation) and correctly
	// reduce the cluster-wide total, preventing over-commitment.
	var totalNPUFree int64
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || !canScheduleOnNode(pod, node) {
			continue
		}
		if isVirtualNode(node) {
			if _, exists := nodeFreeNPU[node.Name]; !exists {
				continue
			}
		}
		allocatable := node.Status.Allocatable[fullResourceName]
		totalNPUFree += allocatable.Value() - nodeTotalOccupied[node.Name]
	}
	hasCandidate := totalNPUFree >= int64(reqCount)

	if !hasCandidate {
		klog.InfoS("ARCSync: PreFilter rejected pod (no node has enough NPU)",
			"pod", pod.Name, "required", reqCount)
		return nil, framework.NewStatus(framework.Unschedulable, "No node has enough available NPU slots")
	}

	if head := pl.olderPoolHead(pod, nsHasOffloading); head == nil {
		// This pod is the pool head. Pin the node it is draining toward so
		// backfilling younger pods keep off it while it waits.
		headFree, _ := headCouldHostFree(pod, int64(reqCount), nodeInfos, fullResourceName, nodeTotalOccupied, nsHasOffloading, nsOffloading)
		pl.updateDrainTarget(fifoPoolKey(pod, nsHasOffloading), pod, headFree)
	} else if st := pl.backfillCheck(pod, int64(reqCount), head, nsHasOffloading, nsOffloading, nodeInfos, fullResourceName, nodeTotalOccupied, nodeFreeNPU, totalNPUFree); st != nil {
		return nil, st
	}

	state.Write(stateKey, &preFilterState{
		requiredCount: int64(reqCount),
		resourceName:  fullResourceName,
		nodeFreeNPU:   nodeFreeNPU,
	})
	return nil, framework.NewStatus(framework.Success, "")
}

func (pl *ARCSync) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

func hasPendingRunnerOnNode(nodeInfos []*framework.NodeInfo, nodeName, resDomain, resModel string) bool {
	for _, ni := range nodeInfos {
		if ni.Node() == nil || ni.Node().Name != nodeName {
			continue
		}
		for _, podInfo := range ni.Pods {
			p := podInfo.Pod
			if p == nil {
				continue
			}
			if p.Labels[RequiredNPUCount] == "" {
				continue
			}
			if p.Labels[ResourceDomain] != resDomain || p.Labels[ResourceModel] != resModel {
				continue
			}
			if p.Status.Phase == v1.PodPending {
				return true
			}
		}
		return false
	}
	return false
}

func (pl *ARCSync) applyLiqoComparison(
	nodeInfos []*framework.NodeInfo,
	pod *v1.Pod,
	resDomain, resModel string,
	fullResourceName v1.ResourceName,
	nsLocalOccupied map[string]int64,
	nodeFreeNPU map[string]int64,
	nsOffloading *unstructured.Unstructured,
	reqCount int64,
	virtualNodes map[string]bool,
) {
	eligibleVirtuals := getEligibleVirtualNodes(nodeInfos, nsOffloading)

	// Remove virtual nodes that are not targeted by the NamespaceOffloading
	// clusterSelector. These must never receive pods from this namespace,
	// regardless of the local-vs-remote comparison outcome. Without this,
	// non-eligible virtual nodes would leak into the candidate set when local
	// wins the comparison or when eligibleVirtuals is empty.
	for nodeName := range nodeFreeNPU {
		if virtualNodes[nodeName] && !eligibleVirtuals[nodeName] {
			delete(nodeFreeNPU, nodeName)
		}
	}

	if len(eligibleVirtuals) == 0 {
		return
	}

	var localTotalAllocatable, localTotalOccupied int64
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || isVirtualNode(node) || !canScheduleOnNode(pod, node) {
			continue
		}
		if _, exists := nodeFreeNPU[node.Name]; !exists {
			continue
		}
		allocatable := node.Status.Allocatable[fullResourceName]
		localTotalAllocatable += allocatable.Value()
		localTotalOccupied += nsLocalOccupied[node.Name]
	}

	localTotalCapacity := localTotalAllocatable
	if queueLimit, qFound := getQueueNpuLimit(pod, pl.queueLister, fullResourceName); qFound {
		if queueLimit < localTotalCapacity {
			localTotalCapacity = queueLimit
		}
	}

	localRemaining := localTotalCapacity - localTotalOccupied
	if localRemaining < 0 {
		localRemaining = 0
	}

	var bestVirtNode string
	var bestVirtRemaining int64

	localHasCandidate := false
	for nodeName, free := range nodeFreeNPU {
		if !virtualNodes[nodeName] && free >= int64(reqCount) {
			localHasCandidate = true
			break
		}
	}

	for nodeName := range eligibleVirtuals {
		if free, exists := nodeFreeNPU[nodeName]; exists && free > bestVirtRemaining {
			if localHasCandidate && hasPendingRunnerOnNode(nodeInfos, nodeName, resDomain, resModel) {
				continue
			}
			bestVirtRemaining = free
			bestVirtNode = nodeName
		}
	}

	if localRemaining >= bestVirtRemaining {
		for nodeName := range eligibleVirtuals {
			delete(nodeFreeNPU, nodeName)
		}
		klog.InfoS("ARCSync: local wins liqo comparison",
			"pod", pod.Name, "localRemaining", localRemaining, "bestVirtRemaining", bestVirtRemaining)
	} else {
		for nodeName := range nodeFreeNPU {
			if nodeName != bestVirtNode {
				delete(nodeFreeNPU, nodeName)
			}
		}
		klog.InfoS("ARCSync: virtual node wins liqo comparison",
			"pod", pod.Name, "bestVirtNode", bestVirtNode, "bestVirtRemaining", bestVirtRemaining,
			"localRemaining", localRemaining)
	}
}

func (pl *ARCSync) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	s, err := state.Read(stateKey)
	if err != nil {
		return framework.NewStatus(framework.Success, "")
	}
	data := s.(*preFilterState)

	nodeName := nodeInfo.Node().Name
	free, exists := data.nodeFreeNPU[nodeName]
	if !exists {
		return framework.NewStatus(framework.Unschedulable, "node not eligible for NPU scheduling")
	}
	// Runner pods carry required-npu-count for reservation but don't request
	// NPU in their containers — they run on CPU nodes while NPU is reserved
	// elsewhere. Only enforce NPU capacity for pods that actually request NPU
	// resources in their containers; for others, let the default scheduler's
	// NodeResourcesFit and NodeAffinity filters handle placement.
	if !podRequestsNPU(pod, data.resourceName) {
		return framework.NewStatus(framework.Success, "")
	}
	if free < data.requiredCount {
		return framework.NewStatus(framework.Unschedulable, "insufficient NPU on node")
	}
	return framework.NewStatus(framework.Success, "")
}

func podRequestsNPU(pod *v1.Pod, resourceName v1.ResourceName) bool {
	for _, container := range pod.Spec.Containers {
		if _, exists := container.Resources.Requests[resourceName]; exists {
			return true
		}
	}
	return false
}

func (pl *ARCSync) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	s, err := state.Read(stateKey)
	if err != nil {
		return 0, framework.NewStatus(framework.Success, "")
	}
	data := s.(*preFilterState)
	free := data.nodeFreeNPU[nodeName]
	// Best-fit: prefer the node with the least free NPU that still passed
	// Filter. Packing small jobs into partially-used nodes preserves large
	// contiguous blocks for large jobs, reducing the fragmentation that
	// forces drain waits. (The previous most-free scoring was worst-fit: it
	// sent every small job to the emptiest node, breaking up exactly the
	// blocks the 8/16-card jobs need.) For runner pods this also prefers
	// NPU-less nodes, keeping them off NPU nodes entirely.
	score := framework.MaxNodeScore - free
	if score > framework.MaxNodeScore {
		score = framework.MaxNodeScore
	}
	if score < 0 {
		score = 0
	}
	return score, framework.NewStatus(framework.Success, "")
}

func (pl *ARCSync) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

func (pl *ARCSync) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	s, err := state.Read(stateKey)
	if err != nil {
		return framework.NewStatus(framework.Success, "")
	}
	data := s.(*preFilterState)

	podKey := string(pod.UID)
	if podKey == "" {
		podKey = pod.Namespace + "/" + pod.Name
	}

	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.inFlightReservations[podKey] = reservation{
		nodeName:  nodeName,
		count:     data.requiredCount,
		timestamp: time.Now(),
		baseName:  getBaseName(pod.Name),
		namespace: pod.Namespace,
	}
	klog.InfoS("ARCSync: Reserved NPU slots", "pod", pod.Name, "node", nodeName, "count", data.requiredCount)
	return framework.NewStatus(framework.Success, "")
}

func (pl *ARCSync) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	podKey := string(pod.UID)
	if podKey == "" {
		podKey = pod.Namespace + "/" + pod.Name
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	delete(pl.inFlightReservations, podKey)
	klog.InfoS("ARCSync: Unreserved NPU slots", "pod", pod.Name, "node", nodeName)
}

func (pl *ARCSync) PostBind(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	s, err := state.Read(stateKey)
	if err != nil {
		return
	}
	data := s.(*preFilterState)

	// Only clear reservation if the pod itself requests NPU (i.e. it's a workflow pod).
	// Runner pods don't request NPU — their reservation must persist until the
	// workflow pod starts and physical usage takes over.
	for _, container := range pod.Spec.Containers {
		if _, exists := container.Resources.Requests[data.resourceName]; exists {
			podKey := string(pod.UID)
			if podKey == "" {
				podKey = pod.Namespace + "/" + pod.Name
			}
			pl.mu.Lock()
			defer pl.mu.Unlock()
			delete(pl.inFlightReservations, podKey)
			klog.InfoS("ARCSync: PostBind cleared reservation (pod has NPU request)", "pod", pod.Name, "node", nodeName)
			return
		}
	}
	klog.V(4).InfoS("ARCSync: PostBind keeping reservation (runner pod)", "pod", pod.Name, "node", nodeName)
}
