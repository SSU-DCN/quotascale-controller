package scalerresolver

import (
	"context"
	"testing"

	scalerv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/quotaautoscaler/v1"
	scalerfake "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestResolveResourceQuotaUsesExplicitName(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(
		&corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "compute-resources", Namespace: "default"},
		},
	)
	scaler := &scalerv1.QuotaAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "qa", Namespace: "default"},
		Spec:       scalerv1.QuotaAutoscalerSpec{ResourceQuota: "compute-resources"},
	}

	quota, err := ResolveResourceQuota(context.TODO(), kubeClient, nil, scaler)
	if err != nil {
		t.Fatalf("expected explicit ResourceQuota resolution to succeed, got error: %v", err)
	}
	if quota.Name != "compute-resources" {
		t.Fatalf("expected compute-resources, got %s", quota.Name)
	}
}

func TestResolveResourceQuotaAutoPersistsSingleQuota(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(
		&corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "compute-resources", Namespace: "default"},
		},
	)
	scalerClient := scalerfake.NewSimpleClientset(
		&scalerv1.QuotaAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "qa", Namespace: "default"},
		},
	)
	scaler := &scalerv1.QuotaAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "qa", Namespace: "default"},
	}

	quota, err := ResolveResourceQuota(context.TODO(), kubeClient, scalerClient, scaler)
	if err != nil {
		t.Fatalf("expected automatic ResourceQuota resolution to succeed, got error: %v", err)
	}
	if quota.Name != "compute-resources" {
		t.Fatalf("expected compute-resources, got %s", quota.Name)
	}
	if scaler.Spec.ResourceQuota != "compute-resources" {
		t.Fatalf("expected scaler spec.resourceQuota to be updated in-memory, got %q", scaler.Spec.ResourceQuota)
	}

	persisted, err := scalerClient.IchpV1().QuotaAutoscalers("default").Get(context.TODO(), "qa", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected persisted QuotaAutoscaler to be readable, got error: %v", err)
	}
	if persisted.Spec.ResourceQuota != "compute-resources" {
		t.Fatalf("expected persisted spec.resourceQuota to be compute-resources, got %q", persisted.Spec.ResourceQuota)
	}
}

func TestResolveResourceQuotaFailsWhenMultipleQuotasExist(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(
		&corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "quota-a", Namespace: "default"},
		},
		&corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "quota-b", Namespace: "default"},
		},
	)
	scaler := &scalerv1.QuotaAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "qa", Namespace: "default"},
	}

	if _, err := ResolveResourceQuota(context.TODO(), kubeClient, nil, scaler); err == nil {
		t.Fatalf("expected automatic ResourceQuota resolution to fail when multiple quotas exist")
	}
}
