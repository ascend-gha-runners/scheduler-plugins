package arcsync

import (
	"encoding/json"
	"fmt"
	"strconv"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const virtNodeLabelKey = "liqo.io/remote-cluster-id"

var (
	gvrNamespaceOffloading = schema.GroupVersionResource{
		Group:    "offloading.liqo.io",
		Version:  "v1beta1",
		Resource: "namespaceoffloadings",
	}
	gvrQueue = schema.GroupVersionResource{
		Group:    "scheduling.volcano.sh",
		Version:  "v1beta1",
		Resource: "queues",
	}
)

type nsOffloadingLister interface {
	Get(namespace string) (*unstructured.Unstructured, bool, error)
}

type queueLister interface {
	Get(queueName string) (*unstructured.Unstructured, bool, error)
}

type dynamicNSOffloadingLister struct {
	lister cache.GenericLister
}

func (d *dynamicNSOffloadingLister) Get(namespace string) (*unstructured.Unstructured, bool, error) {
	objs, err := d.lister.List(labels.Everything())
	if err != nil {
		return nil, false, err
	}
	for _, obj := range objs {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if u.GetNamespace() == namespace {
			return u, true, nil
		}
	}
	return nil, false, nil
}

type dynamicQueueLister struct {
	lister cache.GenericLister
}

func (d *dynamicQueueLister) Get(queueName string) (*unstructured.Unstructured, bool, error) {
	obj, err := d.lister.Get(queueName)
	if err != nil {
		return nil, false, nil
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, false, fmt.Errorf("expected *unstructured.Unstructured, got %T", obj)
	}
	return u, true, nil
}

func isVirtualNode(node *v1.Node) bool {
	if node == nil {
		return false
	}
	_, exists := node.Labels[virtNodeLabelKey]
	return exists
}

func calcVirtualNodeOccupied(nodeInfo *framework.NodeInfo, resDomain, resModel string, fullResourceName v1.ResourceName) int64 {
	if nodeInfo == nil {
		return 0
	}
	var occupied int64
	for _, podInfo := range nodeInfo.Pods {
		p := podInfo.Pod
		if p == nil {
			continue
		}
		if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
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
		if p.Labels[RequiredNPUCount] != "" && p.Labels[ResourceDomain] == resDomain && p.Labels[ResourceModel] == resModel {
			count, err := strconv.ParseInt(p.Labels[RequiredNPUCount], 10, 64)
			if err == nil && count > podUsage {
				podUsage = count
			}
		}
		occupied += podUsage
	}
	return occupied
}

func getEligibleVirtualNodes(nodeInfos []*framework.NodeInfo, nsOffloading *unstructured.Unstructured) map[string]bool {
	result := make(map[string]bool)
	if nsOffloading == nil {
		return result
	}
	selector, err := extractClusterSelector(nsOffloading)
	if err != nil {
		return result
	}
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || !isVirtualNode(node) {
			continue
		}
		if selector.Matches(labels.Set(node.Labels)) {
			result[node.Name] = true
		}
	}
	return result
}

func extractClusterSelector(obj *unstructured.Unstructured) (labels.Selector, error) {
	selectorMap, found, err := unstructured.NestedMap(obj.Object, "spec", "clusterSelector")
	if !found || err != nil {
		return labels.Everything(), nil
	}
	bytes, err := json.Marshal(selectorMap)
	if err != nil {
		return labels.Everything(), nil
	}
	var ls metav1.LabelSelector
	if err := json.Unmarshal(bytes, &ls); err != nil {
		return labels.Everything(), nil
	}
	selector, err := metav1.LabelSelectorAsSelector(&ls)
	if err != nil {
		return labels.Everything(), nil
	}
	return selector, nil
}
