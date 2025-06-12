package runnernpu

import (
	"context"
	"fmt"
	"log"
	"strconv"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	framework "k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Name                   = "runnerScheduler"
	npuCountLabel          = "ascend-ci.com/required-npu-count"
	npuResourceDomainLabel = "ascend-ci.com/npu-resource-domain"
	npuResourceModelLabel  = "ascend-ci.com/npu-resource-model"
)

type runnerScheduler struct{}

var _ framework.FilterPlugin = &runnerScheduler{}
var _ framework.PreScorePlugin = &runnerScheduler{}

// New initializes a new plugin and returns it.
func New(_ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
	return &runnerScheduler{}, nil
}

// Name returns name of the plugin.
func (pl *runnerScheduler) Name() string {
	return Name
}

// 根据待调度pod与当前节点的剩余NPU卡判断是否可以调度到当前节点。
// 调度pod的`label.ascend-ci.com/required-npu-count`表明pod所需NPU卡。
// 当前节点的`label.ascend-ci.com/npu-resource-domain`与`ascend-ci.com/npu-resource-model`表明节点的NPU类型，根据NPU类型获当前节点的总卡数。
// 遍历当前节点的所有pod，将其`label.ascend-ci.com/required-npu-count`相加，作为当前节点已分配卡数。
// 如果当前节点的总卡数-当前节点已分配卡数<=pod所需NPU卡，则可以将pod分配到当前节点。
// Filter函数用于过滤出符合条件的节点，返回一个framework.Status类型的结果
func (pl *runnerScheduler) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	schedulingPodNpuCount, err := extractNpuCountFromPodLabel(pod)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}

	allocatableNpuCount, err := getAllocatableNpuCountFromNode(nodeInfo)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}

	allocatedNpuCount := 0
	podNames := make([]string, 0, len(nodeInfo.Pods))
	for _, podInfo := range nodeInfo.Pods {
		if podInfo.Pod != nil {
			podNames = append(podNames, podInfo.Pod.Name)
		}
		podNpuCount, err := extractNpuCountFromPodLabel(podInfo.Pod)
		if err != nil {
			continue
		}
		allocatedNpuCount += podNpuCount

		klog.InfoS("pod status", "nodeName", nodeInfo.Node().Name, "podName", podInfo.Pod.Name, "podNpuCount", podNpuCount, "allocatedNpuCount", allocatedNpuCount, "schedulingPod", pod.Name, "schedulingPodNpuCount", schedulingPodNpuCount)
	}

	klog.InfoS("Node status", "nodeName", nodeInfo.Node().Name, "allocatableNpu", allocatableNpuCount, "allocatedNpu", allocatedNpuCount, "schedulingCount", schedulingPodNpuCount, "schedulingPod", pod.Name, npuResourceDomainLabel, nodeInfo.Node().Labels[npuResourceDomainLabel], npuResourceModelLabel, nodeInfo.Node().Labels[npuResourceModelLabel], "nodePodsNames", podNames)

	if allocatableNpuCount-int64(allocatedNpuCount) < int64(schedulingPodNpuCount) {
		klog.Infof("current node has no enough npu, node name : %v", nodeInfo.Node().Name)
		return framework.NewStatus(framework.Unschedulable, "current node has no enough npu")
	}

	return framework.NewStatus(framework.Success, "")
}

func (pl *runnerScheduler) PreScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodes []*v1.Node) *framework.Status {
	log.Printf("PreScore: %+v", nodes)
	return framework.NewStatus(framework.Success, "Pod: "+pod.Name)
}

func getAllocatableNpuCountFromNode(nodeInfo *framework.NodeInfo) (int64, error) {
	domainLabel, domainLabelExists := nodeInfo.Node().Labels[npuResourceDomainLabel]
	modelLabel, modelLabelExists := nodeInfo.Node().Labels[npuResourceModelLabel]
	if !modelLabelExists || !domainLabelExists {
		return 0, fmt.Errorf("fail to parse npu resource label. nodeName: %v, label: %v = %v, %v = %v", nodeInfo.Node().Name, npuResourceDomainLabel, domainLabel, npuResourceModelLabel, modelLabel)
	}

	resourceType := domainLabel + "/" + modelLabel
	allocatableNpuCount, ok := nodeInfo.Allocatable.ScalarResources[v1.ResourceName(resourceType)]
	if !ok {
		return 0, fmt.Errorf("fail to parse allocatable npu resource. nodeName: %v, label: %v = %v, %v = %v", nodeInfo.Node().Name, npuResourceDomainLabel, domainLabel, npuResourceModelLabel, modelLabel)
	}
	return allocatableNpuCount, nil
}

func extractNpuCountFromPodLabel(pod *v1.Pod) (int, error) {
	labelValue, exists := pod.Labels[npuCountLabel]
	if !exists {
		return 0, fmt.Errorf("fail to parse NPU label. pod: %v, podNamespace: %v, label: %v", pod.Name, pod.Namespace, npuCountLabel)
	}

	npuCount, err := strconv.Atoi(labelValue)
	if err != nil {
		return 0, fmt.Errorf("fail to parse NPU count. pod: %v, podNamespace: %v, label: %v", pod.Name, pod.Namespace, npuCountLabel)
	}
	return npuCount, nil
}
