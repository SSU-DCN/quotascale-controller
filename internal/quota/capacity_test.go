package quota

import (
	"testing"

	"github.com/SSU-DCN/quotascale-controller/pkg/resources"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWorkerNodeAvailableResourcesExcludesControlPlaneAndAllocatedPods(t *testing.T) {
	client := fake.NewSimpleClientset(
		node("control-plane", "4", "8Gi", map[string]string{"node-role.kubernetes.io/control-plane": ""}),
		node("worker-1", "4", "8Gi", nil),
		pod("running", "worker-1", corev1.PodRunning, "1200m", "1Gi"),
		pod("finished", "worker-1", corev1.PodSucceeded, "2000m", "2Gi"),
	)

	available, err := WorkerNodeAvailableResources(client)
	if err != nil {
		t.Fatalf("expected available resources, got error: %v", err)
	}

	if available.Cpu != 2800 {
		t.Fatalf("expected 2800m CPU available, got %dm", available.Cpu)
	}
	allocatableMemory := resource.MustParse("8Gi")
	requestedMemory := resource.MustParse("1Gi")
	expectedMemory := allocatableMemory.ScaledValue(resource.Mega) - requestedMemory.ScaledValue(resource.Mega)
	if available.Memory != expectedMemory {
		t.Fatalf("expected %dM memory available, got %dM", expectedMemory, available.Memory)
	}
}

func TestWorkerNodeAvailableResourcesExcludesOnlyScalingTaintedNodes(t *testing.T) {
	client := fake.NewSimpleClientset(
		node("worker-1", "4", "8Gi", nil),
		node("scaling-by-role", "4", "8Gi", map[string]string{"role": "scaling"}),
		node("scaling-by-label", "4", "8Gi", map[string]string{"node-role.kubernetes.io/scaling": ""}),
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "scaling-by-taint"},
			Spec: corev1.NodeSpec{
				Taints: []corev1.Taint{
					{Key: "node-role.kubernetes.io/scaling", Effect: corev1.TaintEffectNoSchedule},
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
	)

	available, err := WorkerNodeAvailableResources(client)
	if err != nil {
		t.Fatalf("expected available resources, got error: %v", err)
	}

	if available.Cpu != 12000 {
		t.Fatalf("expected taint-free labeled nodes to count toward CPU, got %dm", available.Cpu)
	}
	expectedMemoryQuantity := resource.MustParse("24Gi")
	expectedMemory := expectedMemoryQuantity.ScaledValue(resource.Mega)
	if available.Memory != expectedMemory {
		t.Fatalf("expected taint-free labeled nodes to count toward memory, got %dM", available.Memory)
	}
}

func TestEnsurePodDemandFitsClusterRejectsUnavailableCapacity(t *testing.T) {
	client := fake.NewSimpleClientset(
		node("worker-1", "4", "4Gi", nil),
		pod("running", "worker-1", corev1.PodRunning, "1000m", "1Gi"),
	)

	err := EnsurePodDemandFitsCluster(client, resources.Resources{Cpu: 3500, Memory: 3500})
	if err == nil {
		t.Fatal("expected pending pod requests to be rejected when they exceed worker availability")
	}
}

func TestEnsurePodDemandFitsClusterAllowsSchedulableRequests(t *testing.T) {
	client := fake.NewSimpleClientset(
		node("worker-1", "4", "4Gi", nil),
		pod("running", "worker-1", corev1.PodRunning, "500m", "512Mi"),
	)

	err := EnsurePodDemandFitsCluster(client, resources.Resources{Cpu: 3000, Memory: 3072})
	if err != nil {
		t.Fatalf("expected pending pod requests to fit worker availability, got error: %v", err)
	}
}

func TestEnsurePodDemandFitsClusterAllowsEmptyDemand(t *testing.T) {
	client := fake.NewSimpleClientset()

	err := EnsurePodDemandFitsCluster(client, resources.Resources{})
	if err != nil {
		t.Fatalf("expected empty pending demand to bypass capacity check, got error: %v", err)
	}
}

func node(name, cpu, memory string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			},
		},
	}
}

func pod(name, nodeName string, phase corev1.PodPhase, cpu, memory string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cpu),
							corev1.ResourceMemory: resource.MustParse(memory),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}
