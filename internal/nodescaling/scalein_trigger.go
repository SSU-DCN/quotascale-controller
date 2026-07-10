package nodescaling

import (
	"context"
	"fmt"
	"time"

	"github.com/SSU-DCN/quotascale-controller/pkg/resources"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ScaleInCapacityEvaluation struct {
	Eligible              bool
	ManagedLimits         resources.Resources
	CurrentWorkerCapacity resources.Resources
	ProjectedCapacity     resources.Resources
	RemovableNodes        []string
}

func (controller *NodeScalingController) EvaluateAutomaticScaleIn() error {
	if controller.runtime == nil || controller.client == nil || controller.quotaAutoscalerClient == nil || controller.inventoryStore == nil {
		return nil
	}
	if controller.hasActiveScaleInWaiters() {
		return nil
	}
	replicas, err := controller.runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		return err
	}
	if replicas <= minNodeCount {
		controller.resetScaleInTrigger()
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
			"managed ResourceQuota limits cpu=%dm memory=%dM still fit after removing scaling nodes %v, leaving worker capacity cpu=%dm memory=%dM for %s",
			evaluation.ManagedLimits.Cpu,
			evaluation.ManagedLimits.Memory,
			evaluation.RemovableNodes,
			evaluation.ProjectedCapacity.Cpu,
			evaluation.ProjectedCapacity.Memory,
			controller.scaleInTriggerDelay,
		),
	})
}

func (controller *NodeScalingController) EvaluateScaleInCapacity() (*ScaleInCapacityEvaluation, error) {
	managedLimits, err := controller.ManagedQuotaLimitTotals()
	if err != nil {
		return nil, err
	}

	currentWorkerCapacity, capacityByNode, err := controller.SchedulableWorkerNodeCapacity()
	if err != nil {
		return nil, err
	}

	inventory, err := controller.inventoryStore.Get()
	if err != nil {
		return nil, err
	}

	replicas, err := controller.runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		return nil, err
	}

	candidates, err := FindHighestOrderUsedInventoryNodes(inventory, int(replicas-minNodeCount))
	if err != nil {
		return nil, err
	}

	projectedCapacity := currentWorkerCapacity
	removableNodes := make([]string, 0, len(candidates))
	for _, node := range candidates {
		nodeCapacity, exists := capacityByNode[node.Name]
		if !exists {
			continue
		}

		nextCapacity := projectedCapacity
		nextCapacity.Cpu -= nodeCapacity.Cpu
		nextCapacity.Memory -= nodeCapacity.Memory
		if nextCapacity.Cpu < 0 {
			nextCapacity.Cpu = 0
		}
		if nextCapacity.Memory < 0 {
			nextCapacity.Memory = 0
		}

		if managedLimits.Cpu > nextCapacity.Cpu || managedLimits.Memory > nextCapacity.Memory {
			break
		}

		projectedCapacity = nextCapacity
		removableNodes = append(removableNodes, node.Name)
	}

	return &ScaleInCapacityEvaluation{
		Eligible:              len(removableNodes) > 0,
		ManagedLimits:         managedLimits,
		CurrentWorkerCapacity: currentWorkerCapacity,
		ProjectedCapacity:     projectedCapacity,
		RemovableNodes:        removableNodes,
	}, nil
}

func (controller *NodeScalingController) ManagedQuotaLimitTotals() (resources.Resources, error) {
	scalers, err := controller.quotaAutoscalerClient.IchpV1().QuotaAutoscalers("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resources.Resources{}, err
	}

	total := resources.Resources{}
	for _, scaler := range scalers.Items {
		quota, err := controller.client.CoreV1().ResourceQuotas(scaler.Namespace).Get(context.TODO(), scaler.Spec.ResourceQuota, metav1.GetOptions{})
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
