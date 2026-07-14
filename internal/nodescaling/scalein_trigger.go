package nodescaling

import (
	"context"
	"fmt"
	"time"

	"github.com/SSU-DCN/quotascale-controller/internal/scalerresolver"
	"github.com/SSU-DCN/quotascale-controller/pkg/resources"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ScaleInCapacityEvaluation struct {
	Eligible               bool
	CurrentWorkerAvailable resources.Resources
	ProjectedAvailable     resources.Resources
	RemovableNodes         []string
}

func (controller *NodeScalingController) EvaluateAutomaticScaleIn() error {
	if controller.runtime == nil || controller.client == nil || controller.quotaAutoscalerClient == nil || controller.inventoryStore == nil {
		return nil
	}
	if controller.hasActiveScaleInWaiters() {
		return nil
	}

	evaluation, err := controller.EvaluateScaleInCapacity()
	if err != nil {
		return err
	}
	if !evaluation.Eligible {
		controller.resetScaleInTrigger()
		return nil
	}

	if controller.scaleInEligibleSince.IsZero() {
		controller.scaleInEligibleSince = time.Now()
		return nil
	}
	if time.Since(controller.scaleInEligibleSince) < controller.scaleInTriggerDelay {
		return nil
	}

	controller.resetScaleInTrigger()
	return controller.HandleScaleInRequest(ScaleInRequest{
		Reason: fmt.Sprintf(
			"worker available resources still stay non-negative after removing scaling nodes %v, leaving available cpu=%dm memory=%dM for %s",
			evaluation.RemovableNodes,
			evaluation.ProjectedAvailable.Cpu,
			evaluation.ProjectedAvailable.Memory,
			controller.scaleInTriggerDelay,
		),
	})
}

func (controller *NodeScalingController) EvaluateScaleInCapacity() (*ScaleInCapacityEvaluation, error) {
	currentWorkerAvailable, capacityByNode, err := controller.SchedulableWorkerNodeAvailableResources()
	if err != nil {
		return nil, err
	}

	inventory, err := controller.inventoryStore.Get()
	if err != nil {
		return nil, err
	}

	usedCount := CountUsedInventoryNodes(inventory)
	if usedCount == 0 {
		return &ScaleInCapacityEvaluation{
			Eligible:               false,
			CurrentWorkerAvailable: currentWorkerAvailable,
			ProjectedAvailable:     currentWorkerAvailable,
			RemovableNodes:         []string{},
		}, nil
	}

	candidates, err := FindHighestOrderUsedInventoryNodes(inventory, usedCount)
	if err != nil {
		return nil, err
	}

	projectedAvailable := currentWorkerAvailable
	removableNodes := make([]string, 0, len(candidates))
	for _, node := range candidates {
		nodeCapacity, exists := capacityByNode[node.Name]
		if !exists {
			continue
		}

		nextAvailable := projectedAvailable
		nextAvailable.Cpu -= nodeCapacity.Cpu
		nextAvailable.Memory -= nodeCapacity.Memory
		if nextAvailable.Cpu < 0 || nextAvailable.Memory < 0 {
			break
		}

		projectedAvailable = nextAvailable
		removableNodes = append(removableNodes, node.Name)
	}

	return &ScaleInCapacityEvaluation{
		Eligible:               len(removableNodes) > 0,
		CurrentWorkerAvailable: currentWorkerAvailable,
		ProjectedAvailable:     projectedAvailable,
		RemovableNodes:         removableNodes,
	}, nil
}

func (controller *NodeScalingController) ManagedQuotaLimitTotals() (resources.Resources, error) {
	scalers, err := controller.quotaAutoscalerClient.IchpV1().QuotaAutoscalers("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resources.Resources{}, err
	}

	total := resources.Resources{}
	for _, scaler := range scalers.Items {
		scalerCopy := scaler
		quota, err := scalerresolver.ResolveResourceQuota(context.TODO(), controller.client, controller.quotaAutoscalerClient, &scalerCopy)
		if err != nil {
			return resources.Resources{}, err
		}

		cpuLimit, ok := quota.Spec.Hard[v12.ResourceLimitsCPU]
		if !ok {
			cpuLimit = *quota.Spec.Hard.Cpu()
		}
		memLimit, ok := quota.Spec.Hard[v12.ResourceLimitsMemory]
		if !ok {
			memLimit = *quota.Spec.Hard.Memory()
		}

		total.Cpu += cpuLimit.ScaledValue(resource.Milli)
		total.Memory += memLimit.ScaledValue(resource.Mega)
	}

	return total, nil
}

func (controller *NodeScalingController) NonScalingWorkerNodeCapacity() (resources.Resources, error) {
	total, _, err := controller.SchedulableWorkerNodeCapacity()
	return total, err
}

func (controller *NodeScalingController) SchedulableWorkerNodeAvailableResources() (resources.Resources, map[string]resources.Resources, error) {
	totalCapacity, capacityByNode, err := controller.SchedulableWorkerNodeCapacity()
	if err != nil {
		return resources.Resources{}, nil, err
	}

	pods, err := controller.client.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resources.Resources{}, nil, err
	}

	available := totalCapacity
	for _, pod := range pods.Items {
		if _, tracked := capacityByNode[pod.Spec.NodeName]; !tracked || isTerminalPodForAvailability(pod) {
			continue
		}

		requested := podRequestedResourcesForAvailability(pod)
		available.Cpu -= requested.Cpu
		available.Memory -= requested.Memory
	}

	if available.Cpu < 0 {
		available.Cpu = 0
	}
	if available.Memory < 0 {
		available.Memory = 0
	}
	return available, capacityByNode, nil
}

