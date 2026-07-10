package quota

import (
	"context"
	"errors"
	"fmt"

	"github.com/SSU-DCN/quotascale-controller/pkg/logging"
	"github.com/SSU-DCN/quotascale-controller/pkg/resources"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v13 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func GetResourcesFromPodEvents(client kubernetes.Interface, events []v12.Event) (*resources.Resources, error) {
	sum := &resources.Resources{}
	involvedObjects := map[string]bool{} // Make sure we only handle each InvolvedObject once

	for _, ev := range events {
		if isDaemonSet(ev) {
			continue // Skip these for now
		}

		name := ev.InvolvedObject.Kind + ev.InvolvedObject.Name
		if _, ok := involvedObjects[name]; !ok {
			logging.LogInfo("[%s] Processing event %s %s", ev.Namespace, ev.InvolvedObject.Kind, ev.InvolvedObject.Name)
			involvedObjects[name] = true

			spec, missingReplicas, err := getPodTemplateSpecFromEv(client, ev)
			if err != nil {
				logging.LogError("[%s] Cannot get template spec from event: %s %s: %v. Ignoring it", ev.Namespace, ev.InvolvedObject.Kind, ev.InvolvedObject.Name, err)
				continue // We process those we do know
			}

			res := CalculatePodResources(spec, int64(missingReplicas))
			sum.Add(&res)
		}
	}

	return sum, nil
}

// CalculatePodResources sums the container resources of a Pod and multiplies them by the missing replicas.
// Zero or negative replicas will result in an empty Resource.
func CalculatePodResources(podTemplate v12.PodTemplateSpec, missingReplicas int64) resources.Resources {
	neededCpu := resource.NewQuantity(0, resource.DecimalSI)
	neededMemory := resource.NewQuantity(0, resource.DecimalSI)
	for _, container := range podTemplate.Spec.Containers {
		if container.Resources.Requests != nil {
			expectedNeededCpu := *container.Resources.Requests.Cpu()
			expectedNeededMemory := *container.Resources.Requests.Memory()

			// Take limits into account when they are higher than requests.
			if container.Resources.Limits != nil {
				expectedNeededMemory = MaxResourceQuantity(container.Resources.Requests.Memory(), container.Resources.Limits.Memory())
				expectedNeededCpu = MaxResourceQuantity(container.Resources.Requests.Cpu(), container.Resources.Limits.Cpu())
			}

			neededCpu.Add(expectedNeededCpu)
			neededMemory.Add(expectedNeededMemory)
		}
	}

	if missingReplicas <= 0 {
		return resources.Resources{}
	}
	return resources.Resources{
		Cpu:    neededCpu.ScaledValue(resource.Milli) * missingReplicas,
		Memory: neededMemory.ScaledValue(resource.Mega) * missingReplicas,
	}
}

func isDaemonSet(ev v12.Event) bool {
	return ev.InvolvedObject.Kind == "DaemonSet"
}

func getPodTemplateSpecFromEv(client kubernetes.Interface, ev v12.Event) (v12.PodTemplateSpec, int32, error) {
	var pod v12.PodTemplateSpec
	var replicas int32 = 1

	namespace := ev.InvolvedObject.Namespace
	name := ev.InvolvedObject.Name
	switch ev.InvolvedObject.Kind {
	case "ReplicaSet":
		target, err := client.AppsV1().ReplicaSets(namespace).Get(context.TODO(), name, v13.GetOptions{})
		if err != nil {
			return pod, replicas, err
		}
		pod = target.Spec.Template
		replicas = *target.Spec.Replicas - target.Status.Replicas
	case "Job":
		target, err := client.BatchV1().Jobs(namespace).Get(context.TODO(), name, v13.GetOptions{})
		if err != nil {
			return pod, replicas, err
		}
		pod = target.Spec.Template
	case "StatefulSet":
		target, err := client.AppsV1().StatefulSets(namespace).Get(context.TODO(), name, v13.GetOptions{})
		if err != nil {
			return pod, replicas, err
		}
		pod = target.Spec.Template
		replicas = *target.Spec.Replicas - target.Status.Replicas
	case "ReplicationController":
		target, err := client.CoreV1().ReplicationControllers(namespace).Get(context.TODO(), name, v13.GetOptions{})
		if err != nil {
			return pod, replicas, err
		}
		if target.Spec.Template == nil {
			return pod, replicas, fmt.Errorf("ReplicationController %s Pod Template missing", name)
		}
		pod = *target.Spec.Template
		replicas = *target.Spec.Replicas - target.Status.Replicas
	default:
		return v12.PodTemplateSpec{}, 0, errors.New("unsupported event")
	}

	pod.Namespace = namespace
	return pod, replicas, nil
}

func MaxResourceQuantity(left, right *resource.Quantity) resource.Quantity {
	if right.Cmp(*left) > 0 {
		return *right
	}
	return *left
}
