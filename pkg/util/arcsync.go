package util

import (
	"context"
	"strconv"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/klog/v2"
)

const (
	ARCSyncName = "ARCSync"
	RequiredNPUCount  = "ascend-ci.com/required-npu-count"
	ResourceDomain    = "ascend-ci.com/npu-resource-domain"
	ResourceModel     = "ascend-ci.com/npu-resource-model"
	AllocatedNPUCount = "ascend-ci.com/npu-count"
)

type ARCSync struct {
	Handle framework.Handle
}

var _ framework.PreFilterPlugin = &ARCSync{}

func NewARCSync(_ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &ARCSync{Handle: h}, nil
}

func (pl *ARCSync) Name() string {
	return ARCSyncName
}

func (pl *ARCSync) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) *framework.Status {
	reqCountStr, ok := pod.Labels[RequiredNPUCount]
	if !ok {
		return framework.NewStatus(framework.Success, "")
	}

	reqCount, _ := strconv.Atoi(reqCountStr)
	resDomain := pod.Labels[ResourceDomain]
	resModel := pod.Labels[ResourceModel]
	fullResourceName := v1.ResourceName(resDomain + "/" + resModel)

	nodeInfos, err := pl.Handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return framework.NewStatus(framework.Error, err.Error())
	}

	foundCandidate := false
	for _, nodeInfo := range nodeInfos {
		if nodeInfo.Node() == nil {
			continue
		}

		var occupiedNPU int64 = 0
		for _, podInfo := range nodeInfo.Pods {
			p := podInfo.Pod
			if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
				continue
			}

			if countStr, ok := p.Labels[AllocatedNPUCount]; ok {
				count, _ := strconv.ParseInt(countStr, 10, 64)
				occupiedNPU += count
			} else {
				for _, container := range p.Spec.Containers {
					if res, ok := container.Resources.Requests[fullResourceName]; ok {
						occupiedNPU += res.Value()
					}
				}
			}
		}

		allocatable := nodeInfo.Node().Status.Allocatable[fullResourceName]
		freeNPU := allocatable.Value() - occupiedNPU

		if freeNPU >= int64(reqCount) {
			foundCandidate = true
			break
		}
	}

	if foundCandidate {
		return framework.NewStatus(framework.Success, "")
	}

	return framework.NewStatus(framework.Unschedulable, "Global NPU resources insufficient")
}

func (pl *ARCSync) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