func (controller *NodeScalingController) SchedulableWorkerNodeCapacity() (resources.Resources, map[string]resources.Resources, error) {
	nodes, err := controller.client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resources.Resources{}, nil, err
	}

	total := resources.Resources{}
	perNode := map[string]resources.Resources{}
	for _, node := range nodes.Items {
		if isControlPlaneNode(node) || node.Spec.Unschedulable || nodeHasScalingTaint(node) {
			continue
		}

		nodeCapacity := resources.Resources{
			Cpu:    node.Status.Allocatable.Cpu().ScaledValue(resource.Milli),
			Memory: node.Status.Allocatable.Memory().ScaledValue(resource.Mega),
		}
		total.Cpu += nodeCapacity.Cpu
		total.Memory += nodeCapacity.Memory
		perNode[node.Name] = nodeCapacity
	}

	return total, perNode, nil
}

func isControlPlaneNode(node v12.Node) bool {
	_, controlPlane := node.Labels["node-role.kubernetes.io/control-plane"]
	_, master := node.Labels["node-role.kubernetes.io/master"]
	return controlPlane || master
}

func nodeHasScalingRole(node v12.Node) bool {
	if node.Labels["role"] == "scaling" {
		return true
	}
	if _, ok := node.Labels["node-role.kubernetes.io/scaling"]; ok {
		return true
	}
	for _, taint := range node.Spec.Taints {
		if taint.Key == "node-role.kubernetes.io/scaling" && taint.Effect == v12.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

func nodeHasScalingTaint(node v12.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == "node-role.kubernetes.io/scaling" && taint.Effect == v12.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

func isTerminalPodForAvailability(pod v12.Pod) bool {
	return pod.Status.Phase == v12.PodSucceeded || pod.Status.Phase == v12.PodFailed
}

func podRequestedResourcesForAvailability(pod v12.Pod) resources.Resources {
	sum := resources.Resources{}
	for _, container := range pod.Spec.Containers {
		sum.Cpu += container.Resources.Requests.Cpu().ScaledValue(resource.Milli)
		sum.Memory += container.Resources.Requests.Memory().ScaledValue(resource.Mega)
	}

	initMax := resources.Resources{}
	for _, container := range pod.Spec.InitContainers {
		cpu := container.Resources.Requests.Cpu().ScaledValue(resource.Milli)
		memory := container.Resources.Requests.Memory().ScaledValue(resource.Mega)
		if cpu > initMax.Cpu {
			initMax.Cpu = cpu
		}
		if memory > initMax.Memory {
			initMax.Memory = memory
		}
	}
	sum.Max(&initMax)
	return sum
}
