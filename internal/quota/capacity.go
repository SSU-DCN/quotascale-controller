package quota

import (
	"context"
	"fmt"

	"github.com/SSU-DCN/quotascale-controller/pkg/resources"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func EnsureScaleUpFitsCluster(client kubernetes.Interface, current, desired resources.Resources) error {
	if desired.Cpu <= current.Cpu && desired.Memory <= current.Memory {
		return nil
	}

	required := resources.Resources{}
	if desired.Cpu > current.Cpu {
		required.Cpu = desired.Cpu - current.Cpu
	}
	if desired.Memory > current.Memory {
		required.Memory = desired.Memory - current.Memory
	}

	available, err := WorkerNodeAvailableResources(client)
	if err != nil {
		return err
	}
	if required.Cpu > available.Cpu || required.Memory > available.Memory {
		return fmt.Errorf(
			"scale up requires additional CPU %dm and memory %dM, but worker node available CPU is %dm and memory is %dM",
			required.Cpu,
			required.Memory,
			available.Cpu,
			available.Memory,
		)
	}

	return nil
}

func WorkerNodeAvailableResources(client kubernetes.Interface) (resources.Resources, error) {
	nodes, err := client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resources.Resources{}, err
	}

	workerNodes := map[string]bool{}
	available := resources.Resources{}
	for _, node := range nodes.Items {
		if isControlPlaneNode(node) || node.Spec.Unschedulable || nodeHasScalingTaint(node) {
			continue
		}

		workerNodes[node.Name] = true
		available.Cpu += node.Status.Allocatable.Cpu().ScaledValue(resource.Milli)
		available.Memory += node.Status.Allocatable.Memory().ScaledValue(resource.Mega)
	}

	pods, err := client.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resources.Resources{}, err
	}

	for _, pod := range pods.Items {
		if !workerNodes[pod.Spec.NodeName] || isTerminalPod(pod) {
			continue
		}

		requested := PodRequestedResources(pod)
		available.Cpu -= requested.Cpu
		available.Memory -= requested.Memory
	}

	if available.Cpu < 0 {
		available.Cpu = 0
	}
	if available.Memory < 0 {
		available.Memory = 0
	}
	return available, nil
}

func isControlPlaneNode(node corev1.Node) bool {
	_, controlPlane := node.Labels["node-role.kubernetes.io/control-plane"]
	_, master := node.Labels["node-role.kubernetes.io/master"]
	return controlPlane || master
}

func nodeHasScalingTaint(node corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == "node-role.kubernetes.io/scaling" && taint.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

func isTerminalPod(pod corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func PodRequestedResources(pod corev1.Pod) resources.Resources {
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
