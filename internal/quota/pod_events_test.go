package quota

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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

func TestFailedSchedulingPodEventReturnsPendingPodRequests(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-pod", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1025m"), corev1.ResourceMemory: resource.MustParse("900Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("2848Mi")},
			},
		}}},
	}
	events := []corev1.Event{{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "default"},
		Reason:         "FailedScheduling",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "default", Name: "pending-pod"},
	}}

	quotaDemand, podRequests, err := GetResourceDemandsFromPodEvents(fake.NewSimpleClientset(pod), events)
	if err != nil {
		t.Fatalf("unexpected demand lookup error: %v", err)
	}
	if quotaDemand.Cpu != 3000 || podRequests.Cpu != 1025 {
		t.Fatalf("expected pod limits for quota and requests for scheduling, quota=%+v requests=%+v", quotaDemand, podRequests)
	}
}
