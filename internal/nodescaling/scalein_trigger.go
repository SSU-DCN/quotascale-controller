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
	Eligible           bool
	ManagedLimits      resources.Resources
	NonScalingCapacity resources.Resources
}

func (controller *NodeScalingController) EvaluateAutomaticScaleIn() error {
	if controller.runtime == nil || controller.client == nil || controller.quotaAutoscalerClient == nil {
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
			"managed ResourceQuota limits cpu=%dm memory=%dM fit within non-scaling node capacity cpu=%dm memory=%dM for %s",
			evaluation.ManagedLimits.Cpu,
			evaluation.ManagedLimits.Memory,
			evaluation.NonScalingCapacity.Cpu,
			evaluation.NonScalingCapacity.Memory,
			controller.scaleInTriggerDelay,
		),
	})
}

func (controller *NodeScalingController) EvaluateScaleInCapacity() (*ScaleInCapacityEvaluation, error) {
	managedLimits, err := controller.ManagedQuotaLimitTotals()
	if err != nil {
		return nil, err
	}

	nonScalingCapacity, err := controller.NonScalingWorkerNodeCapacity()
	if err != nil {
		return nil, err
	}

	return &ScaleInCapacityEvaluation{
		Eligible:           managedLimits.Cpu <= nonScalingCapacity.Cpu && managedLimits.Memory <= nonScalingCapacity.Memory,
		ManagedLimits:      managedLimits,
		NonScalingCapacity: nonScalingCapacity,
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
	nodes, err := controller.client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resources.Resources{}, err
	}

	total := resources.Resources{}
	for _, node := range nodes.Items {
		if isControlPlaneNode(node) || node.Spec.Unschedulable || nodeHasScalingRole(node) {
			continue
		}

		total.Cpu += node.Status.Allocatable.Cpu().ScaledValue(resource.Milli)
		total.Memory += node.Status.Allocatable.Memory().ScaledValue(resource.Mega)
	}

	return total, nil
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
