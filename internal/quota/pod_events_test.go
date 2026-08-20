package quota

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestPodQuotaAndSchedulingResourcesAreCalculatedSeparately(t *testing.T) {
	template := corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
				{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("25m"),
							corev1.ResourceMemory: resource.MustParse("400Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("800Mi"),
						},
					},
				},
			},
		},
	}

	quotaResources := CalculatePodResources(template, 2)
	requestResources := CalculatePodRequestResources(template, 2)

	if quotaResources.Cpu != 6000 || quotaResources.Memory != 5974 {
		t.Fatalf("expected two replicas of limit-backed quota resources, got %+v", quotaResources)
	}
	if requestResources.Cpu != 1050 || requestResources.Memory != 1914 {
		t.Fatalf("expected two replicas of scheduler requests, got %+v", requestResources)
	}
}
